package server

import (
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// sealOp seals one op plaintext at an exact counter, the way the client does
// (budget plus the deterministic jitter the server subtracts back off).
func sealOp(t *testing.T, svc *ChatService, ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte, ctr uint32, plain []byte) []byte {
	t.Helper()
	pad := protocol.ChatCellPlainSize
	if protocol.ChatPlainOp(plain) != protocol.ChatOpFrag {
		pad += protocol.ChatCellJitter(svc.queryKey, ref, ctr)
	}
	payload, err := protocol.SealChatCellPayloadN(ks, ref, ctr, plain, pad)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// One nonce must seal at most one plaintext.
//
// The response seal is AES-CTR with a nonce fixed by (session key, selector,
// counter), so a repeat of request N must come back byte-identical or not at
// all. The response cache is what normally guarantees it, and it holds
// chatRespCacheSize entries -- so this drives a counter out of the cache and
// then repeats it, which is the case the cache alone cannot cover.
func TestReplayAfterCacheEvictionCannotReuseANonce(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	victim := newSimClient(t)
	refV, ksV := registerHandshake(t, svc, qk, ekPub, victim)
	friend := newSimClient(t)
	refF, ksF := registerHandshake(t, svc, qk, ekPub, friend)

	// The counter this test will repeat: a STATUS at 0.
	const target = uint32(0)
	req := sealOp(t, svc, refV, ksV, target, protocol.BuildChatStatusPlain())
	resp1 := svc.HandleCell(refV, target, req, simDomain, time.Now())
	if len(resp1) == 0 {
		t.Fatal("no first response")
	}

	// Replaying it right away is safe: the cache returns the same bytes.
	again := svc.HandleCell(refV, target, req, simDomain, time.Now())
	if string(again) != string(resp1) {
		t.Fatal("an immediate repeat did not replay the cached bytes")
	}

	// Ordinary traffic pushes counter 0 out of the ring...
	ctr := uint32(1)
	for i := 0; i < chatRespCacheSize+2; i++ {
		svc.HandleCell(refV, ctr, sealOp(t, svc, refV, ksV, ctr, protocol.BuildChatStatusPlain()), simDomain, time.Now())
		ctr++
	}

	// ...and the state the response reports changes in between, so a fresh
	// response would not match the first.
	ctrF := uint32(0)
	sendTo(t, svc, ekPub, refF, ksF, &ctrF, friend, victim, 1, "a message arrives")

	// Repeat the evicted counter. Identical bytes or nothing; never a second,
	// different sealed response.
	resp2 := svc.HandleCell(refV, target, req, simDomain, time.Now())

	if len(resp2) == 0 {
		t.Log("the repeat was dropped, so no second response exists")
	} else if string(resp1) == string(resp2) {
		t.Log("the repeat returned the identical bytes")
	} else {
		t.Errorf("a second, DIFFERENT response was sealed at counter %d, where "+
			"one was already sealed; the invariant is one plaintext per nonce\n"+
			"  first:  %x\n  second: %x", target, resp1, resp2)
	}

	// And the victim's own session must still work -- the guard must not have
	// broken the live connection it is protecting.
	ctrV := ctr
	st, body := chatOp(t, svc, refV, ksV, &ctrV, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK {
		t.Fatalf("the victim's session broke after the guard fired: st=%d", st)
	}
	if body[6] != 1 {
		t.Errorf("the victim sees %d waiting, want 1", body[6])
	}
}

// Out-of-order arrival must NOT be mistaken for a replay.
//
// Chat cells go over UDP, scattered across resolvers, so copies can arrive in
// any order. The guard keys off "already answered", not a time window: a
// counter the session has not reached yet is served normally however late the
// ones around it are.
func TestOutOfOrderCountersAreNotTreatedAsReplays(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	ref, ks := registerHandshake(t, svc, qk, ekPub, a)

	// Arrive badly out of order: a high counter first, then lower ones that
	// were never answered.
	order := []uint32{7, 2, 5, 0, 9, 1, 3, 8, 4, 6}
	for _, ctr := range order {
		req := sealOp(t, svc, ref, ks, ctr, protocol.BuildChatStatusPlain())
		resp := svc.HandleCell(ref, ctr, req, simDomain, time.Now())
		if len(resp) == 0 {
			t.Fatalf("counter %d was dropped, but it had never been answered -- "+
				"out-of-order delivery is normal and must still be served", ctr)
		}
		st, _, err := protocol.OpenChatResponse(ks, ref, ctr, resp)
		if err != nil || st != protocol.ChatStatusOK {
			t.Fatalf("counter %d: st=%d err=%v", ctr, st, err)
		}
	}

	// Every one of those is now answered, so a repeat of any is either the
	// cached bytes or nothing -- never a fresh seal.
	for _, ctr := range order {
		req := sealOp(t, svc, ref, ks, ctr, protocol.BuildChatStatusPlain())
		first := svc.HandleCell(ref, ctr, req, simDomain, time.Now())
		second := svc.HandleCell(ref, ctr, req, simDomain, time.Now())
		if len(second) != 0 && string(first) != string(second) {
			t.Errorf("counter %d gave two different sealed responses", ctr)
		}
	}
}

// The window's edges.
//
// Inside it the session knows exactly which counters it has answered, so
// out-of-order arrival is served. At or beyond it that knowledge is gone, so
// a counter is refused rather than answered a second time.
//
// Measured against the released client over a bad link (28% of cells
// swallowed, 300-1500ms of delay, a quarter of them delivered a second time
// up to 26s later): the worst a legitimate cell ever arrived behind the
// newest counter was 8, and nothing was refused. That bound is structural
// rather than lucky -- the client consumes one counter and awaits its answer
// before the next, every chunk of an upload included, so the gap is bounded
// by one op sequence and not by how bad the link is. chatReplayWindow is 64,
// eight times that.
func TestReplayWindowEdges(t *testing.T) {
	var s chatSession

	// A fresh session accepts anything as a first sighting.
	if s.markCounter(100) {
		t.Fatal("the first counter was called a repeat")
	}
	// The same one again is a repeat.
	if !s.markCounter(100) {
		t.Error("an exact repeat was not caught")
	}

	// Out of order, but inside the window: never seen, so served.
	for _, c := range []uint32{98, 95, 100 - (chatReplayWindow - 1)} {
		if s.markCounter(c) {
			t.Errorf("counter %d (%d behind) was refused, but had never been answered",
				c, 100-c)
		}
		if !s.markCounter(c) {
			t.Errorf("counter %d was not remembered, so a repeat would be resealed", c)
		}
	}

	// Exactly at the window, and beyond it: no longer knowable, so refused.
	for _, back := range []uint32{chatReplayWindow, chatReplayWindow + 1, 100} {
		if back > 100 {
			continue
		}
		if !s.markCounter(100 - back) {
			t.Errorf("counter %d behind was served; the window is %d and beyond it "+
				"the session cannot tell a repeat from a new cell", back, chatReplayWindow)
		}
	}

	// Moving forward slides the window and forgets what fell off the back.
	var s2 chatSession
	s2.markCounter(0)
	s2.markCounter(chatReplayWindow * 3) // a big jump, e.g. after a long gap
	if !s2.markCounter(0) {
		t.Error("a counter left far behind by a jump was served again")
	}
	if s2.markCounter(chatReplayWindow*3) != true {
		t.Error("the newest counter was not remembered across the jump")
	}

	// A jump wider than the window must not wrap the mask round and resurrect
	// stale bits as "already seen".
	var s3 chatSession
	s3.markCounter(5)
	s3.markCounter(5 + chatReplayWindow*2)
	if s3.markCounter(5 + chatReplayWindow*2 - 1) {
		t.Error("a never-seen counter just inside the window after a wide jump was refused")
	}
}
