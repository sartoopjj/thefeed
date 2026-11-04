package server

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// junkSession completes a handshake in one query and then says nothing, as a
// flood would.
func junkSession(t *testing.T, r *MTProtoRelay) {
	t.Helper()
	eph, err := protocol.GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	var tag [protocol.ChatSelectorSize]byte
	rand.Read(tag[:])
	protocol.ChatMarkHandshakeSelector(&tag)
	r.HandleCell(tag, 0, append(eph.PublicKey().Bytes(), make([]byte, 8)...), "m.test", time.Now())
}

// A flood cannot grow the table past its size, and a real user still gets in
// and gets through.
func TestSessionTableStaysBounded(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.MaxSessions = 50 })
	for i := 0; i < 300; i++ {
		junkSession(t, r)
	}
	if n, _ := r.Stats(); n > 50 {
		t.Fatalf("%d sessions, limit 50", n)
	}
	c, st := tryHandshake(t, r)
	if st != protocol.MTPStatusOK {
		t.Fatalf("a real user was refused (%d)", st)
	}
	c.mustOpen()
	c.data(0, 0, []byte("through"))
	dc.waitGot(t, 7)
}

// Wave after wave of flood, a user who keeps using the relay keeps their
// session and tunnel.
func TestFloodsDoNotPushOutAnActiveUser(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.MaxSessions = 50 })
	c := newTestClient(t, r)
	c.mustOpen()
	sent := 0
	for wave := 0; wave < 5; wave++ {
		for i := 0; i < 200; i++ {
			junkSession(t, r)
			if i%10 == 9 {
				if rep := c.poll(0); rep == nil {
					t.Fatalf("wave %d: the user's session was dropped", wave)
				}
			}
		}
		msg := []byte{byte('a' + wave)}
		c.data(uint16(sent), 0, msg)
		sent++
		dc.waitGot(t, sent)
	}
	if r.sessions[c.ref] == nil || r.sessions[c.ref].currentTunnel() == nil {
		t.Fatal("the active user lost their session or tunnel")
	}
}

// What sessions keep for retries stays within the relay-wide budget, and goes
// back when the sessions go.
func TestReplyBudgetIsSharedAndGivenBack(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.ReplyBudget = 8 << 10 })
	var clients []*testClient
	for i := 0; i < 4; i++ {
		c := newTestClient(t, r)
		c.mustOpen()
		clients = append(clients, c)
	}
	for i := 0; i < 200; i++ {
		for _, c := range clients {
			c.poll(0)
		}
		if used := r.replyBytes.Load(); used > 8<<10 {
			t.Fatalf("kept %d bytes, budget %d", used, 8<<10)
		}
	}
	if r.replyBytes.Load() == 0 {
		t.Fatal("nothing kept at all")
	}
	r.Sweep(time.Now().Add(time.Hour))
	if used, tun := r.replyBytes.Load(), r.tunnels.Load(); used != 0 || tun != 0 {
		t.Fatalf("after the sweep: %d bytes kept, %d tunnels counted", used, tun)
	}
}

// No two live sessions ever share a reference.
func TestSessionRefsAreUnique(t *testing.T) {
	r := newTestRelay(t, newFakeDC(t), nil)
	refs := map[[protocol.ChatSelectorSize]byte]bool{}
	for i := 0; i < 10000; i++ {
		c, st := tryHandshake(t, r)
		if st != protocol.MTPStatusOK {
			continue
		}
		if refs[c.ref] {
			t.Fatalf("handshake %d was given a reference already in use", i)
		}
		refs[c.ref] = true
	}
	if n, _ := r.Stats(); n != len(refs) {
		t.Fatalf("%d granted, %d in the table", len(refs), n)
	}
}

// The tunnel count follows every way a tunnel opens and closes.
func TestTunnelCountFollowsOpenAndClose(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	a, b := newTestClient(t, r), newTestClient(t, r)
	want := func(n int64, what string) {
		t.Helper()
		if got := r.tunnels.Load(); got != n {
			t.Fatalf("%s: %d tunnels counted, want %d", what, got, n)
		}
	}
	a.mustOpen()
	b.mustOpen()
	want(2, "two opened")
	a.mustOpen()
	want(2, "one re-opened")
	a.op(protocol.BuildMTPClose())
	want(1, "one closed")
	a.op(protocol.BuildMTPClose())
	want(1, "closed again")
	r.Sweep(time.Now().Add(time.Hour))
	want(0, "all swept")
}

// Two clients that draw the same handshake tag each get their own session.
func TestSameTagDifferentClients(t *testing.T) {
	r := newTestRelay(t, newFakeDC(t), nil)
	var tag [protocol.ChatSelectorSize]byte
	rand.Read(tag[:])
	protocol.ChatMarkHandshakeSelector(&tag)
	hello := func() ([protocol.ChatSelectorSize]byte, byte) {
		eph, _ := protocol.GenerateEphemeralKey()
		ks, _ := protocol.MTPSessionKey(eph, r.cfg.Ek.PublicKey().Bytes(), protocol.MTProtoVersion, r.cfg.QueryKey)
		frags, _ := protocol.SplitMTPHello(eph.PublicKey().Bytes(), protocol.ChatCellPayloadSize, rand.Reader)
		var last []byte
		for i, f := range frags {
			last = r.HandleCell(tag, uint32(i), f, "m.test", time.Now())
		}
		_, body, err := protocol.OpenChatResponse(ks, tag, protocol.ChatBootstrapCounter(), last)
		if err != nil {
			t.Fatalf("this client's own reply did not open: %v", err)
		}
		st, ref, _, _ := protocol.ParseMTPHelloAck(body)
		return ref, st
	}
	a, sa := hello()
	b, sb := hello()
	if sa != protocol.MTPStatusOK || sb != protocol.MTPStatusOK || a == b {
		t.Fatalf("statuses %d/%d, refs equal: %v", sa, sb, a == b)
	}
}

// The per-session share is the configured one.
func TestReplySessionShareIsConfigurable(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.ReplySessionBytes = 4 << 10 })
	c := newTestClient(t, r)
	c.mustOpen()
	for i := 0; i < 500; i++ {
		c.poll(0)
	}
	sess := r.sessions[c.ref]
	sess.tmu.Lock()
	kept := sess.replies.bytes
	sess.tmu.Unlock()
	if kept > 4<<10 || kept == 0 {
		t.Fatalf("session keeps %d bytes, share %d", kept, 4<<10)
	}
}

// With the defaults, a repeat of a recent cell that is not the latest still
// gets its reply back.
func TestDefaultsKeepRecentReplies(t *testing.T) {
	r := newTestRelay(t, newFakeDC(t), nil)
	c := newTestClient(t, r)
	c.mustOpen()
	ctr, cell := c.sealed(protocol.BuildMTPPoll(0))
	first := c.deliver(ctr, cell)
	for i := 0; i < 20; i++ {
		c.poll(0)
	}
	if again := c.deliver(ctr, cell); len(first) == 0 || string(again) != string(first) {
		t.Fatal("a recent reply was not kept")
	}
}

// At the cap a newcomer finds a tunnel to take even when most sessions have
// none, and the cap holds.
func TestTunnelCapAmongIdleSessions(t *testing.T) {
	r := newTestRelay(t, newFakeDC(t), func(c *MTProtoConfig) { c.MaxTunnels = 3 })
	for i := 0; i < 3; i++ {
		newTestClient(t, r).mustOpen()
	}
	for i := 0; i < 500; i++ {
		junkSession(t, r)
	}
	for i := 0; i < 20; i++ {
		if st := newTestClient(t, r).openTunnel(2); st != protocol.MTPStatusOK {
			t.Fatalf("open %d: status %d at the cap", i, st)
		}
		if n := r.tunnels.Load(); n > 3 {
			t.Fatalf("%d tunnels open, cap 3", n)
		}
	}
}

// A tunnel that ended on its own is not a place: taking it frees nothing, and
// opening again on its session must still count against the cap.
func TestBrokenTunnelIsNotAPlace(t *testing.T) {
	r := newTestRelay(t, newFakeDC(t), func(c *MTProtoConfig) { c.MaxTunnels = 1 })
	a := newTestClient(t, r)
	a.mustOpen()
	chunk := make([]byte, protocol.MTPChunkSize(protocol.ChatCellPlainMax))
	for off := 0; r.tunnels.Load() != 0; off += len(chunk) {
		if off > mtpMaxPending+len(chunk) {
			t.Fatal("the tunnel never ended")
		}
		a.dataPart(uint16(off), 0, chunk)
	}
	newTestClient(t, r).mustOpen()
	a.mustOpen()
	newTestClient(t, r).mustOpen()
	if n := r.tunnels.Load(); n != 1 {
		t.Fatalf("%d tunnels open, cap 1", n)
	}
}
