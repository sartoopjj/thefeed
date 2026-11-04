package server

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// connsDC is a DC that keeps what each connection sent apart.
type connsDC struct {
	mu    sync.Mutex
	conns [][]byte
}

func newConnsDC(t *testing.T) (*connsDC, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	d := &connsDC{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.mu.Lock()
			i := len(d.conns)
			d.conns = append(d.conns, nil)
			d.mu.Unlock()
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						d.mu.Lock()
						d.conns[i] = append(d.conns[i], buf[:n]...)
						d.mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return d, ln.Addr().String()
}

// got waits until connection i has received n bytes, or returns what it has.
func (d *connsDC) got(i, n int) []byte {
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		var b []byte
		if i < len(d.conns) {
			b = append([]byte(nil), d.conns[i]...)
		}
		d.mu.Unlock()
		if len(b) >= n || time.Now().After(deadline) {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sealed builds a cell at the next counter without sending it.
func (c *testClient) sealed(plaintext []byte) (uint32, []byte) {
	c.t.Helper()
	ctr := c.ctr
	c.ctr++
	jitter := protocol.ChatCellJitter(c.r.cfg.QueryKey, c.ref, ctr)
	p, err := protocol.SealChatCellPayloadN(c.ks, c.ref, ctr, plaintext, protocol.MTPCellPad(plaintext, testBudget, jitter))
	if err != nil {
		c.t.Fatal(err)
	}
	return ctr, p
}

func (c *testClient) deliver(ctr uint32, payload []byte) []byte {
	return c.r.HandleCell(c.ref, ctr, payload, "m.test", time.Now())
}

func relayWithConnsDC(t *testing.T) (*MTProtoRelay, *connsDC) {
	d, addr := newConnsDC(t)
	r := newTestRelay(t, newFakeDC(t), nil)
	r.addr = map[uint8]string{2: addr}
	return r, d
}

// A packet from an earlier connection never reaches a later one: neither a
// late copy of one already carried out, nor one whose every copy arrives
// only after the reconnect.
func TestOldCellsNeverReachANewTunnel(t *testing.T) {
	r, d := relayWithConnsDC(t)
	c := newTestClient(t, r)
	for round := 0; round < 10; round++ {
		c.mustOpen()
		ctrA, cellA := c.sealed(protocol.BuildMTPDataEnd(0, 0, []byte(fmt.Sprintf("old-%d", round))))
		c.deliver(ctrA, cellA)
		ctrB, cellB := c.sealed(protocol.BuildMTPDataEnd(0, 0, []byte("late-first-copy")))
		d.got(2*round, 5)
		c.op(protocol.BuildMTPClose())
		c.mustOpen()
		for i := 0; i < 3; i++ { // resolvers hand the old copies over again and again
			c.deliver(ctrA, cellA)
			c.deliver(ctrB, cellB)
		}
		msg := []byte(fmt.Sprintf("new-%d", round))
		c.data(0, 0, msg)
		got := d.got(2*round+1, len(msg))
		if !bytes.Equal(got, msg) {
			t.Fatalf("round %d: the new connection got %q, want %q", round, got, msg)
		}
		c.op(protocol.BuildMTPClose())
	}
}

// The same packet twice gets the same bytes back, whatever changed between.
func TestRepeatGetsTheSameReply(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()
	c.data(0, 0, []byte("hi"))
	dc.waitGot(t, 2)
	ctr, poll := c.sealed(protocol.BuildMTPPoll(0))
	first := c.deliver(ctr, poll)
	dc.push(t, []byte("downstream arrived in between"))
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if again := c.deliver(ctr, poll); !bytes.Equal(again, first) {
			t.Fatalf("copy %d: not the reply the first got", i)
		}
	}
}

// A repeat within mtpReplyKeep gets the same bytes; after it, nothing -- the
// cell is never carried out again and sealed with different content.
func TestRepeatsAcrossTheKeepTime(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, func(c *MTProtoConfig) { c.IdleTTL = time.Hour })
	c := newTestClient(t, r)
	c.mustOpen()
	c.data(0, 0, []byte("hi"))
	dc.waitGot(t, 2)
	t0 := time.Now()
	at := func(ctr uint32, cell []byte, when time.Time) []byte {
		return r.HandleCell(c.ref, ctr, cell, "m.test", when)
	}
	ctr, poll := c.sealed(protocol.BuildMTPPoll(0))
	first := at(ctr, poll, t0)
	dc.push(t, []byte("state changed"))
	time.Sleep(100 * time.Millisecond)

	other, otherCell := c.sealed(protocol.BuildMTPPoll(0))
	at(other, otherCell, t0.Add(mtpReplyKeep-10*time.Second))
	if again := at(ctr, poll, t0.Add(mtpReplyKeep-10*time.Second)); !bytes.Equal(again, first) {
		t.Fatal("a repeat inside the keep time did not get the same reply")
	}
	later, laterCell := c.sealed(protocol.BuildMTPPoll(0))
	at(later, laterCell, t0.Add(mtpReplyKeep+time.Second))
	for i := 0; i < 3; i++ {
		if again := at(ctr, poll, t0.Add(mtpReplyKeep+time.Second)); len(again) != 0 {
			t.Fatalf("copy %d after the keep time was answered", i)
		}
	}
}

// However many cells a session sends, what it keeps stays under the cap, and
// a repeat whose reply was dropped for room gets nothing.
func TestRepliesStayUnderTheCap(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()
	ctr0, cell0 := c.sealed(protocol.BuildMTPPoll(0))
	c.deliver(ctr0, cell0)
	for i := 0; i < 10000; i++ {
		c.poll(0)
	}
	sess := r.sessions[c.ref]
	sess.tmu.Lock()
	kept, n := sess.replies.bytes, len(sess.replies.byCtr)
	sess.tmu.Unlock()
	if kept > mtpReplyBytes || n != len(sess.replies.order) {
		t.Fatalf("kept %d bytes in %d replies (cap %d)", kept, n, mtpReplyBytes)
	}
	if again := c.deliver(ctr0, cell0); len(again) != 0 {
		t.Fatal("a repeat whose reply was dropped for room was answered again")
	}
}

// Counters keep working far past the window, in and out of order.
func TestCountersPastTheWindow(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()
	served := func(ctr uint32, cell []byte) bool {
		raw := c.deliver(ctr, cell)
		return len(raw) > 0 && !protocol.ChatIsSessionLost(raw)
	}
	held := map[int][2][]byte{}
	ctrs := map[int]uint32{}
	for i := 0; i < mtpSeenWindow+2000; i++ {
		ctr, cell := c.sealed(protocol.BuildMTPPoll(0))
		if i == mtpSeenWindow+500 || i == mtpSeenWindow+1500 { // these arrive late
			held[i], ctrs[i] = [2][]byte{cell}, ctr
			continue
		}
		if !served(ctr, cell) {
			t.Fatalf("in-order counter %d not served", ctr)
		}
	}
	for i, h := range held {
		if !served(ctrs[i], h[0]) {
			t.Fatalf("late first copy of counter %d not served", ctrs[i])
		}
		if raw := c.deliver(ctrs[i], h[0]); len(raw) == 0 {
			t.Fatalf("its repeat lost the reply the first copy got")
		}
	}
}

// First copies arriving far out of order are all carried out: a fast client
// with many packets in flight must not lose any.
func TestOutOfOrderFirstCopiesAreServed(t *testing.T) {
	dc := newFakeDC(t)
	r := newTestRelay(t, dc, nil)
	c := newTestClient(t, r)
	c.mustOpen()
	const n = 1000
	cells := make([][]byte, n)
	ctrs := make([]uint32, n)
	for i := 0; i < n; i++ {
		ctrs[i], cells[i] = c.sealed(protocol.BuildMTPPoll(0))
	}
	order := []int{n - 1}
	for i := 0; i < n-1; i++ {
		order = append(order, i)
	}
	for _, i := range order {
		raw := c.deliver(ctrs[i], cells[i])
		if len(raw) == 0 || protocol.ChatIsSessionLost(raw) {
			t.Fatalf("first copy of counter %d (out of order) was not served", ctrs[i])
		}
		if _, _, err := protocol.OpenChatResponse(c.ks, c.ref, ctrs[i], raw); err != nil {
			t.Fatalf("counter %d: %v", ctrs[i], err)
		}
	}
}
