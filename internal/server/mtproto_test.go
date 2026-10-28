package server

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// fakeDC stands in for a Telegram data centre: it records what the relay
// forwards and can push bytes back.
type fakeDC struct {
	ln net.Listener

	mu   sync.Mutex
	got  []byte
	conn net.Conn
	cond *sync.Cond
}

func newFakeDC(t *testing.T) *fakeDC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDC{ln: ln}
	d.cond = sync.NewCond(&d.mu)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conn = c
		d.cond.Broadcast()
		d.mu.Unlock()
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				d.mu.Lock()
				d.got = append(d.got, buf[:n]...)
				d.cond.Broadcast()
				d.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return d
}

func (d *fakeDC) addr() string { return d.ln.Addr().String() }

// connected reports whether the relay has dialled yet.
func (d *fakeDC) connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn != nil
}

// waitGot blocks until the DC has received n bytes.
func (d *fakeDC) waitGot(t *testing.T, n int) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.got) < n {
		if time.Now().After(deadline) {
			t.Fatalf("DC got %d bytes, want %d", len(d.got), n)
		}
		d.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		d.mu.Lock()
	}
	return append([]byte(nil), d.got...)
}

func (d *fakeDC) push(t *testing.T, b []byte) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	d.mu.Lock()
	for d.conn == nil {
		if time.Now().After(deadline) {
			d.mu.Unlock()
			t.Fatal("relay never connected to the DC")
		}
		d.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		d.mu.Lock()
	}
	c := d.conn
	d.mu.Unlock()
	if _, err := c.Write(b); err != nil {
		t.Fatalf("DC write: %v", err)
	}
}

func (d *fakeDC) hangUp() {
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// newTestRelay builds a relay whose only known "DC" is the fake one, so the
// allowlist is exercised exactly as in production.
func newTestRelay(t *testing.T, dc *fakeDC, tune func(*MTProtoConfig)) *MTProtoRelay {
	t.Helper()
	ek, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := MTProtoConfig{Enabled: true, IdleTTL: time.Minute, Ek: ek}
	cfg.QueryKey[0] = 0x5A
	if tune != nil {
		tune(&cfg)
	}
	r := NewMTProtoRelay(cfg)
	if r == nil {
		t.Fatal("relay is nil")
	}
	r.addr = map[uint8]string{2: dc.addr()}
	t.Cleanup(r.closeAll)
	return r
}

// testClient drives the relay the way the real client does: an eph↔ek
// handshake, then sealed cells on the server-assigned reference.
type testClient struct {
	t   *testing.T
	r   *MTProtoRelay
	ks  [protocol.KeySize]byte
	ref [protocol.ChatSelectorSize]byte
	ctr uint32
}

const testBudget = protocol.ChatCellPlainMax

func newTestClient(t *testing.T, r *MTProtoRelay) *testClient {
	t.Helper()
	c, status := tryHandshake(t, r)
	if status != protocol.MTPStatusOK {
		t.Fatalf("handshake status = %d", status)
	}
	return c
}

// tryHandshake runs a handshake and reports the ack status, so capacity and
// refusal paths can be asserted without failing the test.
func tryHandshake(t *testing.T, r *MTProtoRelay) (*testClient, byte) {
	t.Helper()
	eph, err := protocol.GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	ks, err := protocol.MTPSessionKey(eph, r.cfg.Ek.PublicKey().Bytes(), protocol.MTProtoVersion, r.cfg.QueryKey)
	if err != nil {
		t.Fatal(err)
	}
	var tag [protocol.ChatSelectorSize]byte
	if _, err := rand.Read(tag[:]); err != nil {
		t.Fatal(err)
	}
	protocol.ChatMarkHandshakeSelector(&tag)

	frags, err := protocol.SplitMTPHello(eph.PublicKey().Bytes(), protocol.ChatCellPayloadSize, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var last []byte
	for i, f := range frags {
		last = r.HandleCell(tag, uint32(i), f, "m.test", time.Now())
	}
	_, body, err := protocol.OpenChatResponse(ks, tag, protocol.ChatBootstrapCounter(), last)
	if err != nil {
		t.Fatalf("handshake reply did not open: %v", err)
	}
	status, ref, _, err := protocol.ParseMTPHelloAck(body)
	if err != nil {
		t.Fatalf("handshake ack: %v", err)
	}
	return &testClient{t: t, r: r, ks: ks, ref: ref}, status
}

// op sends one sealed cell and returns the reply body.
func (c *testClient) op(plaintext []byte) []byte {
	c.t.Helper()
	ctr := c.ctr
	c.ctr++
	jitter := protocol.ChatCellJitter(c.r.cfg.QueryKey, c.ref, ctr)
	padded := protocol.MTPCellPad(plaintext, testBudget, jitter)
	payload, err := protocol.SealChatCellPayloadN(c.ks, c.ref, ctr, plaintext, padded)
	if err != nil {
		c.t.Fatal(err)
	}
	raw := c.r.HandleCell(c.ref, ctr, payload, "m.test", time.Now())
	if protocol.ChatIsSessionLost(raw) {
		return nil
	}
	_, body, err := protocol.OpenChatResponse(c.ks, c.ref, ctr, raw)
	if err != nil {
		c.t.Fatalf("reply did not open: %v", err)
	}
	return body
}

func (c *testClient) openTunnel(dc uint8) byte {
	c.t.Helper()
	status, _, err := protocol.ParseMTPOpenReply(c.op(protocol.BuildMTPOpen(dc)))
	if err != nil {
		c.t.Fatalf("open reply: %v", err)
	}
	return status
}

func (c *testClient) mustOpen() {
	c.t.Helper()
	if st := c.openTunnel(2); st != protocol.MTPStatusOK {
		c.t.Fatalf("open status = %d", st)
	}
}

// data sends a COMPLETE message: the relay buffers plain DATA chunks and only
// writes to the far end when the client marks the end, so a test that expects
// bytes to arrive has to say so.
func (c *testClient) data(upSeq, downAck uint16, chunk []byte) *protocol.MTPReply {
	c.t.Helper()
	rep, err := protocol.ParseMTPReply(c.op(protocol.BuildMTPDataEnd(upSeq, downAck, chunk)))
	if err != nil {
		c.t.Fatalf("reply: %v", err)
	}
	return rep
}

// dataPart sends a chunk that does NOT complete a message; it should be held.
func (c *testClient) dataPart(upSeq, downAck uint16, chunk []byte) *protocol.MTPReply {
	c.t.Helper()
	rep, err := protocol.ParseMTPReply(c.op(protocol.BuildMTPData(upSeq, downAck, chunk)))
	if err != nil {
		c.t.Fatalf("reply: %v", err)
	}
	return rep
}

func (c *testClient) poll(downAck uint16) *protocol.MTPReply {
	c.t.Helper()
	rep, err := protocol.ParseMTPReply(c.op(protocol.BuildMTPPoll(downAck)))
	if err != nil {
		c.t.Fatalf("reply: %v", err)
	}
	return rep
}

// pollUntilOffer polls WITHOUT acknowledging until the relay offers downstream
// bytes, and returns how many. Pushed bytes cross the tunnel's reader
// goroutine first, so a fixed number of polls after a push can all land early.
func (c *testClient) pollUntilOffer(downAck uint16) int {
	c.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if rep := c.poll(downAck); len(rep.Down) > 0 {
			return len(rep.Down)
		}
		if time.Now().After(deadline) {
			c.t.Fatal("the relay never offered the pushed payload")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRelayForwardsUpstreamToDC(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	msg := []byte("mtproto bytes")
	rep := c.data(0, 0, msg)
	if rep.Status != protocol.MTPStatusOK || rep.UpAck != uint16(len(msg)) {
		t.Fatalf("reply %+v", rep)
	}
	if got := dc.waitGot(t, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("DC got %q", got)
	}
}

// Telegram closes a connection that says nothing within about 1.6 seconds,
// while a first message takes tens of seconds to arrive one DNS cell at a
// time. Opening the tunnel must therefore NOT dial: a connection made here is
// always dead by the time the client's first byte is ready.
func TestRelayDefersDialUntilFirstMessage(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	// Long enough that a dial at open would certainly have landed.
	time.Sleep(50 * time.Millisecond)
	if dc.connected() {
		t.Fatal("relay dialled the DC at open; Telegram would hang up before the first cell arrives")
	}

	// A fragment is not a message: still nothing to send, so still no dial.
	c.dataPart(0, 0, []byte("frag"))
	time.Sleep(50 * time.Millisecond)
	if dc.connected() {
		t.Fatal("relay dialled the DC on a partial message")
	}

	// Completing the message is what opens the connection, and both halves
	// must arrive -- the held fragment first.
	c.data(4, 0, []byte("ment"))
	if got := dc.waitGot(t, 8); string(got) != "fragment" {
		t.Fatalf("DC got %q, want %q", got, "fragment")
	}
}

// Once a connection is live Telegram only minds silence, and a single message
// can take longer than its ~30s inactivity limit to arrive cell by cell. So
// after the first whole message, contiguous bytes must go out as they arrive
// instead of being held to the end of the message.
func TestRelayStreamsAfterFirstMessage(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	c.data(0, 0, []byte("first")) // a whole message: dials and writes
	dc.waitGot(t, 5)

	// A fragment of the NEXT message, with no end marker in sight.
	c.dataPart(5, 0, []byte("part"))
	if got := dc.waitGot(t, 9); string(got) != "firstpart" {
		t.Fatalf("DC got %q, want %q", got, "firstpart")
	}
}

// waitDown polls until downstream bytes arrive.
func waitDown(t *testing.T, c *testClient, ack uint16) *protocol.MTPReply {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rep := c.poll(ack)
		if len(rep.Down) > 0 {
			return rep
		}
		if time.Now().After(deadline) {
			t.Fatal("downstream never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRelayReturnsDownstream(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()
	c.data(0, 0, []byte("hi")) // make the relay connect

	dc.push(t, []byte("from telegram"))
	rep := waitDown(t, c, 0)
	if string(rep.Down) != "from telegram" || rep.DownSeq != 0 {
		t.Fatalf("down=%q seq=%d", rep.Down, rep.DownSeq)
	}
}

// A replayed DATA cell (its reply was lost, the client retried) must not write
// the same bytes to Telegram twice.
func TestRelayIgnoresReplayedUpstream(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	msg := []byte("once")
	first := c.data(0, 0, msg)
	again := c.data(0, 0, msg) // same offset: a replay
	if first.UpAck != again.UpAck {
		t.Fatalf("ack moved on replay: %d then %d", first.UpAck, again.UpAck)
	}
	c.data(uint16(len(msg)), 0, []byte("!"))
	if got := dc.waitGot(t, len(msg)+1); string(got) != "once!" {
		t.Fatalf("DC got %q, want once!", got)
	}
}

// Acknowledged downstream is dropped; unacknowledged is re-offered, so a lost
// reply costs a round trip and never a byte.
func TestRelayRedeliversUnackedDownstream(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()
	c.data(0, 0, []byte("x"))
	dc.push(t, []byte("abcdef"))

	first := waitDown(t, c, 0)
	if again := c.poll(0); !bytes.Equal(again.Down, first.Down) {
		t.Fatalf("re-offer = %q, want %q", again.Down, first.Down)
	}
	if acked := c.poll(uint16(len(first.Down))); len(acked.Down) != 0 {
		t.Fatalf("acked bytes came back: %q", acked.Down)
	}
}

func TestRelayBadDCIsRefused(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	if st := c.openTunnel(7); st != protocol.MTPStatusBadDC {
		t.Fatalf("status = %d, want BadDC", st)
	}
}

// A cell on an unknown reference must get the session-lost sentinel, never a
// reply that leaks whether the reference exists.
func TestRelayUnknownSessionIsRejected(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	var ref [protocol.ChatSelectorSize]byte
	ref[0] = 9
	got := r.HandleCell(ref, 1, []byte("whatever"), "m.test", time.Now())
	if !protocol.ChatIsSessionLost(got) {
		t.Fatalf("got %v, want the session-lost sentinel", got)
	}
}

// The point of the handshake: knowing a live reference is not enough. Without
// the session key the seal does not open, so another holder of the same config
// cannot drive a stranger's tunnel.
func TestRelayRejectsCellsSealedWithTheWrongKey(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	victim := newTestClient(t, r)
	victim.mustOpen()

	attacker := newTestClient(t, r) // its own session, its own key
	var wrong [protocol.KeySize]byte
	copy(wrong[:], attacker.ks[:])

	payload, err := protocol.SealChatCellPayloadN(wrong, victim.ref, 99, protocol.BuildMTPPoll(0), testBudget)
	if err != nil {
		t.Fatal(err)
	}
	got := r.HandleCell(victim.ref, 99, payload, "m.test", time.Now())
	if !protocol.ChatIsSessionLost(got) {
		t.Fatalf("a cell sealed with the wrong key was accepted: %v", got)
	}
	// The victim's session must be untouched.
	if rep := victim.poll(0); rep.Status != protocol.MTPStatusOK {
		t.Fatalf("victim session disturbed: status %d", rep.Status)
	}
}

// 0 means uncapped: the operator opts into a limit, they do not inherit one.
func TestRelayUncappedByDefault(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.MaxTunnels = 0 })
	for i := 0; i < 8; i++ {
		newTestClient(t, r)
	}
	if active, max := r.Stats(); active != 8 || max != 0 {
		t.Fatalf("stats = %d/%d, want 8/0", active, max)
	}
}

func TestRelayCapacityLimit(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.MaxTunnels = 1 })
	newTestClient(t, r)

	if _, status := tryHandshake(t, r); status != protocol.MTPStatusBusy {
		t.Fatalf("second handshake status = %d, want Busy", status)
	}
	if active, max := r.Stats(); active != 1 || max != 1 {
		t.Fatalf("stats = %d/%d", active, max)
	}
}

// Re-opening on the same session must replace the old tunnel, not leak it.
func TestRelayReopenReplacesTunnel(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.MaxTunnels = 1 })
	c := newTestClient(t, r)
	c.mustOpen()
	c.mustOpen() // same session again: must not hit the cap
	if active, _ := r.Stats(); active != 1 {
		t.Fatalf("active = %d, want 1", active)
	}
}

// A session nobody is using is reclaimed. That is the ONLY thing that takes a
// session away now that closing ends just the tunnel, so it has to work: a
// session left behind holds its buffers and its socket for ever.
func TestRelaySweepsIdleSessions(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.IdleTTL = 10 * time.Millisecond })
	c := newTestClient(t, r)
	c.mustOpen()
	c.op(protocol.BuildMTPClose())

	time.Sleep(30 * time.Millisecond)
	if n := r.Sweep(time.Now()); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if active, _ := r.Stats(); active != 0 {
		t.Fatalf("active = %d after sweep", active)
	}
	// And once swept it really is gone, so a stale client is told so rather
	// than being answered from a half-dead session.
	if body := c.op(protocol.BuildMTPPoll(0)); body != nil {
		t.Fatalf("a swept session still answered: %v", body)
	}
}

// A partial handshake must not be remembered forever.
// At one query a second, with some lost and retried, the two HELLO fragments
// can arrive a long way apart. The second must still complete the handshake:
// treating it as the start of a new attempt means the handshake never finishes
// and every dial dies with "session lost".
func TestRelayHandshakeSurvivesSlowFragments(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	eph, _ := protocol.GenerateEphemeralKey()
	frags, _ := protocol.SplitMTPHello(eph.PublicKey().Bytes(), protocol.ChatCellPayloadSize, rand.Reader)
	if len(frags) < 2 {
		t.Skip("hello fits in one fragment on this build")
	}
	var tag [protocol.ChatSelectorSize]byte
	tag[0] = 2
	protocol.ChatMarkHandshakeSelector(&tag)

	start := time.Now()
	r.HandleCell(tag, 0, frags[0], "m.test", start)
	// A gap a slow, lossy link really produces between two cells.
	late := start.Add(45 * time.Second)
	for i := 1; i < len(frags); i++ {
		r.HandleCell(tag, uint32(i), frags[i], "m.test", late)
	}

	r.mu.Lock()
	sessions := len(r.sessions)
	hs := r.handshakes[tag]
	r.mu.Unlock()
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1: the late fragment restarted the handshake instead of completing it", sessions)
	}
	// Completing it keeps the sealed ack (so a retransmit is idempotent) but
	// drops the fragments, which are no longer needed.
	if hs == nil || hs.ack == nil {
		t.Fatal("completed handshake kept no ack; a retried last fragment would get a short reply")
	}
	// Dropped, but the map itself stays non-nil: a nil map here would panic
	// under r.mu and deadlock the relay if a fragment ever reached the write.
	if len(hs.frags) != 0 {
		t.Errorf("completed handshake still holds %d fragments", len(hs.frags))
	}
	if hs.frags == nil {
		t.Error("fragment map must not be nil; a write to it would panic holding the lock")
	}
}

// The client reads the LAST fragment's reply as the ack, so a lost reply makes
// it re-send that fragment. The relay must answer with the same sealed ack
// rather than treating it as a new, incomplete handshake -- which is what gave
// the user "mtproto: handshake reply: chat: sealed too short".
func TestRelayHandshakeRetransmitGetsTheSameAck(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	eph, _ := protocol.GenerateEphemeralKey()
	frags, _ := protocol.SplitMTPHello(eph.PublicKey().Bytes(), protocol.ChatCellPayloadSize, rand.Reader)
	var tag [protocol.ChatSelectorSize]byte
	tag[0] = 3
	protocol.ChatMarkHandshakeSelector(&tag)

	now := time.Now()
	var ack []byte
	for i, f := range frags {
		ack = r.HandleCell(tag, uint32(i), f, "m.test", now)
	}
	if len(ack) <= len(protocol.ChatSessionLostResp) {
		t.Fatalf("handshake did not produce a sealed ack (%d bytes)", len(ack))
	}

	// The reply was lost: the client sends the last fragment again.
	last := len(frags) - 1
	again := r.HandleCell(tag, uint32(last), frags[last], "m.test", now.Add(20*time.Second))
	if !bytes.Equal(again, ack) {
		t.Fatalf("retransmit got a different reply (%d bytes vs %d); the client cannot open it",
			len(again), len(ack))
	}

	// And it must not have created a second session.
	r.mu.Lock()
	sessions := len(r.sessions)
	r.mu.Unlock()
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1: the retransmit built another session", sessions)
	}
}

// A flood of first fragments must not pin memory now that the TTL is generous.
func TestRelayCapsPendingHandshakes(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	eph, _ := protocol.GenerateEphemeralKey()
	frags, _ := protocol.SplitMTPHello(eph.PublicKey().Bytes(), protocol.ChatCellPayloadSize, rand.Reader)
	now := time.Now()
	for i := 0; i < mtpMaxHandshakes+64; i++ {
		var tag [protocol.ChatSelectorSize]byte
		tag[0] = byte(i)
		tag[1] = byte(i >> 8)
		tag[2] = byte(i >> 16)
		protocol.ChatMarkHandshakeSelector(&tag)
		r.HandleCell(tag, 0, frags[0], "m.test", now)
	}
	r.mu.Lock()
	pending := len(r.handshakes)
	r.mu.Unlock()
	if pending > mtpMaxHandshakes {
		t.Fatalf("pending handshakes = %d, over the %d cap", pending, mtpMaxHandshakes)
	}
}

func TestRelayStaleHandshakeIsSwept(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.IdleTTL = time.Minute })
	eph, _ := protocol.GenerateEphemeralKey()
	frags, _ := protocol.SplitMTPHello(eph.PublicKey().Bytes(), protocol.ChatCellPayloadSize, rand.Reader)
	var tag [protocol.ChatSelectorSize]byte
	tag[0] = 1
	protocol.ChatMarkHandshakeSelector(&tag)
	r.HandleCell(tag, 0, frags[0], "m.test", time.Now()) // first fragment only

	r.mu.Lock()
	pending := len(r.handshakes)
	r.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending handshakes = %d, want 1", pending)
	}
	r.Sweep(time.Now().Add(2 * mtpHandshakeTTL))
	r.mu.Lock()
	pending = len(r.handshakes)
	r.mu.Unlock()
	if pending != 0 {
		t.Fatalf("stale handshake survived: %d", pending)
	}
}

// When Telegram hangs up, buffered bytes must still be delivered, and only the
// reply that empties the buffer reports the close.
func TestRelayDeliversBufferedBytesBeforeReportingClose(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()
	c.data(0, 0, []byte("x"))

	dc.push(t, []byte("final bytes"))
	time.Sleep(50 * time.Millisecond)
	dc.hangUp()
	time.Sleep(50 * time.Millisecond)

	rep := c.poll(0)
	if string(rep.Down) != "final bytes" {
		t.Fatalf("down = %q, want the buffered bytes", rep.Down)
	}
	if rep.Status != protocol.MTPStatusUpstreamClosed {
		t.Fatalf("status = %d; the emptying reply should report the close", rep.Status)
	}
}

func TestRelayDisabledReturnsSessionLost(t *testing.T) {
	if got := NewMTProtoRelay(MTProtoConfig{Enabled: false}); got != nil {
		t.Fatal("disabled relay should be nil")
	}
	var r *MTProtoRelay
	var ref [protocol.ChatSelectorSize]byte
	if got := r.HandleCell(ref, 0, []byte{protocol.MTPOpOpen, 2}, "m.test", time.Now()); !protocol.ChatIsSessionLost(got) {
		t.Fatalf("nil relay replied %v", got)
	}
	if active, max := r.Stats(); active != 0 || max != 0 {
		t.Fatalf("nil relay stats = %d/%d", active, max)
	}
}

func TestRelayMalformedCells(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	for _, bad := range [][]byte{nil, {}, {99}, {protocol.MTPOpOpen}} {
		body := c.op(bad)
		if len(body) > 0 && body[0] == protocol.MTPStatusOK {
			t.Fatalf("malformed cell %v accepted", bad)
		}
	}
	// A DATA cell shorter than its own header cannot be sealed at all: the
	// framing puts it below the cell layer's minimum. So it can never reach the
	// relay, and the sealer says so rather than silently reshaping it.
	short := []byte{protocol.MTPOpData, 1}
	_, err := protocol.SealChatCellPayloadN(c.ks, c.ref, 1, short,
		protocol.MTPCellPad(short, testBudget, 0))
	if err == nil {
		t.Fatal("an undersized DATA cell should not be representable on the wire")
	}
	// Garbage on a handshake tag must not create state either.
	var tag [protocol.ChatSelectorSize]byte
	protocol.ChatMarkHandshakeSelector(&tag)
	// A fragment too small to be part of any key, and an index past the end:
	// neither may create state or produce a reply.
	if got := r.HandleCell(tag, 0, []byte{1}, "m.test", time.Now()); !protocol.ChatIsSessionLost(got) {
		t.Fatalf("undersized handshake fragment accepted: %v", got)
	}
	big := bytes.Repeat([]byte{2}, protocol.ChatCellPayloadSize)
	if got := r.HandleCell(tag, 99, big, "m.test", time.Now()); !protocol.ChatIsSessionLost(got) {
		t.Fatalf("out-of-range fragment index accepted: %v", got)
	}
}

// The relay must only ever dial Telegram's published DC addresses, or it is an
// open proxy.
func TestRelayAllowlistComesFromTelegramDCList(t *testing.T) {
	r := NewMTProtoRelay(MTProtoConfig{Enabled: true})
	if r == nil {
		t.Fatal("nil relay")
	}
	if len(r.addr) < 3 {
		t.Fatalf("production allowlist has %d DCs, expected the full set", len(r.addr))
	}
	for id, host := range r.addr {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			t.Fatalf("dc%d: %q is not host:port", id, host)
		}
		if net.ParseIP(h) == nil {
			t.Fatalf("dc%d: %q is not a literal IP; a hostname would let DNS redirect the relay", id, h)
		}
	}
	test := NewMTProtoRelay(MTProtoConfig{Enabled: true, TestDCs: true})
	if len(test.addr) == 0 {
		t.Fatal("test DC list is empty")
	}
}

// Backpressure: a DC that floods must be paused once the buffer fills, instead
// of growing the relay's memory without bound.
func TestRelayBuffersAreBounded(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.BufferBytes = 2048 })
	cl := newTestClient(t, r)
	cl.mustOpen()
	cl.data(0, 0, []byte("x"))

	go func() {
		blob := bytes.Repeat([]byte{0xAB}, 4096)
		for i := 0; i < 20; i++ {
			d := dc
			d.mu.Lock()
			c := d.conn
			d.mu.Unlock()
			if c == nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			c.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := c.Write(blob); err != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)

	r.mu.Lock()
	sess := r.sessions[cl.ref]
	r.mu.Unlock()
	if sess == nil {
		t.Fatal("session vanished")
	}
	tun := sess.currentTunnel()
	if tun == nil {
		t.Fatal("tunnel vanished")
	}
	tun.mu.Lock()
	held := len(tun.down)
	tun.mu.Unlock()
	// One in-flight read may overshoot the cap, but it must not grow without end.
	if held > 2048+8192 {
		t.Fatalf("buffered %d bytes with a 2048-byte cap", held)
	}
}

var _ = io.EOF

// A downstream ack is cumulative and may only move forward. A later cell
// carrying a LOWER ack (a reordered or replayed cell on a lossy path) must be
// ignored, not rewind the buffer — rewinding would re-deliver bytes the client
// already handed to MTProto, and duplicate bytes break the session.
func TestRelayDownAckNeverRewinds(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()
	c.data(0, 0, []byte("x"))
	dc.push(t, []byte("abcdefghij"))

	first := waitDown(t, c, 0)
	if len(first.Down) < 10 {
		t.Fatalf("expected the whole slice, got %q", first.Down)
	}
	// Acknowledge 5 bytes, then send a stale cell still claiming 4.
	if rep := c.poll(5); rep.DownSeq != 5 {
		t.Fatalf("after ack 5, DownSeq = %d, want 5", rep.DownSeq)
	}
	stale := c.poll(4)
	if stale.DownSeq != 5 {
		t.Fatalf("a lower ack rewound the stream to %d; it must stay at 5", stale.DownSeq)
	}
	if !bytes.Equal(stale.Down, []byte("fghij")) {
		t.Fatalf("after the stale ack the server offered %q, want the un-acked tail", stale.Down)
	}
	// Forward progress still works afterwards.
	if done := c.poll(10); len(done.Down) != 0 || done.DownSeq != 10 {
		t.Fatalf("final ack: down=%q seq=%d", done.Down, done.DownSeq)
	}
}

// The same rule on the upstream side: an ack cannot un-send bytes already
// written to Telegram.
func TestRelayUpstreamAckOnlyMovesForward(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	c.data(0, 0, []byte("hello"))
	if rep := c.data(5, 0, []byte(" world")); rep.UpAck != 11 {
		t.Fatalf("UpAck = %d, want 11", rep.UpAck)
	}
	// A replay of the first cell must not move the stream backwards.
	if rep := c.data(0, 0, []byte("hello")); rep.UpAck != 11 {
		t.Fatalf("replay rewound UpAck to %d, want 11", rep.UpAck)
	}
	if got := dc.waitGot(t, 11); string(got) != "hello world" {
		t.Fatalf("DC got %q", got)
	}
}

// A client that never marks a message complete must not be able to make the
// relay buffer without limit — that is a trivial way to exhaust the server's
// memory from a single tunnel.
func TestRelayCapsUnflushedUpstream(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	tun := &mtpTunnel{conn: nil, bufCap: 1 << 20}
	tun.cond = sync.NewCond(&tun.mu)

	// Feed plain DATA (never DataEnd) past the cap.
	chunk := make([]byte, 512)
	var off uint16
	var refused bool
	for i := 0; i < (mtpMaxPending/len(chunk))+4; i++ {
		resp := tun.handle(&protocol.MTPRequest{
			Op: protocol.MTPOpData, UpSeq: off, Chunk: chunk,
		})
		if len(resp) > 0 && resp[0] == protocol.MTPStatusBadRequest {
			refused = true
			break
		}
		off += uint16(len(chunk))
	}
	if !refused {
		t.Fatal("relay buffered unflushed upstream without limit")
	}
	tun.mu.Lock()
	held := len(tun.pend)
	tun.mu.Unlock()
	if held > mtpMaxPending {
		t.Fatalf("held %d bytes, cap is %d", held, mtpMaxPending)
	}
	_ = r
}

// A message split across cells must reach the far end as ONE write, not as a
// trickle: Telegram closes a connection whose message arrives in slow pieces,
// which is what made the tunnel unusable on a degraded link.
func TestRelayDeliversSplitMessageInOneWrite(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	a, b := []byte("hello "), []byte("world")

	// First half: held, not delivered.
	if rep := c.dataPart(0, 0, a); rep.Status != protocol.MTPStatusOK {
		t.Fatalf("part reply %+v", rep)
	}
	dc.mu.Lock()
	early := len(dc.got)
	dc.mu.Unlock()
	if early != 0 {
		t.Fatalf("relay forwarded an incomplete message: %d bytes", early)
	}

	// Second half marks the end: the whole message lands at once.
	if rep := c.data(uint16(len(a)), 0, b); rep.Status != protocol.MTPStatusOK {
		t.Fatalf("end reply %+v", rep)
	}
	want := append(append([]byte(nil), a...), b...)
	if got := dc.waitGot(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("DC got %q, want %q", got, want)
	}
}

// The relay carries opaque bytes and cannot tell a message from a file, so a
// throughput budget is the only limit it can apply. Nothing is dropped: the
// trimmed bytes stay buffered for the next window.
func TestTunnelDownstreamThrottle(t *testing.T) {
	tun := &mtpTunnel{rateCap: 1000}

	// throttleLocked only TRIMS; chargeLocked is what spends the window. They
	// are separate because the same unacknowledged bytes are offered again in
	// every reply until the client acks, and charging each of those offers
	// divided the cap by the in-flight window.
	if got := tun.throttleLocked(600); got != 600 {
		t.Fatalf("first reply trimmed to %d, want 600", got)
	}
	// Offered again, not yet acknowledged: still the full slice, still free.
	if got := tun.throttleLocked(600); got != 600 {
		t.Fatalf("a re-offer was trimmed to %d; nothing has been delivered yet", got)
	}
	tun.chargeLocked(600) // the client acknowledged them

	if got := tun.throttleLocked(600); got != 400 {
		t.Fatalf("second reply = %d, want the 400 left in the window", got)
	}
	tun.chargeLocked(400)
	if got := tun.throttleLocked(600); got != 0 {
		t.Fatalf("over budget = %d, want 0", got)
	}

	// A new window starts fresh.
	tun.winStart = time.Now().Add(-2 * time.Minute)
	if got := tun.throttleLocked(600); got != 600 {
		t.Fatalf("new window = %d, want 600", got)
	}

	// Uncapped stays uncapped.
	free := &mtpTunnel{}
	if got := free.throttleLocked(99999); got != 99999 {
		t.Fatalf("uncapped tunnel trimmed to %d", got)
	}
}

// Downstream carries the bulk of a conversation, and each reply is one DNS
// query the user waits for, so a reply has to fill the response it rides in.
func TestReplyFillsTheDNSResponse(t *testing.T) {
	var ks [protocol.KeySize]byte
	var sel [3]byte
	sealedSize := func(down int) int {
		body := protocol.BuildMTPReply(protocol.MTPReply{
			Status: protocol.MTPStatusOK, UpAck: 1, DownSeq: 2, Down: make([]byte, down),
		})
		return len(protocol.SealChatResponse(ks, sel, 7, protocol.MTPStatusOK, body))
	}
	if got := sealedSize(mtpMaxDownPerReply); got > protocol.MaxBlockPayload {
		t.Fatalf("a full reply is %d B, over the %d B a response carries",
			got, protocol.MaxBlockPayload)
	}
	// And it is not leaving a lot of room unused: the whole point is to spend
	// as few queries as possible.
	headroom := 0
	for n := mtpMaxDownPerReply; sealedSize(n) <= protocol.MaxBlockPayload; n++ {
		headroom = n - mtpMaxDownPerReply
	}
	if headroom > 32 {
		t.Errorf("%d more bytes would still fit in every reply", headroom)
	}
}

// With several queries in flight the client cannot control which answer lands
// first, so cells arrive out of order. The relay must hold the early ones,
// fill the gaps as they turn up, and hand the far end the whole message only
// once the stream is contiguous through its end.
func TestRelayReassemblesOutOfOrderCells(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	// "hello world" in three cells, delivered last, first, middle.
	parts := [][]byte{[]byte("hell"), []byte("o wo"), []byte("rld!")}
	offs := []uint16{0, 4, 8}

	// The last cell ends the message, but arrives first.
	if rep := c.data(offs[2], 0, parts[2]); rep.Status != protocol.MTPStatusOK {
		t.Fatalf("tail reply %+v", rep)
	}
	dc.mu.Lock()
	early := len(dc.got)
	dc.mu.Unlock()
	if early != 0 {
		t.Fatalf("relay delivered before the message was whole: %d bytes", early)
	}

	// Then the first, which still leaves a hole in the middle.
	if rep := c.dataPart(offs[0], 0, parts[0]); rep.Status != protocol.MTPStatusOK {
		t.Fatalf("head reply %+v", rep)
	}
	dc.mu.Lock()
	stillEarly := len(dc.got)
	dc.mu.Unlock()
	if stillEarly != 0 {
		t.Fatalf("relay delivered with a hole in the stream: %d bytes", stillEarly)
	}

	// The middle completes it: everything goes at once, in order.
	if rep := c.dataPart(offs[1], 0, parts[1]); rep.Status != protocol.MTPStatusOK {
		t.Fatalf("middle reply %+v", rep)
	}
	want := []byte("hello world!")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		dc.mu.Lock()
		got := append([]byte(nil), dc.got...)
		dc.mu.Unlock()
		if bytes.Equal(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	dc.mu.Lock()
	got := append([]byte(nil), dc.got...)
	dc.mu.Unlock()
	t.Fatalf("far end got %q, want %q", got, want)
}

// An early cell is acknowledged but not counted as received: the ack has to
// keep naming the gap, or the client would believe the hole was filled.
func TestRelayAcksOnlyTheContiguousStream(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	rep := c.dataPart(8, 0, []byte("late"))
	if rep.Status != protocol.MTPStatusOK {
		t.Fatalf("reply %+v", rep)
	}
	if rep.UpAck != 0 {
		t.Errorf("acked %d with a gap at 0: the client would skip the hole", rep.UpAck)
	}
}

// Streaming after the first message writes contiguous bytes as they arrive.
// Cells race on a parallel window, so the ordering guarantee has to survive
// them landing out of order: Telegram must still see one ordered byte stream.
func TestRelayStreamsOutOfOrderCellsInStreamOrder(t *testing.T) {
	dc := newFakeDC(t)
	c := newTestClient(t, newTestRelay(t, dc, nil))
	c.mustOpen()

	c.data(0, 0, []byte("first")) // whole message: dials, starts streaming
	dc.waitGot(t, 5)

	// Offsets 5..8 and 9..12, delivered back to front.
	c.dataPart(9, 0, []byte("cdef"))
	// The second half alone must NOT reach the far end: it would corrupt the
	// stream. Only the gap in front of it being filled may release it.
	time.Sleep(40 * time.Millisecond)
	if got := string(dc.snapshot()); got != "first" {
		t.Fatalf("DC got %q before the gap was filled; bytes were written out of order", got)
	}

	c.dataPart(5, 0, []byte("ab12"))
	if got := dc.waitGot(t, 13); string(got) != "firstab12cdef" {
		t.Fatalf("DC got %q, want %q", got, "firstab12cdef")
	}
}

// snapshot is what the DC has received so far, without waiting for more.
func (d *fakeDC) snapshot() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.got...)
}

// Closing ends the TUNNEL and leaves the session alone.
//
// The client sends a close when one connection to Telegram is done, which gotd
// does on every re-dial. Dropping the session there threw away the keys the
// next tunnel needs -- with one session shared across dials, the first close
// killed the connection for good and every later dial answered "session lost".
func TestCloseEndsTheTunnelNotTheSession(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()
	c.data(0, 0, []byte("hello"))
	dc.waitGot(t, 5)

	c.closeTunnel()

	if active, _ := r.Stats(); active != 1 {
		t.Fatalf("sessions = %d, want the session to survive a tunnel close", active)
	}
	// And the same session can carry a new tunnel straight away: that is what
	// makes a reconnect one query instead of a fresh handshake. (The stand-in
	// DC accepts a single connection, so this checks the open is accepted
	// rather than following the bytes through a second one.)
	if st := c.openTunnel(2); st != protocol.MTPStatusOK {
		t.Fatalf("re-open on the same session = %d, want OK", st)
	}
	if active, _ := r.Stats(); active != 1 {
		t.Fatalf("sessions = %d after re-open, want 1", active)
	}
}

// closeTunnel sends the close the client sends when a connection ends.
func (c *testClient) closeTunnel() {
	c.t.Helper()
	c.op(protocol.BuildMTPClose())
}

// A captured RESPONSE replayed as a REQUEST must be refused before anything is
// sealed.
//
// Request counters live below 0x400000; a response is sealed under
// reqCounter|0x800000. Both directions use the same session key and the same
// AEAD, so a recorded response cell opens perfectly well when fed back in as a
// request under its own counter. Answering it would then seal a fresh reply
// under chatRespCounter(0x800000|X) — which is 0x800000|X again, the very
// nonce the captured response already used. Reusing an AEAD nonce under one
// key is total loss of confidentiality and integrity for those two messages.
//
// The chat relay has always refused counters in the reserved range; the
// MTProto relay was built from it and did not.
func TestMTProtoRefusesReservedCounters(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()

	// A genuine request, and the reply the relay sealed for it.
	plaintext := protocol.BuildMTPPoll(0)
	reqCtr := c.ctr
	jitter := protocol.ChatCellJitter(r.cfg.QueryKey, c.ref, reqCtr)
	padded := protocol.MTPCellPad(plaintext, testBudget, jitter)
	payload, err := protocol.SealChatCellPayloadN(c.ks, c.ref, reqCtr, plaintext, padded)
	if err != nil {
		t.Fatal(err)
	}
	sealedReply := r.HandleCell(c.ref, reqCtr, payload, "m.test", time.Now())
	if protocol.ChatIsSessionLost(sealedReply) {
		t.Fatal("the honest request should have been answered")
	}

	// Now replay that reply as a request, numbered the way it was sealed.
	respCtr := reqCtr | 0x800000
	out := r.HandleCell(c.ref, respCtr, sealedReply, "m.test", time.Now())
	if !protocol.ChatIsSessionLost(out) {
		t.Fatalf("a response replayed as a request was answered — the reply is sealed "+
			"under a nonce already used (counter %#x)", respCtr)
	}

	// The bootstrap counter is reserved too: it is what the handshake seals
	// under, and an in-session cell must never reach for it.
	if out := r.HandleCell(c.ref, protocol.ChatBootstrapCounter(), payload, "m.test", time.Now()); !protocol.ChatIsSessionLost(out) {
		t.Fatal("the bootstrap counter must be refused in-session")
	}

	// Ordinary traffic is untouched: the session is still there and answers.
	if body := c.op(protocol.BuildMTPPoll(0)); body == nil {
		t.Fatal("honest cells must still be answered after a refused one")
	}
}

// A cell that does not open must not keep a session alive. The selector
// travels in the clear, so anyone who sees one could otherwise hold a session,
// its upstream socket and its buffers open indefinitely with junk.
func TestMTProtoUnopenableCellDoesNotRefreshSession(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()

	r.mu.Lock()
	sess := r.sessions[c.ref]
	r.mu.Unlock()
	if sess == nil {
		t.Fatal("no session")
	}
	sess.tmu.Lock()
	before := sess.lastSeen
	sess.tmu.Unlock()

	// Junk under a plausible counter: opens for nobody.
	junk := make([]byte, protocol.ChatCellPayloadSize)
	later := before.Add(time.Hour)
	if out := r.HandleCell(c.ref, c.ctr+50, junk, "m.test", later); !protocol.ChatIsSessionLost(out) {
		t.Fatal("junk must not be answered")
	}

	sess.tmu.Lock()
	after := sess.lastSeen
	sess.tmu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("an unopenable cell refreshed lastSeen (%v -> %v): the idle sweep "+
			"can be held off by anyone who knows the selector", before, after)
	}
}

// A retransmitted OPEN must not wreck a tunnel that is mid-stream.
//
// The client retransmits the identical cell on transport loss -- UDP drops
// replies, and its own comment calls every op idempotent. DATA and POLL are:
// each carries its own stream offset. OPEN is not. Carrying it out twice built
// a second tunnel with its offsets at zero and swapped it in under a client
// half way through a message, and both directions then stalled: the client is
// writing at offset N to a relay expecting 0.
func TestMTProtoDuplicateOpenDoesNotResetTheTunnel(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)

	openCtr := c.ctr
	if st := c.openTunnel(2); st != protocol.MTPStatusOK {
		t.Fatalf("open status %d", st)
	}
	first := c.r.sessions[c.ref].currentTunnel()
	if first == nil {
		t.Fatal("no tunnel after open")
	}

	// Get the stream off zero, so a reset is visible.
	advance := []byte("advance it")
	c.data(0, 0, advance)

	// The very same cell again, as a lost reply would produce.
	jitter := protocol.ChatCellJitter(r.cfg.QueryKey, c.ref, openCtr)
	plaintext := protocol.BuildMTPOpen(2)
	padded := protocol.MTPCellPad(plaintext, testBudget, jitter)
	payload, err := protocol.SealChatCellPayloadN(c.ks, c.ref, openCtr, plaintext, padded)
	if err != nil {
		t.Fatal(err)
	}
	raw := r.HandleCell(c.ref, openCtr, payload, "m.test", time.Now())
	if protocol.ChatIsSessionLost(raw) {
		t.Fatal("a retransmitted open must still be answered")
	}
	if _, _, err := protocol.OpenChatResponse(c.ks, c.ref, openCtr, raw); err != nil {
		t.Fatalf("retransmit reply did not open: %v", err)
	}

	if got := c.r.sessions[c.ref].currentTunnel(); got != first {
		t.Fatal("a retransmitted OPEN replaced the live tunnel — the stream offsets " +
			"went back to zero under a client that is past them")
	}
}

// Likewise a stale CLOSE: a retry overtaken by its own reply, or a recorded
// cell replayed, must not tear down a tunnel opened after it.
func TestMTProtoStaleCloseDoesNotDropALaterTunnel(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()

	closeCtr := c.ctr
	closePlain := protocol.BuildMTPClose()
	jitter := protocol.ChatCellJitter(r.cfg.QueryKey, c.ref, closeCtr)
	payload, err := protocol.SealChatCellPayloadN(c.ks, c.ref, closeCtr, closePlain,
		protocol.MTPCellPad(closePlain, testBudget, jitter))
	if err != nil {
		t.Fatal(err)
	}
	c.ctr++
	if raw := r.HandleCell(c.ref, closeCtr, payload, "m.test", time.Now()); protocol.ChatIsSessionLost(raw) {
		t.Fatal("close should be answered")
	}

	// A new tunnel on the same session, as gotd's next dial produces.
	if st := c.openTunnel(2); st != protocol.MTPStatusOK {
		t.Fatalf("second open status %d", st)
	}
	live := c.r.sessions[c.ref].currentTunnel()
	if live == nil {
		t.Fatal("no tunnel after the second open")
	}

	// The old CLOSE, arriving late.
	r.HandleCell(c.ref, closeCtr, payload, "m.test", time.Now())
	if got := c.r.sessions[c.ref].currentTunnel(); got != live {
		t.Fatal("a stale CLOSE tore down a tunnel opened after it")
	}
}

// floodTag builds a distinct handshake selector per attacker attempt.
func floodTag(i int) [protocol.ChatSelectorSize]byte {
	// Byte 0 carries the handshake flag, so the counter lives in the rest:
	// writing it into byte 0 made every other tag collide with its own
	// flag-bit twin and only half the table filled.
	var tag [protocol.ChatSelectorSize]byte
	tag[1] = byte(i)
	tag[2] = byte(i >> 8)
	protocol.ChatMarkHandshakeSelector(&tag)
	return tag
}

// A flood of half-finished handshakes must not lock out everyone else.
//
// The table was capped and the newcomer refused when it was full, while every
// fragment refreshed the TTL of an entry already in it. So filling the table
// once and re-sending one fragment per entry per TTL -- a few queries a second
// -- held every slot indefinitely, and every genuine user from then on was
// told "session lost". At capacity the oldest entry goes instead, so a flood
// displaces itself.
func TestMTProtoHandshakeFloodDoesNotLockOutNewUsers(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)

	// Fill the table with first fragments that never complete.
	frag := make([]byte, protocol.ChatCellPayloadSize)
	now := time.Now()
	for i := 0; i < mtpMaxHandshakes; i++ {
		tag := floodTag(i)
		r.HandleCell(tag, 0, frag, "m.test", now)
	}
	r.mu.Lock()
	held := len(r.handshakes)
	r.mu.Unlock()
	if held < mtpMaxHandshakes {
		t.Fatalf("only %d entries held, wanted the table full", held)
	}

	// Keep them all alive the way the attack does, then a real user arrives.
	for i := 0; i < mtpMaxHandshakes; i++ {
		tag := floodTag(i)
		r.HandleCell(tag, 0, frag, "m.test", now.Add(mtpHandshakeTTL-time.Second))
	}
	if _, status := tryHandshake(t, r); status != protocol.MTPStatusOK {
		t.Fatalf("a genuine handshake was refused (status %d) while the table was "+
			"held full — every new user is locked out for as long as it goes on", status)
	}
}

// An attempt must not be holdable for ever by being poked.
//
// Every fragment used to push the expiry a full TTL further out, so an entry
// touched regularly never aged. Refreshes are now measured from when the
// attempt started: it can be kept alive, but only up to a total lifetime.
// (Letting it lapse and starting again is a NEW attempt, which is legitimate
// and is what the flood test above covers.)
func TestMTProtoHandshakeHasATotalLifetime(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	frag := make([]byte, protocol.ChatCellPayloadSize)
	tag := floodTag(7)

	start := time.Now()
	r.HandleCell(tag, 0, frag, "m.test", start)
	// Poked well within the TTL every time, so it never lapses.
	for at := start; at.Before(start.Add(3 * mtpHandshakeMaxLife)); at = at.Add(mtpHandshakeTTL / 4) {
		r.HandleCell(tag, 0, frag, "m.test", at)
	}

	r.mu.Lock()
	hs := r.handshakes[tag]
	var expires, born time.Time
	if hs != nil {
		expires, born = hs.expires, hs.born
	}
	r.mu.Unlock()
	if hs == nil {
		t.Skip("entry already gone, which is also fine")
	}
	if limit := born.Add(mtpHandshakeMaxLife); expires.After(limit) {
		t.Fatalf("expiry %v is past the total lifetime %v — an entry poked regularly "+
			"never ages and holds its slot for ever", expires, limit)
	}
}

// The minute budget must count bytes DELIVERED, not bytes offered.
//
// Buffered downstream is not dropped until the client acknowledges it, so the
// same leading slice is handed back in every reply until that ack arrives —
// and with a window of cells in flight, that is every cell in the window. The
// budget was charged on each of those, so one delivery was billed many times
// and the cap came out divided by the window size: 512 KiB a minute behaved
// like about seventeen.
func TestMTProtoThrottleChargesDeliveredBytesOnce(t *testing.T) {
	dc := newFakeDC(t)
	const cap = 4096
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.DownBytesPerMin = cap })
	c := newTestClient(t, r)
	c.mustOpen()
	c.data(0, 0, []byte("go")) // dial

	payload := bytes.Repeat([]byte("D"), 600)
	dc.push(t, payload)

	tun := r.sessions[c.ref].currentTunnel()
	if tun == nil {
		t.Fatal("no tunnel")
	}

	// The client polls repeatedly WITHOUT acknowledging, exactly as a window
	// of in-flight cells does.
	offered := c.pollUntilOffer(0)
	for i := 0; i < 8; i++ {
		rep := c.poll(0)
		if len(rep.Down) == 0 {
			t.Fatalf("re-offer %d came back empty: an unacknowledged slice must "+
				"stay on offer, or the client stalls on bytes nobody will resend", i)
		}
		offered = len(rep.Down)
	}
	tun.mu.Lock()
	charged := tun.winBytes
	tun.mu.Unlock()
	if charged != 0 {
		t.Fatalf("budget charged %d bytes for re-offers of the same unacknowledged "+
			"slice (offered %d per reply) — the cap is divided by the in-flight window",
			charged, offered)
	}

	// Acknowledging is what actually spends the budget, once.
	c.poll(uint16(offered))
	tun.mu.Lock()
	charged = tun.winBytes
	tun.mu.Unlock()
	if charged != offered {
		t.Fatalf("after acking %d bytes the budget shows %d", offered, charged)
	}
}

// A cell that grows at an offset already held must replace what is held.
//
// The client re-chunks everything still unacknowledged on every round, so a
// partial ack followed by a fresh write can widen the cell that starts at a
// given offset: the tail of one write and the head of the next end up in one
// cell. Keeping the short version and dropping the long one left the bytes
// between them in nobody's hands — the client counts them as sent, the relay
// never had them, and the stream stalls on a gap that is never retransmitted.
func TestMTProtoLongerCellAtAHeldOffsetReplacesIt(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()

	// Out of order: the cell at offset 4 arrives first and is held.
	short := []byte("ab")
	c.dataPart(4, 0, short)

	// Then the same offset again, now carrying more — the tail plus what was
	// written after it.
	grown := []byte("abcdef")
	c.dataPart(4, 0, grown)

	// Finally the gap in front of it.
	head := []byte("0123")
	c.data(0, 0, head)

	want := append(append([]byte(nil), head...), grown...)
	if got := dc.waitGot(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("DC got %q, want %q — the bytes past the short cell were dropped", got, want)
	}
}
