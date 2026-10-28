package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// fakeTunnelServer is the relay half of the cell protocol, in memory: it takes
// upstream bytes, hands back downstream bytes, and can be told to misbehave the
// way a DNS path does (replayed cells, re-sent replies, tiny chunks).
type fakeTunnelServer struct {
	mu sync.Mutex

	up      []byte // received from the client
	upOff   uint16 // next upstream offset expected
	down    []byte // queued for the client
	downOff uint16 // stream offset of down[0]

	chunk   int  // chunk size advertised at OPEN
	maxDown int  // bytes per reply (a DNS response holds ~600)
	status  byte // status to report on the next data reply

	// Fault injection.
	replayEvery int  // deliver every Nth request twice (a lost reply, then a retry)
	resendDown  bool // re-send the previous downstream slice before the new one
	// dropEvery fails every Nth query outright, the way a lossy link does.
	// The cell is NOT delivered, so the client must re-send it.
	dropEvery int
	drops     int

	calls    int
	lastDown []byte
	lastSeq  uint16
	// ends counts DataEnd cells: how many pieces the far end would see.
	ends int
	// latency delays every answer, and inFlight/peak record how many queries
	// overlapped -- the measure of whether a window is doing anything.
	latency  time.Duration
	inFlight int
	peak     int
}

// dropped is how many queries the link swallowed.
func (s *fakeTunnelServer) dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drops
}

func (s *fakeTunnelServer) peakConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

func (s *fakeTunnelServer) flushes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ends
}

func newFakeServer() *fakeTunnelServer {
	return &fakeTunnelServer{chunk: 10, maxDown: 600, status: protocol.MTPStatusOK}
}

func (s *fakeTunnelServer) queueDown(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = append(s.down, b...)
}

func (s *fakeTunnelServer) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.up...)
}

// handle applies one request and builds its reply.
func (s *fakeTunnelServer) handle(plain []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(plain) > 0 && plain[0] == protocol.MTPOpOpen {
		return protocol.BuildMTPOpenReply(protocol.MTPStatusOK, byte(s.chunk))
	}
	if len(plain) > 0 && plain[0] == protocol.MTPOpClose {
		return []byte{protocol.MTPStatusOK}
	}

	req, err := protocol.ParseMTPRequest(plain)
	if err != nil {
		return []byte{protocol.MTPStatusBadRequest}
	}

	// Accept only the slice that starts where we are; an earlier offset is a
	// replay. DataEnd carries bytes like Data, and an unacked one hangs the
	// client's Write for ever.
	if req.Op == protocol.MTPOpDataEnd {
		s.ends++
	}
	isData := req.Op == protocol.MTPOpData || req.Op == protocol.MTPOpDataEnd
	if isData && len(req.Chunk) > 0 {
		if req.UpSeq == s.upOff {
			s.up = append(s.up, req.Chunk...)
			s.upOff += uint16(len(req.Chunk))
		}
	}

	// Drop downstream the client has acknowledged.
	if adv := protocol.MTPSeqDelta(s.downOff, req.DownAck); adv > 0 && adv <= len(s.down) {
		s.down = s.down[adv:]
		s.downOff = req.DownAck
	}

	out, seq := s.lastDown, s.lastSeq
	if !s.resendDown || out == nil {
		n := len(s.down)
		if n > s.maxDown {
			n = s.maxDown
		}
		out, seq = append([]byte(nil), s.down[:n]...), s.downOff
		s.lastDown, s.lastSeq = out, seq
	}
	left := len(s.down) - len(out)
	if left < 0 {
		left = 0
	}
	return protocol.BuildMTPReply(protocol.MTPReply{
		Status: s.status, UpAck: s.upOff, DownSeq: seq,
		DownLeft: uint16(left), Down: out,
	})
}

// exchange is the TunnelExchange the client drives.
func (s *fakeTunnelServer) exchange(ctx context.Context, plain []byte) ([]byte, error) {
	s.mu.Lock()
	s.calls++
	replay := s.replayEvery > 0 && s.calls%s.replayEvery == 0
	// The OPEN is call 1 and is left alone: these tests are about a lossy
	// link under an ESTABLISHED tunnel, not about failing to dial one.
	drop := s.dropEvery > 0 && s.calls > 1 && s.calls%s.dropEvery == 0
	if drop {
		s.drops++
	}
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	latency := s.latency
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if drop {
		return nil, errors.New("resolver timeout")
	}
	reply := s.handle(plain)
	if replay {
		// The reply was "lost": the cell layer retried and the server saw the
		// very same request again. The second reply is the one that arrives.
		reply = s.handle(plain)
	}
	return reply, nil
}

// abridgedFrame wraps a payload the way gotd's codec does, so transport tests
// carry the framing the tunnel is built for. The payload is padded to a
// 4-byte boundary, as MTProto messages always are.
func abridgedFrame(payload []byte) []byte {
	for len(payload)%4 != 0 {
		payload = append(payload, 0)
	}
	words := len(payload) / 4
	var hdr []byte
	if words < 127 {
		hdr = []byte{byte(words)}
	} else {
		hdr = []byte{0x7f, byte(words), byte(words >> 8), byte(words >> 16)}
	}
	return append(hdr, payload...)
}

func dialFake(t *testing.T, s *fakeTunnelServer) *TunnelConn {
	t.Helper()
	return dialFakeWindow(t, s, 0)
}

// dialFakeWindow is dialFake with an explicit send window. Zero keeps the
// one-cell-at-a-time default the older tests are written against.
func dialFakeWindow(t *testing.T, s *fakeTunnelServer, window int) *TunnelConn {
	t.Helper()
	return dialFakeOpts(t, s, TunnelOptions{Window: window})
}

// dialFakeOpts dials with caller-supplied options.
//
// Callbacks MUST be passed here rather than assigned to c.opts afterwards: the
// pump goroutine is already running and reading them by the time DialTunnel
// returns, so a later assignment is a data race -- one that `-race` caught
// about five runs in six and would have masked any real race in the package.
func dialFakeOpts(t *testing.T, s *fakeTunnelServer, opts TunnelOptions) *TunnelConn {
	t.Helper()
	if opts.IdlePoll == 0 {
		opts.IdlePoll = time.Millisecond
	}
	if opts.MaxIdlePoll == 0 {
		opts.MaxIdlePoll = 2 * time.Millisecond
	}
	c, err := DialTunnel(context.Background(), s.exchange, 2, opts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Read and Write block until the far end acks, so a regression hangs the
	// package instead of failing it; this deadline makes it a normal failure.
	deadline := time.Now().Add(10 * time.Second)
	_ = c.SetReadDeadline(deadline)
	_ = c.SetWriteDeadline(deadline)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestTunnelWriteReachesServer(t *testing.T) {
	s := newFakeServer()
	c := dialFake(t, s)
	msg := abridgedFrame([]byte("hello from mtproto"))
	n, err := c.Write(msg)
	if err != nil || n != len(msg) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if got := s.received(); !bytes.Equal(got, msg) {
		t.Fatalf("server got %q, want %q", got, msg)
	}
}

// A write far larger than one cell must arrive whole and in order.
func TestTunnelLargeWriteIsSplitAndReassembled(t *testing.T) {
	s := newFakeServer()
	s.chunk = 3 // the compact budget: the worst case the scorer can pick
	c := dialFake(t, s)

	payload := make([]byte, 700)
	rand.New(rand.NewSource(1)).Read(payload)
	msg := abridgedFrame(payload)
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := s.received(); !bytes.Equal(got, msg) {
		t.Fatalf("reassembled %d bytes, want %d (equal=%v)", len(got), len(msg), bytes.Equal(got, msg))
	}
}

func TestTunnelReadReassemblesDownstream(t *testing.T) {
	s := newFakeServer()
	s.maxDown = 7 // force many replies
	c := dialFake(t, s)

	want := []byte("the quick brown fox jumps over the lazy dog")
	s.queueDown(want)

	got := make([]byte, 0, len(want))
	buf := make([]byte, 16)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for len(got) < len(want) {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("read after %d bytes: %v", len(got), err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A replayed cell must not duplicate bytes in either direction — this is the
// property that lets the transport retry freely over DNS.
func TestTunnelReplayedCellsAreIdempotent(t *testing.T) {
	s := newFakeServer()
	s.chunk = 4
	s.replayEvery = 2 // every other request is delivered twice
	s.maxDown = 5
	c := dialFake(t, s)

	down := []byte("downstream payload that spans several replies")
	s.queueDown(down)
	up := abridgedFrame([]byte("upstream payload that spans several cells"))
	if _, err := c.Write(up); err != nil {
		t.Fatal(err)
	}
	if got := s.received(); !bytes.Equal(got, up) {
		t.Fatalf("upstream duplicated: got %q want %q", got, up)
	}

	got := make([]byte, 0, len(down))
	buf := make([]byte, 8)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for len(got) < len(down) {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("read after %d: %v", len(got), err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, down) {
		t.Fatalf("downstream duplicated: got %q want %q", got, down)
	}
}

// The server re-sending a slice the client already consumed must be trimmed,
// not appended twice.
func TestTunnelDuplicateDownstreamIsTrimmed(t *testing.T) {
	s := newFakeServer()
	s.maxDown = 4
	s.resendDown = true
	c := dialFake(t, s)
	s.queueDown([]byte("abcd"))

	buf := make([]byte, 32)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "abcd" {
		t.Fatalf("got %q", buf[:n])
	}
	// Nothing new should ever arrive, however many times it is re-sent.
	c.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	if n, err := c.Read(buf); err == nil {
		t.Fatalf("duplicate slice surfaced as %q", buf[:n])
	}
}

// Bytes buffered when the far end hangs up must still be readable, and only
// then io.EOF.
func TestTunnelUpstreamClosedDeliversBufferedBytesThenEOF(t *testing.T) {
	s := newFakeServer()
	c := dialFake(t, s)
	s.mu.Lock()
	s.down = append(s.down, []byte("tail")...)
	s.status = protocol.MTPStatusUpstreamClosed
	s.mu.Unlock()

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 32)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("buffered read: %v", err)
	}
	if string(buf[:n]) != "tail" {
		t.Fatalf("got %q, want tail", buf[:n])
	}
	if _, err := c.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("second read err = %v, want io.EOF", err)
	}
}

func TestTunnelLostTunnelIsReported(t *testing.T) {
	s := newFakeServer()
	c := dialFake(t, s)
	s.mu.Lock()
	s.status = protocol.MTPStatusNoTunnel
	s.mu.Unlock()

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 8)); !errors.Is(err, ErrTunnelLost) {
		t.Fatalf("err = %v, want ErrTunnelLost", err)
	}
}

func TestTunnelReadDeadline(t *testing.T) {
	c := dialFake(t, newFakeServer())
	c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	start := time.Now()
	_, err := c.Read(make([]byte, 8))
	if err == nil {
		t.Fatal("expected a timeout")
	}
	var te interface{ Timeout() bool }
	if !errors.As(err, &te) || !te.Timeout() {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("deadline took %v to fire", d)
	}
}

func TestTunnelCloseIsIdempotentAndUnblocksReaders(t *testing.T) {
	c := dialFake(t, newFakeServer())
	done := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 8)); done <- err }()
	time.Sleep(20 * time.Millisecond)

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read should fail after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wake the blocked reader")
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("write after close should fail")
	}
}

func TestTunnelRefusesUnusableChunkSize(t *testing.T) {
	s := newFakeServer()
	s.chunk = protocol.MTPMinChunk - 1
	_, err := DialTunnel(context.Background(), s.exchange, 2, TunnelOptions{})
	if err == nil {
		t.Fatal("a chunk size too small to make progress must be refused at dial")
	}
}

func TestTunnelOpenRefusalsAreReported(t *testing.T) {
	for _, st := range []byte{
		protocol.MTPStatusDisabled, protocol.MTPStatusBusy, protocol.MTPStatusBadDC,
	} {
		ex := func(ctx context.Context, p []byte) ([]byte, error) { return []byte{st}, nil }
		if _, err := DialTunnel(context.Background(), ex, 2, TunnelOptions{}); err == nil {
			t.Fatalf("status %d: dial should fail", st)
		}
	}
}

func TestTunnelRejectsBadDC(t *testing.T) {
	s := newFakeServer()
	for _, dc := range []int{0, -1, 256} {
		if _, err := DialTunnel(context.Background(), s.exchange, dc, TunnelOptions{}); err == nil {
			t.Fatalf("dc %d should be rejected before any query", dc)
		}
	}
}

// The UI counter must see every cell, both directions.
func TestTunnelReportsQueryCounts(t *testing.T) {
	s := newFakeServer()
	s.chunk = 4
	var mu sync.Mutex
	var cells, up, down int
	c, err := DialTunnel(context.Background(), s.exchange, 2, TunnelOptions{
		IdlePoll: time.Millisecond, MaxIdlePoll: 2 * time.Millisecond,
		OnQuery: func(u, d int) {
			mu.Lock()
			cells++
			up += u
			down += d
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s.queueDown([]byte("12345678"))
	if _, err := c.Write([]byte("abcdefghij")); err != nil { // 10 bytes / 4 = 3 cells
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	io.ReadFull(c, make([]byte, 8))
	// The first reply already carries the queued downstream, so the read can
	// finish while the upstream cells are still in flight. Wait on the counter
	// itself: the server records a cell on receipt, but the count under test
	// only moves when that cell's reply gets back here.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := up >= 10
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if cells < 3 {
		t.Fatalf("counted %d cells, want at least 3", cells)
	}
	if up != 10 {
		t.Fatalf("counted %d upstream bytes, want 10", up)
	}
	if down < 8 {
		t.Fatalf("counted %d downstream bytes, want at least 8", down)
	}
}

// The whole point: an arbitrary byte stream survives the tunnel intact, in both
// directions at once, at the smallest chunk size.
func TestTunnelCarriesStreamBothWays(t *testing.T) {
	s := newFakeServer()
	s.chunk = protocol.MTPMinChunk
	s.maxDown = 64
	c := dialFake(t, s)

	rng := rand.New(rand.NewSource(7))
	payload := make([]byte, 400)
	down := make([]byte, 400)
	rng.Read(payload)
	rng.Read(down)
	up := abridgedFrame(payload)
	s.queueDown(down)

	var wg sync.WaitGroup
	wg.Add(1)
	got := make([]byte, len(down))
	var readErr error
	go func() {
		defer wg.Done()
		c.SetReadDeadline(time.Now().Add(20 * time.Second))
		_, readErr = io.ReadFull(c, got)
	}()
	if _, err := c.Write(up); err != nil {
		t.Fatalf("write: %v", err)
	}
	wg.Wait()
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if !bytes.Equal(got, down) {
		t.Fatal("downstream corrupted")
	}
	if !bytes.Equal(s.received(), up) {
		t.Fatal("upstream corrupted")
	}
}

// When the relay reports the far end closed, the tunnel must drain what is
// buffered and then report EOF. It used to ignore the status and keep polling
// a dead connection forever, so the caller hung instead of failing.
func TestTunnelReportsUpstreamClosed(t *testing.T) {
	var step int
	ex := func(ctx context.Context, plain []byte) ([]byte, error) {
		step++
		switch step {
		case 1: // OPEN
			return protocol.BuildMTPOpenReply(protocol.MTPStatusOK, 8), nil
		default:
			// Hand back two bytes AND say the far end is gone.
			return protocol.BuildMTPReply(protocol.MTPReply{
				Status:  protocol.MTPStatusUpstreamClosed,
				UpAck:   0,
				DownSeq: 0,
				Down:    []byte{0xaa, 0xbb},
			}), nil
		}
	}

	conn, err := DialTunnel(context.Background(), ex, 2, TunnelOptions{
		Chunk: 8, IdlePoll: 5 * time.Millisecond, MaxIdlePoll: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// The buffered bytes still arrive...
	buf := make([]byte, 2)
	if n, err := io.ReadFull(conn, buf); err != nil || n != 2 {
		t.Fatalf("buffered downstream lost: n=%d err=%v", n, err)
	}
	if buf[0] != 0xaa || buf[1] != 0xbb {
		t.Fatalf("wrong bytes: %x", buf)
	}
	// ...and then the reader is told the far end is gone, rather than waiting.
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read hung on a closed upstream")
	}
}

// Stream offsets are 16 bits, compared by the half-window rule. A write larger
// than half that window would compare as already acknowledged and return with
// its bytes still unsent, so Write has to segment it.
func TestTunnelHugeWriteArrivesWhole(t *testing.T) {
	s := newFakeServer()
	s.maxDown = 600
	c := dialFake(t, s)
	// Comfortably past the half-window, and not a multiple of the segment or
	// chunk size, so a boundary bug shows up as missing or duplicated bytes.
	payload := make([]byte, 40*1024+4)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	msg := abridgedFrame(payload)
	n, err := c.Write(msg)
	if err != nil || n != len(msg) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if got := s.received(); !bytes.Equal(got, msg) {
		t.Fatalf("server got %d bytes, want %d (equal=%v)", len(got), len(msg), bytes.Equal(got, msg))
	}
}

// A status the client does not recognise has to stop the tunnel. Ignoring it
// leaves the client polling a tunnel the server has already given up on --
// and would make any status added later invisible to an old client.
func TestTunnelStopsOnAnUnknownStatus(t *testing.T) {
	s := newFakeServer()
	c := dialFake(t, s)

	s.mu.Lock()
	s.status = 99 // not a status this client knows
	s.mu.Unlock()

	// Any traffic now: the pump must fail rather than keep polling.
	_, err := c.Write([]byte("ping"))
	if err == nil {
		_, err = c.Read(make([]byte, 16))
	}
	if err == nil {
		t.Fatal("an unknown status was ignored")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error does not name the status: %v", err)
	}
}

// gotd's abridged codec writes one MTProto message as two calls: the length
// header, then the body. The header must NOT be flushed on its own -- the far
// end would hold an incomplete frame until the body finished crawling through,
// which is what Telegram closes a connection for.
func TestTunnelDoesNotFlushAFrameHeaderAlone(t *testing.T) {
	s := newFakeServer()
	c := dialFake(t, s)

	body := bytes.Repeat([]byte("x"), 320)
	prefix := []byte{byte(len(body) / 4)} // abridged: length in 4-byte words

	if _, err := c.Write(prefix); err != nil { // exactly how gotd writes it
		t.Fatalf("prefix: %v", err)
	}
	if ends := s.flushes(); ends != 0 {
		t.Fatalf("the frame header was flushed on its own (%d flushes)", ends)
	}
	if _, err := c.Write(body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if ends := s.flushes(); ends != 1 {
		t.Errorf("message delivered in %d pieces, want 1", ends)
	}
	want := append(append([]byte(nil), prefix...), body...)
	if got := s.received(); !bytes.Equal(got, want) {
		t.Errorf("far end got %d bytes, want %d", len(got), len(want))
	}
}

// The framing says where a message ends, so a message is flushed on its last
// byte however the caller chose to split it -- including one byte at a time.
func TestTunnelFlushesExactlyAtFrameEnds(t *testing.T) {
	s := newFakeServer()
	c := dialFake(t, s)

	frame := func(n int) []byte {
		b := []byte{byte(n / 4)}
		return append(b, bytes.Repeat([]byte("z"), n)...)
	}
	stream := append(frame(16), frame(8)...)

	for i := range stream { // the worst possible split
		if _, err := c.Write(stream[i : i+1]); err != nil {
			t.Fatalf("byte %d: %v", i, err)
		}
	}
	if ends := s.flushes(); ends != 2 {
		t.Errorf("%d flushes, want one per message (2)", ends)
	}
	if got := s.received(); !bytes.Equal(got, stream) {
		t.Errorf("far end got %d bytes, want %d", len(got), len(stream))
	}
}

// An incomplete message is held, because flushing it would hand the far end a
// fragment -- the thing that makes Telegram hang up.
func TestTunnelHoldsAnIncompleteMessage(t *testing.T) {
	s := newFakeServer()
	c := dialFake(t, s)

	// Announce 320 bytes, then send only part of them.
	if _, err := c.Write([]byte{80}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(bytes.Repeat([]byte("p"), 100)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if ends := s.flushes(); ends != 0 {
		t.Fatalf("a fragment was handed to the far end (%d flushes)", ends)
	}
	// The rest completes it, and only then does it go.
	if _, err := c.Write(bytes.Repeat([]byte("p"), 220)); err != nil {
		t.Fatal(err)
	}
	if ends := s.flushes(); ends != 1 {
		t.Errorf("%d flushes after completion, want 1", ends)
	}
}

// The framing gives a message's size before the first cell is sent, so the UI
// can show "cell 3 of 20" rather than a number that only climbs.
func TestTunnelReportsPerMessageProgress(t *testing.T) {
	s := newFakeServer()
	s.chunk = 16
	var mu sync.Mutex
	var seen []TunnelProgress
	c := dialFakeOpts(t, s, TunnelOptions{OnProgress: func(p TunnelProgress) {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	}})

	body := bytes.Repeat([]byte("m"), 320) // 20 cells of 16 bytes
	if _, err := c.Write(abridgedFrame(body)); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no progress reported")
	}
	// The first report ABOUT THIS MESSAGE, not the first report full stop: the
	// tunnel legitimately reports zeroes when nothing is in flight, and a
	// reply from before the write can arrive first. What matters is that the
	// size is known as soon as cells start moving, never counted upwards.
	first := -1
	for i, p := range seen {
		if p.TotalCells > 0 {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("no report ever carried a total: %+v", seen)
	}
	total := seen[first].TotalCells
	if total < 20 || total > 22 { // 321 bytes over 16-byte cells
		t.Errorf("first report about the message says %d cells total, want ~21", total)
	}
	// Known BEFORE the message is on its way, which is the whole point: a UI
	// that learns the size half way through has nothing to show until then.
	if seen[first].SentCells > 1 {
		t.Errorf("size only known after %d cells had gone out", seen[first].SentCells)
	}
	// It must count up to the total, not past it.
	last := seen[len(seen)-1]
	if last.SentCells > last.TotalCells && last.TotalCells != 0 {
		t.Errorf("sent %d of %d", last.SentCells, last.TotalCells)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].TotalCells != 0 && seen[i].SentCells < seen[i-1].SentCells {
			t.Errorf("progress went backwards: %+v then %+v", seen[i-1], seen[i])
		}
	}
}

// The relay says how much it still holds, so a long answer shows as bytes
// remaining instead of a spinner.
func TestTunnelReportsRemainingDownstream(t *testing.T) {
	s := newFakeServer()
	s.maxDown = 50
	var mu sync.Mutex
	var maxLeft int
	c := dialFakeOpts(t, s, TunnelOptions{OnProgress: func(p TunnelProgress) {
		mu.Lock()
		if p.DownLeft > maxLeft {
			maxLeft = p.DownLeft
		}
		mu.Unlock()
	}})
	s.queueDown(bytes.Repeat([]byte("d"), 400))
	buf := make([]byte, 400)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxLeft == 0 {
		t.Error("the relay never reported anything still waiting")
	}
}

// On a slow link the window is what decides throughput. Sending one cell at a
// time moves one chunk per round trip, so a message of twenty cells takes
// twenty round trips however generous the rate limit is; with a window it
// takes one. Measured as concurrency actually observed by the far end.
func TestTunnelWindowOverlapsQueries(t *testing.T) {
	const rtt = 200 * time.Millisecond

	run := func(window int) (time.Duration, int) {
		s := newFakeServer()
		s.chunk = 16
		s.latency = rtt
		c, err := DialTunnel(context.Background(), s.exchange, 2, TunnelOptions{
			Chunk: 16, IdlePoll: 10 * time.Millisecond,
			MaxIdlePoll: 50 * time.Millisecond, Window: window,
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		_ = c.SetWriteDeadline(time.Now().Add(60 * time.Second))

		start := time.Now()
		if _, err := c.Write(abridgedFrame(bytes.Repeat([]byte("w"), 320))); err != nil {
			t.Fatalf("write (window %d): %v", window, err)
		}
		return time.Since(start), s.peakConcurrency()
	}

	serial, serialPeak := run(1)
	windowed, windowedPeak := run(16)
	t.Logf("one at a time: %s (peak %d in flight) · windowed: %s (peak %d in flight)",
		serial.Round(time.Millisecond), serialPeak,
		windowed.Round(time.Millisecond), windowedPeak)

	if windowedPeak < 2 {
		t.Fatalf("the window never overlapped anything: peak %d", windowedPeak)
	}
	if windowed >= serial {
		t.Errorf("windowed took %s, no better than one at a time (%s)", windowed, serial)
	}
}

// The relay can forget a session -- it restarts, or sweeps an idle one. That
// is unrecoverable for the session, so the tunnel must give up AT ONCE and let
// the caller re-dial. Retrying is twelve rounds of waiting for an answer that
// cannot arrive, and with one session shared across dials it meant the
// connection never came back at all.
func TestLostSessionEndsTheTunnelImmediately(t *testing.T) {
	s := newFakeServer()
	c, err := DialTunnel(context.Background(), s.exchange, 2, TunnelOptions{
		IdlePoll: time.Millisecond, MaxIdlePoll: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if !c.fatalExchangeErr(ErrMTProtoSessionLost) {
		t.Fatal("a lost session must be fatal, not retried")
	}
	// A transient failure still gets its retries.
	if c.fatalExchangeErr(errors.New("dns exchange: i/o timeout")) {
		t.Fatal("a transient failure must still be retried")
	}
}

// gotd writes a message in pieces: the header lands before the body. The size
// has to be known from that header alone, or the progress bar shows nothing
// for the whole of a large upload and then jumps -- which over DNS is minutes
// of a bar that looks stuck.
func TestTunnelReportsProgressForASplitWrite(t *testing.T) {
	s := newFakeServer()
	s.chunk = 16
	var mu sync.Mutex
	var seen []TunnelProgress
	c := dialFakeOpts(t, s, TunnelOptions{OnProgress: func(p TunnelProgress) {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	}})

	frame := abridgedFrame(bytes.Repeat([]byte("m"), 320))
	// The header alone, with none of the body: the message's end is not in
	// these bytes at all, so the size can only come from the framing.
	if _, err := c.Write(frame[:1]); err != nil {
		t.Fatal(err)
	}

	// Reports are driven by the server's replies, so wait for one rather than
	// assuming it has already happened.
	deadline := time.Now().Add(3 * time.Second)
	best := 0
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, p := range seen {
			if p.TotalCells > best {
				best = p.TotalCells
			}
		}
		mu.Unlock()
		if best > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if best < 20 || best > 22 {
		t.Errorf("after the header alone the total was %d, want ~21: "+
			"a message whose size is only learnt once the body arrives shows "+
			"no progress for the whole upload", best)
	}
}

// A lossy link must not kill a tunnel that is moving data.
//
// The death budget counts consecutive failed batches, and a batch is a whole
// WINDOW that exchangeBatch calls failed if any one cell did. Reset it only on
// a flawless window and a link dropping one cell in three climbs the counter
// on batches that are delivering most of their cells.
//
// One in three is worse than any real link; the point is that loss alone, at
// any rate, must never end a tunnel that is making progress.
func TestTunnelSurvivesLossWhileMakingProgress(t *testing.T) {
	s := newFakeServer()
	s.chunk = 8     // many cells per message, so one write is many batches
	s.dropEvery = 3 // one query in three never arrives
	// A real window, as production uses (Window: cur.InFlight()). With the
	// harness default of one cell per batch the bug cannot appear at all: a
	// batch either wholly succeeds or wholly fails, so there is no such thing
	// as a batch that failed while still making progress.
	c := dialFakeWindow(t, s, 16)
	// The fake server accepts upstream strictly in order, so a window lands one
	// cell per round here. Far more time than it needs (about ten seconds):
	// the point is that the tunnel SURVIVES sustained loss, not how fast.
	//
	// KNOWN FLAKE, roughly 2 runs in 10 of the full -race suite and never when
	// run alone. It is not this deadline -- the failures land near 40s, and
	// that is what the retry ladder costs when the budget runs out (12 batches
	// at n*500ms, capped at 5s). Which cell of a 16-wide window gets dropped
	// depends on goroutine scheduling, because the harness drops every Nth
	// CALL rather than every Nth cell; on a loaded machine the window's
	// leading cell can lose repeatedly, and no progress means no budget reset.
	// Keying the drop to the cell's sequence number instead would make it
	// deterministic under any load.
	_ = c.SetWriteDeadline(time.Now().Add(180 * time.Second))

	msg := abridgedFrame(bytes.Repeat([]byte("abcdefgh"), 40))
	n, err := c.Write(msg)
	if err != nil {
		t.Fatalf("a link losing a third of its queries killed the tunnel: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("wrote %d of %d bytes", n, len(msg))
	}
	if got := s.received(); !bytes.Equal(got, msg) {
		t.Fatalf("server got %d bytes, want %d — the stream was corrupted, not just slow",
			len(got), len(msg))
	}
	// It has to have been a real test of the budget, not a lucky clean run.
	if dropped := s.dropped(); dropped <= mtpBatchRetries {
		t.Fatalf("only %d queries were dropped; the budget of %d was never under pressure",
			dropped, mtpBatchRetries)
	}
	// Still usable afterwards: the budget was cleared, not merely survived.
	if _, err := c.Write(abridgedFrame([]byte("and another"))); err != nil {
		t.Fatalf("second write after sustained loss: %v", err)
	}
}

// The budget still has to exist: a link that delivers nothing must be given up
// on rather than retried for ever.
//
// Tested at the counter rather than through a write, because exhausting it for
// real means sitting through the whole backoff ladder -- about forty seconds of
// sleeping that proves nothing the counter does not.
func TestTunnelRetryBudgetStillEnds(t *testing.T) {
	s := newFakeServer()
	c := dialFake(t, s)
	c.mu.Lock()
	c.batchFails = mtpBatchRetries
	c.mu.Unlock()
	// One past the budget: refused outright, with no backoff to wait through.
	if c.retryBatch() {
		t.Fatal("past the budget the tunnel must be given up on, not retried for ever")
	}
	// And progress must genuinely clear it, or the fix above is a no-op.
	c.resetRetries()
	c.mu.Lock()
	n := c.batchFails
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("batchFails = %d after a reset, want 0", n)
	}
}

// A reply that will not open must not kill the session.
//
// Over DNS a bad answer is ordinary: a truncated UDP reply, a resolver handing
// back something cached under this query name. Treating one of those as "the
// relay has forgotten us" invalidated the session, which makes every later
// cell fail at once and forces a whole new key exchange — tens of round trips
// thrown away over one garbled answer. The relay says a session is lost in so
// many words; that, and only that, is fatal.
func TestBadReplyIsRetryableNotSessionLoss(t *testing.T) {
	if errors.Is(ErrMTProtoBadReply, ErrMTProtoSessionLost) {
		t.Fatal("a bad reply must not be classified as session loss")
	}
	c := &TunnelConn{}
	if c.fatalExchangeErr(fmt.Errorf("wrapped: %w", ErrMTProtoBadReply)) {
		t.Error("a bad reply must be retryable: the cell is idempotent and can go again")
	}
	// The genuinely terminal ones must stay terminal.
	for _, err := range []error{ErrMTProtoSessionLost, ErrTunnelLost, ErrTunnelClosed} {
		if !c.fatalExchangeErr(err) {
			t.Errorf("%v must stay fatal", err)
		}
	}
}

// The same thing at the call site, not just in the classification: a session
// that gets one unopenable answer must stay live, so the next cell goes out
// over the key exchange it already paid for.
func TestSessionSurvivesAnUnopenableReply(t *testing.T) {
	info := &protocol.MTPInfo{
		Enabled: true, Domains: []string{"m.example.com"},
		EkPub:      make([]byte, 32),
		MinVersion: protocol.MTProtoVersion, MaxVersion: protocol.MTProtoVersion,
	}
	var qk [protocol.KeySize]byte
	s, err := NewMTProtoSession(
		// Answers every cell with bytes that will not open under any key.
		func(ctx context.Context, qname string) ([]byte, error) {
			return bytes.Repeat([]byte{0xAB}, 48), nil
		},
		qk, protocol.QuerySingleLabel, info, protocol.ChatCellPlainSize)
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for a completed handshake.
	s.mu.Lock()
	s.live, s.chunk = true, 8
	s.mu.Unlock()

	_, err = s.Exchange(context.Background(), protocol.BuildMTPPoll(0))
	if err == nil {
		t.Fatal("an unopenable reply must still be an error")
	}
	if !errors.Is(err, ErrMTProtoBadReply) {
		t.Fatalf("err = %v, want a retryable bad-reply error", err)
	}
	if !s.Live() {
		t.Fatal("one garbled answer killed the session — every later cell now fails " +
			"at once and the whole key exchange has to be paid for again")
	}
}

// A writer parked on an unacknowledged message must be woken when the far end
// closes.
//
// writeSegment waits for the ack that advances outOff, and the pump is the only
// thing that can produce one. Its upstream-EOF exit returned without setting an
// error and without a broadcast, so the writer went on waiting for an ack that
// could never come: blocked until its deadline, or for ever with none set.
func TestUpstreamCloseWakesAParkedWriter(t *testing.T) {
	s := newFakeServer()
	s.chunk = 8
	// A window with loss on it, so the close arrives among cells that are
	// failing rather than on a quiet link.
	c := dialFakeWindow(t, s, 4)
	// Long enough that a deadline cannot be what rescues this.
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))

	s.mu.Lock()
	s.status = protocol.MTPStatusUpstreamClosed
	s.dropEvery = 2 // every other cell in the window fails outright
	s.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := c.Write(abridgedFrame(bytes.Repeat([]byte("x"), 64)))
		done <- err
	}()

	select {
	case err := <-done:
		// io.EOF specifically: the far end closed, which is a fact about the
		// stream. Any other error means the close was dropped and the writer
		// was eventually rescued by the retry budget running out instead --
		// which takes the whole back-off ladder, and leaves the caller with a
		// link error for what was a clean end of stream.
		// io.EOF specifically: a clean end of stream, not a link error. The
		// pump decides `closed` before anything else for this reason — a
		// window holds many cells, so one can report the close while another
		// fails, and handling the error first could drop the close.
		if !errors.Is(err, io.EOF) {
			t.Fatalf("write ended with %v, want io.EOF", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the writer was never woken: it is waiting for an ack that cannot " +
			"arrive, and nothing is left running to produce one")
	}
}

// A write that times out must end the tunnel, not leave a truncated frame in
// the stream.
//
// The bytes are appended to the out buffer before the wait, and some may
// already be on the wire, so "wrote 0 bytes" is not something the stream can
// be held to: they cannot be taken back, and a caller that believes nothing
// was written and sends them again puts them in twice. This path is also only
// reached mid-frame — the backlog cap is what sends a half-written message
// down it — so what is committed is a truncated MTProto frame the far end can
// never parse. The only honest outcome is a dead tunnel and a fresh dial.
func TestWriteTimeoutEndsTheTunnel(t *testing.T) {
	s := newFakeServer()
	s.chunk = 8
	c := dialFake(t, s)

	// Nothing will ever be acknowledged, so the write cannot complete.
	s.mu.Lock()
	s.dropEvery = 1
	s.mu.Unlock()

	_ = c.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	n, err := c.Write(abridgedFrame(bytes.Repeat([]byte("z"), 64)))
	if err == nil {
		t.Fatal("the write should have timed out")
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 alongside the error", n)
	}

	// The connection must now be dead: anything else means a later write
	// would append after a half-written frame.
	c.mu.Lock()
	connErr := c.err
	c.mu.Unlock()
	if connErr == nil {
		t.Fatal("the tunnel is still open after a mid-frame write timeout — the next " +
			"write appends to a stream the far end can no longer parse")
	}
	if _, err := c.Write(abridgedFrame([]byte("more"))); err == nil {
		t.Fatal("a later write was accepted onto a broken stream")
	}
}
