package server

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

const simDomain = "chat.example.com"

// simClient is a minimal client that drives the cell protocol against a
// ChatService in-process (no DNS), exercising the server end-to-end.
type simClient struct {
	id   ed25519.PrivateKey
	enc  *ecdh.PrivateKey
	addr [protocol.AddressSize]byte
}

func newSimClient(t *testing.T) simClient {
	t.Helper()
	seed, err := protocol.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	id, err := protocol.DeriveIdentityKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := protocol.DeriveEncryptionKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	return simClient{id: id, enc: enc, addr: protocol.Address(id.Public().(ed25519.PublicKey))}
}

func newChatSvc(t *testing.T) (*ChatService, [protocol.KeySize]byte, []byte) {
	t.Helper()
	store := newTestStore(t)
	ek, err := protocol.GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	qk, _, err := protocol.DeriveKeys("svc-pass")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewChatService(store, ek, qk, store.Limits(), []string{simDomain})
	return svc, qk, ek.PublicKey().Bytes()
}

// feedStream streams a handshake stream as cells; the completing cell returns
// the sealed handshake response.
func feedStream(t *testing.T, svc *ChatService, tag [protocol.ChatSelectorSize]byte, stream []byte) []byte {
	t.Helper()
	n := (len(stream) + protocol.ChatCellPayloadSize - 1) / protocol.ChatCellPayloadSize
	var resp []byte
	for i := 0; i < n; i++ {
		start := i * protocol.ChatCellPayloadSize
		end := start + protocol.ChatCellPayloadSize
		if end > len(stream) {
			end = len(stream)
		}
		chunk := make([]byte, protocol.ChatCellPayloadSize)
		copy(chunk, stream[start:end])
		resp = svc.HandleCell(tag, uint32(i), chunk, simDomain, time.Now())
	}
	return resp
}

func handshakeTag() [protocol.ChatSelectorSize]byte {
	var tag [protocol.ChatSelectorSize]byte
	_, _ = rand.Read(tag[:])
	protocol.ChatMarkHandshakeSelector(&tag)
	return tag
}

func openHandshake(t *testing.T, ks [protocol.KeySize]byte, tag [protocol.ChatSelectorSize]byte, resp []byte) [protocol.ChatSelectorSize]byte {
	t.Helper()
	st, body, err := protocol.OpenChatResponse(ks, tag, protocol.ChatBootstrapCounter(), resp)
	if err != nil || st != protocol.ChatStatusOK {
		t.Fatalf("handshake st=%d err=%v resp=%x", st, err, resp)
	}
	// body = serverUnix(4) ‖ sessionRef(3) ‖ ttl(2)
	var ref [protocol.ChatSelectorSize]byte
	copy(ref[:], body[4:4+protocol.ChatSelectorSize])
	return ref
}

// registerRecord presents a registration record and returns the server's raw
// status. The record may be anyone's -- which is the point: presenting one
// proves nothing, so this is also how the replay tests drive the server.
func registerRecord(t *testing.T, svc *ChatService, qk [protocol.KeySize]byte, ekPub []byte, rec []byte) (byte, []byte, [protocol.KeySize]byte, [protocol.ChatSelectorSize]byte) {
	t.Helper()
	eph, _ := protocol.GenerateEphemeralKey()
	ks, _ := protocol.ChatSessionKey(eph, ekPub, protocol.ChatProtocolVersion, qk)
	tag := handshakeTag()
	sealedBoot := protocol.SealChat(ks, tag[:], protocol.ChatBootstrapCounter(), rec)
	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(), protocol.ChatProtocolVersion, protocol.ChatHandshakeRegister, sealedBoot)
	st, body, err := protocol.OpenChatResponse(ks, tag, protocol.ChatBootstrapCounter(), feedStream(t, svc, tag, stream))
	if err != nil {
		t.Fatalf("handshake response did not open: %v", err)
	}
	return st, body, ks, tag
}

// registerHandshake onboards a client and returns a working session, the way
// the real client does it: Register publishes the record and hands out NO
// session, so the session comes from the Auth that follows.
func registerHandshake(t *testing.T, svc *ChatService, qk [protocol.KeySize]byte, ekPub []byte, c simClient) (ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte) {
	t.Helper()
	rec, err := protocol.EncodeRegisterEnvelope(c.id, c.enc.PublicKey().Bytes(), 1750000000)
	if err != nil {
		t.Fatal(err)
	}
	st, _, _, _ := registerRecord(t, svc, qk, ekPub, rec)
	if st != protocol.ChatStatusRegistered {
		t.Fatalf("register st=%d, want Registered (%d)", st, protocol.ChatStatusRegistered)
	}
	return authHandshake(t, svc, qk, ekPub, c)
}

func authHandshake(t *testing.T, svc *ChatService, qk [protocol.KeySize]byte, ekPub []byte, c simClient) (ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte) {
	t.Helper()
	eph, _ := protocol.GenerateEphemeralKey()
	ks, _ = protocol.ChatSessionKey(eph, ekPub, protocol.ChatProtocolVersion, qk)
	tag := handshakeTag()
	ts := uint32(time.Now().Unix()) // within the server's skew window
	kss, _ := protocol.ChatServerSharedKey(c.enc, ekPub, c.enc.PublicKey().Bytes(), ekPub)
	proof := protocol.ChatAccountProof(kss, eph.PublicKey().Bytes(), c.addr, ts, simDomain)
	boot := protocol.BuildChatAuthBootstrapPlain(c.addr, ts, proof)
	sealedBoot := protocol.SealChat(ks, tag[:], protocol.ChatBootstrapCounter(), boot)
	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(), protocol.ChatProtocolVersion, protocol.ChatHandshakeAuth, sealedBoot)
	return openHandshake(t, ks, tag, feedStream(t, svc, tag, stream)), ks
}

func chatOp(t *testing.T, svc *ChatService, ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte, ctr *uint32, plain []byte) (byte, []byte) {
	t.Helper()
	// Seal like the real client: budget 15 plus the deterministic jitter the
	// server expects to subtract (none for OP_FRAG).
	pad := protocol.ChatCellPlainSize
	if protocol.ChatPlainOp(plain) != protocol.ChatOpFrag {
		pad += protocol.ChatCellJitter(svc.queryKey, ref, *ctr)
	}
	payload, err := protocol.SealChatCellPayloadN(ks, ref, *ctr, plain, pad)
	if err != nil {
		t.Fatal(err)
	}
	resp := svc.HandleCell(ref, *ctr, payload, simDomain, time.Now())
	st, body, err := protocol.OpenChatResponse(ks, ref, *ctr, resp)
	cur := *ctr
	*ctr++
	if err != nil {
		t.Fatalf("op ctr=%d open: %v resp=%x", cur, err, resp)
	}
	return st, body
}

func TestChatRegisterHandshakeAndStatus(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	ref, ks := registerHandshake(t, svc, qk, ekPub, a)
	ctr := uint32(0)
	st, body := chatOp(t, svc, ref, ks, &ctr, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK {
		t.Fatalf("status st=%d", st)
	}
	if len(body) < 7 || body[6] != 0 {
		t.Fatalf("expected empty inbox, body=%x", body)
	}
}

func TestChatAuthHandshakeAfterRegister(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	registerHandshake(t, svc, qk, ekPub, a) // creates the account
	ref, ks := authHandshake(t, svc, qk, ekPub, a)
	ctr := uint32(0)
	if st, _ := chatOp(t, svc, ref, ks, &ctr, protocol.BuildChatStatusPlain()); st != protocol.ChatStatusOK {
		t.Fatalf("auth-session status st=%d", st)
	}
}

// okAuth attempts an auth handshake against ekPub without failing the test,
// reporting whether it yielded an OK session — used to assert a retired key no
// longer authenticates.
func okAuth(t *testing.T, svc *ChatService, qk [protocol.KeySize]byte, ekPub []byte, c simClient) bool {
	t.Helper()
	eph, _ := protocol.GenerateEphemeralKey()
	ks, _ := protocol.ChatSessionKey(eph, ekPub, protocol.ChatProtocolVersion, qk)
	tag := handshakeTag()
	ts := uint32(time.Now().Unix())
	kss, _ := protocol.ChatServerSharedKey(c.enc, ekPub, c.enc.PublicKey().Bytes(), ekPub)
	proof := protocol.ChatAccountProof(kss, eph.PublicKey().Bytes(), c.addr, ts, simDomain)
	boot := protocol.BuildChatAuthBootstrapPlain(c.addr, ts, proof)
	sealedBoot := protocol.SealChat(ks, tag[:], protocol.ChatBootstrapCounter(), boot)
	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(), protocol.ChatProtocolVersion, protocol.ChatHandshakeAuth, sealedBoot)
	resp := feedStream(t, svc, tag, stream)
	st, _, err := protocol.OpenChatResponse(ks, tag, protocol.ChatBootstrapCounter(), resp)
	return err == nil && st == protocol.ChatStatusOK
}

// TestChatEkRotation: after rotation the new key authenticates, a client still
// on the previous key authenticates during its grace window, and a key retired
// past its grace no longer completes a handshake (forward secrecy boundary).
func TestChatEkRotation(t *testing.T) {
	svc, qk, ekPub0 := newChatSvc(t)
	a := newSimClient(t)
	registerHandshake(t, svc, qk, ekPub0, a) // account created under the original key

	ekPub1, err := svc.RotateEk(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if string(ekPub1) == string(ekPub0) {
		t.Fatal("rotation did not change the key")
	}

	// Both the new current key and the graced previous key authenticate.
	for _, tc := range []struct {
		name string
		ek   []byte
	}{{"current", ekPub1}, {"graced-previous", ekPub0}} {
		ref, ks := authHandshake(t, svc, qk, tc.ek, a)
		ctr := uint32(0)
		if st, _ := chatOp(t, svc, ref, ks, &ctr, protocol.BuildChatStatusPlain()); st != protocol.ChatStatusOK {
			t.Fatalf("%s key: auth st=%d", tc.name, st)
		}
	}

	// Rotate again with zero grace → ekPub1 is retired immediately and dropped;
	// a client still pinned to it can no longer handshake.
	if _, err := svc.RotateEk(time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if okAuth(t, svc, qk, ekPub1, a) {
		t.Fatal("a key retired past its grace still completed a handshake")
	}
}

func TestChatUnknownSessionSentinel(t *testing.T) {
	svc, _, _ := newChatSvc(t)
	var ref [protocol.ChatSelectorSize]byte
	_, _ = rand.Read(ref[:])
	protocol.ChatClearHandshakeSelector(&ref) // unknown, non-handshake selector
	var ks [protocol.KeySize]byte
	payload, _ := protocol.SealChatCellPayload(ks, ref, 0, protocol.BuildChatStatusPlain())
	resp := svc.HandleCell(ref, 0, payload, simDomain, time.Now())
	if !protocol.ChatIsSessionLost(resp) {
		t.Fatalf("expected session-lost sentinel, got %x", resp)
	}
}

func TestChatSendRoundTrip(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	b := newSimClient(t)
	refA, ksA := registerHandshake(t, svc, qk, ekPub, a)
	refB, ksB := registerHandshake(t, svc, qk, ekPub, b)

	const text = "hello world سلام دوست"
	const seq = uint32(1)
	contentKey, _ := protocol.ChatContentKey(a.enc, b.enc.PublicKey().Bytes(), a.addr, b.addr, seq)
	kssA, _ := protocol.ChatServerSharedKey(a.enc, ekPub, a.enc.PublicKey().Bytes(), ekPub)
	env, err := protocol.EncodeChatMessage(contentKey, kssA, a.addr, b.addr, seq, text)
	if err != nil {
		t.Fatal(err)
	}

	// A: send-start → data chunks → fin.
	ctrA := uint32(0)
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env)))); st != protocol.ChatStatusOK {
		t.Fatalf("send-start st=%d", st)
	}
	for i, ch := range protocol.SplitChunks(env, protocol.ChatDataChunkSize) {
		d, _ := protocol.BuildChatDataPlain(uint8(i), ch)
		if st, _ := chatOp(t, svc, refA, ksA, &ctrA, d); st != protocol.ChatStatusOK {
			t.Fatalf("data %d st=%d", i, st)
		}
	}
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatFinPlain(crc32.ChecksumIEEE(env))); st != protocol.ChatStatusOK {
		t.Fatalf("fin st=%d", st)
	}

	// B: status shows 1 waiting from A.
	ctrB := uint32(0)
	st, body := chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK || body[6] != 1 {
		t.Fatalf("B status st=%d count=%d", st, body[6])
	}
	entry := body[7:]
	var src [protocol.AddressSize]byte
	copy(src[:], entry[:protocol.AddressSize])
	if src != a.addr {
		t.Fatal("inbox src mismatch")
	}
	eSeq := binary.BigEndian.Uint32(entry[protocol.AddressSize:])
	blocks := entry[protocol.AddressSize+6]

	// B: fetch + reassemble + decrypt.
	var got []byte
	for blk := uint8(0); blk < blocks; blk++ {
		st, fb := chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatFetchPlain(protocol.ChatPeerHandle(a.addr), eSeq, blk))
		if st != protocol.ChatStatusOK {
			t.Fatalf("fetch blk %d st=%d", blk, st)
		}
		got = append(got, fb...)
	}
	m, err := protocol.ParseChatMessage(got)
	if err != nil {
		t.Fatal(err)
	}
	rk, _ := protocol.ChatContentKey(b.enc, a.enc.PublicKey().Bytes(), a.addr, b.addr, m.Seq)
	gotText, err := m.Open(rk)
	if err != nil || gotText != text {
		t.Fatalf("open: %v %q", err, gotText)
	}

	// B: ack (peer by handle) → inbox freed.
	handle := protocol.ChatPeerHandle(a.addr)
	if st, _ := chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatAckPlain(handle, m.Seq, [protocol.ChatReceiptMACSize]byte{})); st != protocol.ChatStatusOK {
		t.Fatalf("ack st=%d", st)
	}
	st, body = chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK || body[6] != 0 {
		t.Fatalf("inbox not freed: count=%d", body[6])
	}
}

// sendStartBitSet reports whether chunk i is marked received in a SEND_START
// response (6 quota bytes, then the reassembler bitmap, MSB-first).
func sendStartBitSet(body []byte, i int) bool {
	bm := body[6:]
	return i/8 < len(bm) && bm[i/8]&(1<<(7-uint(i%8))) != 0
}

// TestChatResumesPartialUpload verifies that a repeat SEND_START for the same
// in-progress message RESUMES instead of discarding the chunks already received
// — so a client retrying on the same live session doesn't re-send the half it
// already delivered. (A new session resets, since sess.upload is nil there.)
func TestChatResumesPartialUpload(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	b := newSimClient(t)
	refA, ksA := registerHandshake(t, svc, qk, ekPub, a)
	registerHandshake(t, svc, qk, ekPub, b) // recipient must exist to send

	const text = "a long enough message to span several chunks — سلام دوست عزیز"
	const seq = uint32(1)
	contentKey, _ := protocol.ChatContentKey(a.enc, b.enc.PublicKey().Bytes(), a.addr, b.addr, seq)
	kssA, _ := protocol.ChatServerSharedKey(a.enc, ekPub, a.enc.PublicKey().Bytes(), ekPub)
	env, err := protocol.EncodeChatMessage(contentKey, kssA, a.addr, b.addr, seq, text)
	if err != nil {
		t.Fatal(err)
	}
	chunks := protocol.SplitChunks(env, protocol.ChatDataChunkSize)
	if len(chunks) < 2 {
		t.Fatalf("need a multi-chunk message, got %d chunk(s)", len(chunks))
	}

	ctrA := uint32(0)
	// Fresh SEND_START: chunk 0 not yet received.
	st, body := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env))))
	if st != protocol.ChatStatusOK {
		t.Fatalf("send-start st=%d", st)
	}
	if sendStartBitSet(body, 0) {
		t.Fatal("fresh send-start should not report chunk 0 as received")
	}
	// Deliver only chunk 0, then a transport blip — the client retries by
	// re-issuing SEND_START on the SAME session.
	d0, _ := protocol.BuildChatDataPlain(0, chunks[0])
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, d0); st != protocol.ChatStatusOK {
		t.Fatalf("data 0 st=%d", st)
	}
	st, body = chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env))))
	if st != protocol.ChatStatusOK {
		t.Fatalf("resume send-start st=%d", st)
	}
	if !sendStartBitSet(body, 0) {
		t.Fatal("resumed send-start must report chunk 0 already received (no re-send)")
	}

	// Finish the remaining chunks + FIN: the resumed upload still commits.
	for i := 1; i < len(chunks); i++ {
		d, _ := protocol.BuildChatDataPlain(uint8(i), chunks[i])
		if st, _ := chatOp(t, svc, refA, ksA, &ctrA, d); st != protocol.ChatStatusOK {
			t.Fatalf("data %d st=%d", i, st)
		}
	}
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatFinPlain(crc32.ChecksumIEEE(env))); st != protocol.ChatStatusOK {
		t.Fatalf("fin st=%d", st)
	}
}

// TestChatSendStartResetsDifferentMessage is the negative of resume: a
// SEND_START for a DIFFERENT message (here, a different total length) must NOT
// resume onto the in-progress upload's chunks — it starts fresh, so messages
// can never be mixed.
// TestChatResponseCacheRetransmit verifies that retransmitting the same request
// (same counter, same sealed cell) returns the identical sealed response bytes,
// even if server state changed between the two calls.
func TestChatResponseCacheRetransmit(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	b := newSimClient(t)
	refA, ksA := registerHandshake(t, svc, qk, ekPub, a)
	refB, ksB := registerHandshake(t, svc, qk, ekPub, b)

	// B: send INBOX_STATUS at counter 0, get sealed response bytes.
	ctr0 := uint32(0)
	plain := protocol.BuildChatStatusPlain()
	pad := protocol.ChatCellPlainSize + protocol.ChatCellJitter(svc.queryKey, refB, ctr0)
	payload, err := protocol.SealChatCellPayloadN(ksB, refB, ctr0, plain, pad)
	if err != nil {
		t.Fatal(err)
	}
	resp1 := svc.HandleCell(refB, ctr0, payload, simDomain, time.Now())

	// A: send a message to B — this changes B's inbox state.
	contentKey, _ := protocol.ChatContentKey(a.enc, b.enc.PublicKey().Bytes(), a.addr, b.addr, 1)
	kssA, _ := protocol.ChatServerSharedKey(a.enc, ekPub, a.enc.PublicKey().Bytes(), ekPub)
	env, _ := protocol.EncodeChatMessage(contentKey, kssA, a.addr, b.addr, 1, "hi")
	ctrA := uint32(0)
	chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env))))
	for i, ch := range protocol.SplitChunks(env, protocol.ChatDataChunkSize) {
		d, _ := protocol.BuildChatDataPlain(uint8(i), ch)
		chatOp(t, svc, refA, ksA, &ctrA, d)
	}
	chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatFinPlain(crc32.ChecksumIEEE(env)))

	// B: retransmit the exact same cell (same counter=0, same sealed bytes).
	// Server state changed (inbox now has 1 message), but the cached response
	// must be replayed, not re-sealed with different plaintext.
	resp2 := svc.HandleCell(refB, ctr0, payload, simDomain, time.Now())

	if string(resp1) != string(resp2) {
		t.Fatalf("retransmit produced different sealed response:\n  resp1=%x\n  resp2=%x", resp1, resp2)
	}

	// Sanity: a fresh counter returns the updated inbox.
	ctrB := uint32(1)
	st, body := chatOp(t, svc, refB, ksB, &ctrB, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK || body[6] != 1 {
		t.Fatalf("fresh counter should see 1 message, got count=%d", body[6])
	}
}

func TestChatSendStartResetsDifferentMessage(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	a := newSimClient(t)
	b := newSimClient(t)
	refA, ksA := registerHandshake(t, svc, qk, ekPub, a)
	registerHandshake(t, svc, qk, ekPub, b)

	const text = "a long enough message to span several chunks سلام دوست"
	contentKey, _ := protocol.ChatContentKey(a.enc, b.enc.PublicKey().Bytes(), a.addr, b.addr, 1)
	kssA, _ := protocol.ChatServerSharedKey(a.enc, ekPub, a.enc.PublicKey().Bytes(), ekPub)
	env, err := protocol.EncodeChatMessage(contentKey, kssA, a.addr, b.addr, 1, text)
	if err != nil {
		t.Fatal(err)
	}
	chunks := protocol.SplitChunks(env, protocol.ChatDataChunkSize)
	if len(chunks) < 2 {
		t.Fatalf("need a multi-chunk message, got %d", len(chunks))
	}

	ctrA := uint32(0)
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env)))); st != protocol.ChatStatusOK {
		t.Fatalf("send-start st=%d", st)
	}
	d0, _ := protocol.BuildChatDataPlain(0, chunks[0])
	if st, _ := chatOp(t, svc, refA, ksA, &ctrA, d0); st != protocol.ChatStatusOK {
		t.Fatalf("data 0 st=%d", st)
	}
	// SEND_START for a different total length → fresh upload, chunk 0 NOT carried.
	st, body := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatSendStartPlain(b.addr, uint16(len(env)+protocol.ChatDataChunkSize)))
	if st != protocol.ChatStatusOK {
		t.Fatalf("reset send-start st=%d", st)
	}
	if sendStartBitSet(body, 0) {
		t.Fatal("a different message (other length) must reset, not resume onto chunk 0")
	}
}

// A registration record is not proof of who is presenting it.
//
// The record is self-signed, static, and PUBLIC: opKeyFetch hands any
// address's record to anyone who asks. While a session came out of Register,
// replaying one minted a session owned by the victim.
//
// Register now publishes and nothing more, so the replay is INERT rather than
// merely refused -- it writes bytes identical to what is already stored.
func TestRegisterReplayGetsNoSession(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	victim := newSimClient(t)
	registerHandshake(t, svc, qk, ekPub, victim)

	// The attacker holds the victim's public record, as anyone may, and
	// replays it under an ephemeral key of their own.
	rec, err := protocol.EncodeRegisterEnvelope(victim.id, victim.enc.PublicKey().Bytes(), 1750000000)
	if err != nil {
		t.Fatal(err)
	}
	st, body, _, _ := registerRecord(t, svc, qk, ekPub, rec)

	if st == protocol.ChatStatusOK {
		t.Fatal("a replayed registration record minted a session as the victim; " +
			"their inbox can be read and acknowledged away and their account locked out")
	}
	if st != protocol.ChatStatusRegistered {
		t.Errorf("status = %d, want Registered (%d)", st, protocol.ChatStatusRegistered)
	}
	// Only the server clock, no session ref: 4 bytes and not a byte more.
	if len(body) != 4 {
		t.Errorf("the reply carries %d bytes; a session ref would be %d more",
			len(body), protocol.ChatSelectorSize+2)
	}

	// And the victim is unaffected: Auth proves possession, so they connect.
	authHandshake(t, svc, qk, ekPub, victim)
}

// The attack the previous fix did NOT stop: claiming an address on a server
// the victim has never used.
//
// Refusing Register only for addresses the server already knew looked
// sufficient, but this app is multi-server by design. On any server the
// victim had not added yet their address was unknown, so an attacker could
// register it first and be sitting there when the victim arrived -- and the
// victim's own Auth still succeeded, so nothing looked wrong to them.
//
// With no session coming out of Register, the attacker is left holding
// nothing at all.
func TestPreRegisteringSomeoneElseOnAFreshServerBuysNothing(t *testing.T) {
	// Server A, where the victim is, and where their record is public.
	svcA, qkA, ekA := newChatSvc(t)
	victim := newSimClient(t)
	registerHandshake(t, svcA, qkA, ekA, victim)
	attacker := newSimClient(t)
	refAtk, ksAtk := registerHandshake(t, svcA, qkA, ekA, attacker)
	ctrAtk := uint32(0)
	st, rec := chatOp(t, svcA, refAtk, ksAtk, &ctrAtk, protocol.BuildChatKeyFetchPlain(victim.addr))
	if st != protocol.ChatStatusOK || len(rec) == 0 {
		t.Fatalf("key fetch st=%d len=%d -- the attack needs the public record", st, len(rec))
	}

	// Server B, which the victim has never touched. The attacker gets there
	// first with those exact bytes.
	svcB, qkB, ekB := newChatSvc(t)
	hsSt, _, ksH, tag := registerRecord(t, svcB, qkB, ekB, rec)
	if hsSt == protocol.ChatStatusOK {
		t.Fatal("pre-registration minted a session for an address the attacker does not own")
	}
	if hsSt != protocol.ChatStatusRegistered {
		t.Errorf("status = %d, want Registered (%d)", hsSt, protocol.ChatStatusRegistered)
	}
	// The setup tag is not a session either: an op on it must be refused.
	var zero [protocol.ChatSelectorSize]byte
	_ = zero
	ctrH := uint32(0)
	payload, perr := protocol.SealChatCellPayloadN(ksH, tag, ctrH,
		protocol.BuildChatStatusPlain(), protocol.ChatCellPlainSize)
	if perr != nil {
		t.Fatal(perr)
	}
	resp := svcB.HandleCell(tag, ctrH, payload, simDomain, time.Now())
	if s, _, err := protocol.OpenChatResponse(ksH, tag, ctrH, resp); err == nil && s == protocol.ChatStatusOK {
		t.Error("the handshake tag works as a session; STATUS leaks who is messaging the victim")
	}

	// The victim arrives and everything is theirs: a friend's message lands
	// and is still there to be read.
	refV, ksV := authHandshake(t, svcB, qkB, ekB, victim)
	ctrV := uint32(0)
	friend := newSimClient(t)
	refF, ksF := registerHandshake(t, svcB, qkB, ekB, friend)
	ctrF := uint32(0)
	sendTo(t, svcB, ekB, refF, ksF, &ctrF, friend, victim, 1, "meet me at six")
	if n := inboxCount(t, svcB, refV, ksV, &ctrV); n != 1 {
		t.Fatalf("the victim has %d waiting, want 1 -- the pre-registered session "+
			"could still reach their inbox", n)
	}
}

// Re-presenting a record for an address the server already knows must change
// nothing that matters: this is what makes accepting the old, proofless shape
// safe rather than a hole left open for compatibility.
func TestReRegisteringAKnownAddressIsInert(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	victim := newSimClient(t)
	refV, ksV := registerHandshake(t, svc, qk, ekPub, victim)
	friend := newSimClient(t)
	refF, ksF := registerHandshake(t, svc, qk, ekPub, friend)
	ctrF := uint32(0)
	sendTo(t, svc, ekPub, refF, ksF, &ctrF, friend, victim, 1, "still here afterwards")

	ctrV := uint32(0)
	before := inboxCount(t, svc, refV, ksV, &ctrV)
	st, beforeBody := chatOp(t, svc, refV, ksV, &ctrV, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK {
		t.Fatalf("status st=%d", st)
	}
	quotaBefore := binary.BigEndian.Uint16(beforeBody[0:2])

	// An attacker re-presents the victim's own record, as an old client would.
	rec, err := protocol.EncodeRegisterEnvelope(victim.id, victim.enc.PublicKey().Bytes(), 1750000000)
	if err != nil {
		t.Fatal(err)
	}
	if st, _, _, _ := registerRecord(t, svc, qk, ekPub, rec); st != protocol.ChatStatusRegistered {
		t.Fatalf("re-register st=%d", st)
	}

	// Inbox, send quota and the victim's session all survive.
	st, afterBody := chatOp(t, svc, refV, ksV, &ctrV, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK {
		t.Fatalf("the victim's session broke: st=%d", st)
	}
	if after := int(afterBody[6]); after != before {
		t.Errorf("inbox went from %d to %d messages", before, after)
	}
	if q := binary.BigEndian.Uint16(afterBody[0:2]); q != quotaBefore {
		t.Errorf("send quota went from %d to %d", quotaBefore, q)
	}
	// And the message is still readable, not just counted.
	eSeq := binary.BigEndian.Uint32(afterBody[7+protocol.AddressSize:])
	_ = eSeq
	if _, fb := chatOp(t, svc, refV, ksV, &ctrV,
		protocol.BuildChatFetchPlain(protocol.ChatPeerHandle(friend.addr), 1, 0)); len(fb) == 0 {
		t.Error("the waiting message can no longer be fetched")
	}
}

// A RELEASED client, which expects a session out of Register, must still end
// up working. Many users have not updated.
//
// It cannot get a session from Register any more -- that is the fix -- so it
// treats the reply as a failed attempt. What has to hold is that the attempt
// was not wasted: the record is published, so the Auth it makes next
// succeeds, and it never needs Register again on this server.
//
// Measured against the real released client (worktree at the shipped commit,
// new server grafted in): one failure, once per account per server, recovered
// 1s later by the client's own re-handshake throttle -- and absorbed with no
// user-visible error by the backend's existing registration retry loop
// (chatEnableRegisterAttempts = 20, 1.5s-5s backoff, UI shows "Checking").
// Send, fetch, ack, both directions, peer lookups, a server restart and a
// reinstall with the same seed were all unaffected.
func TestAReleasedClientStillEndsUpWorking(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	old := newSimClient(t)
	rec, err := protocol.EncodeRegisterEnvelope(old.id, old.enc.PublicKey().Bytes(), 1750000000)
	if err != nil {
		t.Fatal(err)
	}

	// What the released client does, and what it concludes.
	st, _, _, _ := registerRecord(t, svc, qk, ekPub, rec)
	if st == protocol.ChatStatusOK {
		t.Fatal("Register handed out a session again")
	}

	// The attempt must not be wasted: the record IS published.
	if _, _, known, kerr := svc.store.Keys(old.addr, time.Now()); kerr != nil || !known {
		t.Fatal("Register did not publish the record, so the released client's " +
			"next Auth would fail too and it would be locked out for good")
	}

	// So its next attempt -- Auth, because the account now exists -- works,
	// and it can use the session for real work.
	refA, ksA := authHandshake(t, svc, qk, ekPub, old)
	ctrA := uint32(0)
	peer := newSimClient(t)
	registerHandshake(t, svc, qk, ekPub, peer)
	sendTo(t, svc, ekPub, refA, ksA, &ctrA, old, peer, 1, "from a client nobody updated")

	refP, ksP := authHandshake(t, svc, qk, ekPub, peer)
	ctrP := uint32(0)
	if n := inboxCount(t, svc, refP, ksP, &ctrP); n != 1 {
		t.Fatalf("the released client's message did not arrive (%d waiting)", n)
	}

	// And it never pays that cost twice: Auth works from here on.
	authHandshake(t, svc, qk, ekPub, old)
}

// Registration itself must still work for an address nobody has claimed --
// that is the whole on-boarding path.
func TestRegisterStillWorksForANewAddress(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	fresh := newSimClient(t)
	ref, _ := registerHandshake(t, svc, qk, ekPub, fresh)
	var zero [protocol.ChatSelectorSize]byte
	if ref == zero {
		t.Fatal("a brand-new address could not register")
	}
}

// sendTo uploads one real message from `from` to `to` and returns its seq.
func sendTo(t *testing.T, svc *ChatService, ekPub []byte, ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte, ctr *uint32, from, to simClient, seq uint32, text string) {
	t.Helper()
	contentKey, _ := protocol.ChatContentKey(from.enc, to.enc.PublicKey().Bytes(), from.addr, to.addr, seq)
	kss, _ := protocol.ChatServerSharedKey(from.enc, ekPub, from.enc.PublicKey().Bytes(), ekPub)
	env, err := protocol.EncodeChatMessage(contentKey, kss, from.addr, to.addr, seq, text)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := chatOp(t, svc, ref, ks, ctr, protocol.BuildChatSendStartPlain(to.addr, uint16(len(env)))); st != protocol.ChatStatusOK {
		t.Fatalf("send-start st=%d", st)
	}
	for i, ch := range protocol.SplitChunks(env, protocol.ChatDataChunkSize) {
		d, _ := protocol.BuildChatDataPlain(uint8(i), ch)
		if st, _ := chatOp(t, svc, ref, ks, ctr, d); st != protocol.ChatStatusOK {
			t.Fatalf("data %d st=%d", i, st)
		}
	}
	if st, _ := chatOp(t, svc, ref, ks, ctr, protocol.BuildChatFinPlain(crc32.ChecksumIEEE(env))); st != protocol.ChatStatusOK {
		t.Fatalf("fin st=%d", st)
	}
}

// inboxCount reports how many messages are waiting for whoever owns a session.
func inboxCount(t *testing.T, svc *ChatService, ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte, ctr *uint32) int {
	t.Helper()
	st, body := chatOp(t, svc, ref, ks, ctr, protocol.BuildChatStatusPlain())
	if st != protocol.ChatStatusOK {
		t.Fatalf("status st=%d", st)
	}
	return int(body[6])
}

// The whole attack, carried out, not argued about.
//
// The attacker needs the victim's address and nothing else: the passphrase is
// public by design, and the registration record is handed to anyone who asks
// for it over KEY_FETCH. Both are fetched here the way an attacker would get
// them rather than read out of the victim's struct.
//
// What the attack buys is this test's assertion: the message the victim has
// not read yet is acknowledged away and deleted, so they never learn it
// existed. The sender sees it accepted. Nothing is decrypted -- the attacker
// cannot read a word of it -- which is exactly why asserting on the inbox and
// not on a status code is the honest test.
//
// Delete the `known` check in completeHandshake and this fails at the last
// assertion: the inbox is empty and the message is gone.
func TestHijackedSessionCannotAcknowledgeAwayAnInbox(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)
	sender := newSimClient(t)
	victim := newSimClient(t)
	attacker := newSimClient(t)

	refS, ksS := registerHandshake(t, svc, qk, ekPub, sender)
	refV, ksV := registerHandshake(t, svc, qk, ekPub, victim)
	refA, ksA := registerHandshake(t, svc, qk, ekPub, attacker)
	ctrS, ctrV, ctrA := uint32(0), uint32(0), uint32(0)

	const secret = "the one message that must not disappear"
	sendTo(t, svc, ekPub, refS, ksS, &ctrS, sender, victim, 1, secret)

	if n := inboxCount(t, svc, refV, ksV, &ctrV); n != 1 {
		t.Fatalf("precondition: victim has %d waiting, want 1", n)
	}

	// Step 1 -- the attacker asks the server for the victim's record, from
	// their own perfectly ordinary session. This is the published key
	// directory; it is meant to answer.
	st, rec := chatOp(t, svc, refA, ksA, &ctrA, protocol.BuildChatKeyFetchPlain(victim.addr))
	if st != protocol.ChatStatusOK {
		t.Fatalf("key fetch st=%d -- the attack needs the public record", st)
	}
	if len(rec) == 0 {
		t.Fatal("key fetch returned nothing")
	}

	// Step 2 -- replay those exact bytes as the bootstrap of a new handshake.
	eph, _ := protocol.GenerateEphemeralKey()
	ksH, _ := protocol.ChatSessionKey(eph, ekPub, protocol.ChatProtocolVersion, qk)
	tag := handshakeTag()
	sealedBoot := protocol.SealChat(ksH, tag[:], protocol.ChatBootstrapCounter(), rec)
	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(),
		protocol.ChatProtocolVersion, protocol.ChatHandshakeRegister, sealedBoot)
	hsSt, hsBody, err := protocol.OpenChatResponse(ksH, tag, protocol.ChatBootstrapCounter(),
		feedStream(t, svc, tag, stream))
	if err != nil {
		t.Fatalf("handshake response did not open: %v", err)
	}

	// Step 3 -- if that minted a session, spend it: STATUS hands over the peer
	// handles and sequence numbers for free, and ACK takes a receipt the server
	// never checks (it only relays it to the sender).
	if hsSt == protocol.ChatStatusOK {
		var refH [protocol.ChatSelectorSize]byte
		copy(refH[:], hsBody[4:4+protocol.ChatSelectorSize])
		ctrH := uint32(0)

		st, body := chatOp(t, svc, refH, ksH, &ctrH, protocol.BuildChatStatusPlain())
		if st != protocol.ChatStatusOK {
			t.Fatalf("hijacked status st=%d", st)
		}
		for i := 0; i < int(body[6]); i++ {
			e := body[7+i*(protocol.AddressSize+7):]
			var src [protocol.AddressSize]byte
			copy(src[:], e[:protocol.AddressSize])
			seq := binary.BigEndian.Uint32(e[protocol.AddressSize:])
			chatOp(t, svc, refH, ksH, &ctrH,
				protocol.BuildChatAckPlain(protocol.ChatPeerHandle(src), seq,
					[protocol.ChatReceiptMACSize]byte{}))
		}
	}

	// The only thing that matters: is the victim's message still there?
	if n := inboxCount(t, svc, refV, ksV, &ctrV); n != 1 {
		t.Fatalf("the victim's unread message is gone (%d waiting): a replayed "+
			"registration record minted a session owned by them, and an inbox "+
			"that session can acknowledge is an inbox anyone holding their "+
			"address can empty", n)
	}
	if hsSt != protocol.ChatStatusRegistered {
		t.Errorf("replayed register status = %d, want Registered (%d)", hsSt, protocol.ChatStatusRegistered)
	}
}

// The user's own case, which the fix must not break: the same seed carried to
// a new device, talking to a server that already knows the account.
//
// Register is refused there now, so everything depends on Auth working from
// nothing but the seed -- no local state, no stored session, no record of
// having registered. That is what a reinstall is.
func TestSameSeedOnANewDeviceStillConnects(t *testing.T) {
	svc, qk, ekPub := newChatSvc(t)

	seed, err := protocol.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	fromSeed := func() simClient {
		id, err := protocol.DeriveIdentityKey(seed)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := protocol.DeriveEncryptionKey(seed)
		if err != nil {
			t.Fatal(err)
		}
		return simClient{id: id, enc: enc, addr: protocol.Address(id.Public().(ed25519.PublicKey))}
	}

	oldPhone := fromSeed()
	registerHandshake(t, svc, qk, ekPub, oldPhone)

	// A message arrives while the old phone is gone.
	sender := newSimClient(t)
	refS, ksS := registerHandshake(t, svc, qk, ekPub, sender)
	ctrS := uint32(0)
	sendTo(t, svc, ekPub, refS, ksS, &ctrS, sender, oldPhone, 1, "waiting for you")

	// The new device: same seed, no other state. It must Auth, and Auth must
	// work -- and the waiting message must be readable, not acknowledged away.
	newPhone := fromSeed()
	if newPhone.addr != oldPhone.addr {
		t.Fatal("the same seed gave a different address")
	}
	refN, ksN := authHandshake(t, svc, qk, ekPub, newPhone)
	ctrN := uint32(0)
	if n := inboxCount(t, svc, refN, ksN, &ctrN); n != 1 {
		t.Fatalf("the new device sees %d waiting messages, want 1", n)
	}

	// And a Register from it is refused -- which is the point, and is why the
	// client tries Auth first and only falls back on UnknownSender.
	eph, _ := protocol.GenerateEphemeralKey()
	ksR, _ := protocol.ChatSessionKey(eph, ekPub, protocol.ChatProtocolVersion, qk)
	tag := handshakeTag()
	recBytes, err := protocol.EncodeRegisterEnvelope(newPhone.id, newPhone.enc.PublicKey().Bytes(), 1750000000)
	if err != nil {
		t.Fatal(err)
	}
	sealedBoot := protocol.SealChat(ksR, tag[:], protocol.ChatBootstrapCounter(), recBytes)
	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(),
		protocol.ChatProtocolVersion, protocol.ChatHandshakeRegister, sealedBoot)
	st, _, err := protocol.OpenChatResponse(ksR, tag, protocol.ChatBootstrapCounter(),
		feedStream(t, svc, tag, stream))
	if err != nil {
		t.Fatal(err)
	}
	if st == protocol.ChatStatusOK {
		t.Error("a known address can still register, so the replay is still open")
	}
}

// A handshake cell proves nothing about who sent it, so what one tag can make
// the server hold has to be bounded by what a real handshake could use.
//
// The cell index is a byte: one query carrying index 255 used to grow the
// slice to 256 entries -- 6 KiB of headers for a ~50-byte query -- and the
// tag table holds 50 000 of them, so ~300 MiB from 50 000 queries that cost
// the sender nothing, held until a sweep that runs every few minutes.
func TestHandshakeMemoryIsBoundedPerTag(t *testing.T) {
	svc, _, _ := newChatSvc(t)
	now := time.Now()

	tag := handshakeTag()
	payload := make([]byte, protocol.ChatCellPayloadSize)

	// Every index a byte can hold, including the ones no handshake can use.
	for i := 0; i < 256; i++ {
		svc.HandleCell(tag, uint32(i), payload, simDomain, now)
	}

	svc.mu.Lock()
	h := svc.handshakes[tag]
	held := 0
	if h != nil {
		held = len(h.chunks)
	}
	svc.mu.Unlock()

	if held > chatMaxHandshakeCells {
		t.Errorf("one tag holds %d cells; a real handshake needs at most %d, "+
			"so the rest is memory an unauthenticated sender chose to make us keep",
			held, chatMaxHandshakeCells)
	}
}

// A stale entry must not keep collecting cells until the next sweep.
func TestHandshakeEntryExpiresOnInsertNotOnlyOnSweep(t *testing.T) {
	svc, _, _ := newChatSvc(t)
	start := time.Now()
	tag := handshakeTag()
	payload := make([]byte, protocol.ChatCellPayloadSize)

	svc.HandleCell(tag, 0, payload, simDomain, start)
	svc.mu.Lock()
	first := svc.handshakes[tag]
	svc.mu.Unlock()
	if first == nil {
		t.Fatal("the first cell created no entry")
	}

	// Well past the TTL, but long before a sweep would run.
	later := start.Add(chatHandshakeTTL + time.Second)
	svc.HandleCell(tag, 1, payload, simDomain, later)

	svc.mu.Lock()
	second := svc.handshakes[tag]
	svc.mu.Unlock()
	if second == nil {
		t.Fatal("no entry after the second cell")
	}
	if second == first {
		t.Error("a stale handshake went on collecting cells; an attacker who " +
			"knows the sweep interval just works inside it")
	}
}
