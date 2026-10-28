package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// TunnelConn is a net.Conn carried by thefeed's DNS transport. gotd dials
// through it unmodified, so the whole MTProto stack — handshake, TL schema,
// layer negotiation — runs on top without a single patch.
//
// One pump goroutine owns the wire: it drains pending writes a cell at a time
// and feeds replies into the read buffer. Downstream bytes ride back on the
// reply to every cell, so while a send is in flight the inbound direction costs
// nothing extra; only an idle connection pays for separate POLL cells.

// TunnelExchange performs one sealed cell round trip and returns the reply
// plaintext. Injected so the stream can be tested without DNS or Telegram.
type TunnelExchange func(ctx context.Context, plaintext []byte) ([]byte, error)

// TunnelProgress is how far the message being sent has got, and what is still
// waiting to come back.
type TunnelProgress struct {
	// SentCells / TotalCells describe the message being uploaded. TotalCells
	// is 0 between messages, when nothing is being sent.
	SentCells  int
	TotalCells int
	// DownLeft is what the server still holds for us.
	DownLeft int
}

// TunnelOptions configures a tunnel. Zero values pick the defaults.
type TunnelOptions struct {
	// Chunk is the stream bytes per upstream cell, from the OPEN reply.
	Chunk int
	// IdlePoll is the wait between POLL cells when there is nothing to send.
	// Kept short while the far end may still answer, then backed off.
	IdlePoll time.Duration
	// MaxIdlePoll bounds that back-off.
	MaxIdlePoll time.Duration
	// Window is how many cells may be in flight at once, so the send window
	// can never grow without bound.
	Window int
	// OnProgress reports where the current message stands: cells sent of the
	// cells it needs, and how much the server still holds for us. The frame
	// length gives the first two before a single cell goes out, so the UI can
	// show a real bar instead of a counter that only climbs.
	OnProgress func(TunnelProgress)
	// OnQuery reports every cell as it is sent, for the UI counter: up and down
	// are the stream bytes moved by that cell.
	OnQuery func(up, down int)
}

// ErrTunnelClosed is returned once the tunnel is torn down.
var ErrTunnelClosed = errors.New("mtproto tunnel: closed")

// ErrTunnelLost means the server forgot the tunnel; the MTProto session on top
// is dead and the caller must re-dial.
var ErrTunnelLost = errors.New("mtproto tunnel: server lost the tunnel")

type tunnelAddr struct{ s string }

func (a tunnelAddr) Network() string { return "thefeed-dns" }
func (a tunnelAddr) String() string  { return a.s }

// TunnelConn implements net.Conn over the tunnel cell protocol.
type TunnelConn struct {
	// flushAt holds the stream offsets where the callers writes ended, so the
	// cell carrying each last byte can ask the relay to flush (guarded by mu).
	flushAt []uint16
	// upstreamEOF: the relay reported the far end closed. Read drains what is
	// buffered and then returns io.EOF (guarded by mu).
	upstreamEOF bool
	ex          TunnelExchange
	opts        TunnelOptions
	dc          int
	chunk       int

	mu   sync.Mutex
	cond *sync.Cond

	out    []byte // pending upstream, out[0] sits at stream offset outOff
	outOff uint16
	in     []byte // downstream received but not yet Read
	inOff  uint16 // stream offset of the next byte we expect

	err      error // sticky: first failure or close
	closed   bool
	wakePump chan struct{}
	pumpDone chan struct{}
	// live is cancelled when the tunnel closes. The pump's exchanges hang off
	// it, so a poll already queued behind a rate limit is abandoned instead of
	// spending a query on a connection nobody is reading -- which on a slow
	// link is a query the live connection needed.
	live     context.Context
	stopLive context.CancelFunc

	readDeadline  time.Time
	writeDeadline time.Time
	deadlineTimer *time.Timer

	// batchFails counts consecutive failed batches, so a lossy link is
	// retried but a dead one is still given up on.
	batchFails int
	// frames tracks where each message ends, so the relay is told to flush a
	// complete one and never a fragment. lastWrite backs the stall guard.
	frames    frameScanner
	lastWrite time.Time
	// msgStart / msgEnd bracket the message being uploaded, so progress is
	// measured against a total known before the first cell is sent.
	msgStart uint16
	msgEnd   uint16
	msgLive  bool
	downLeft int
}

// DialTunnel opens a tunnel to a Telegram DC through the server and returns it
// as a net.Conn. The server dials only known DCs, so it can never be driven to
// connect anywhere else.
func DialTunnel(ctx context.Context, ex TunnelExchange, dc int, opts TunnelOptions) (*TunnelConn, error) {
	if dc <= 0 || dc > 255 {
		return nil, fmt.Errorf("mtproto tunnel: bad dc %d", dc)
	}
	reply, err := ex(ctx, protocol.BuildMTPOpen(uint8(dc)))
	if err != nil {
		return nil, fmt.Errorf("mtproto tunnel: open: %w", err)
	}
	status, chunk, err := protocol.ParseMTPOpenReply(reply)
	if err != nil {
		return nil, fmt.Errorf("mtproto tunnel: open reply: %w", err)
	}
	if status != protocol.MTPStatusOK {
		return nil, openStatusError(status)
	}
	if int(chunk) < protocol.MTPMinChunk {
		// Refuse rather than crawl: below this the header dominates so heavily
		// that an MTProto handshake would never finish.
		return nil, fmt.Errorf("mtproto tunnel: server offered %d-byte chunks, need at least %d",
			chunk, protocol.MTPMinChunk)
	}

	c := &TunnelConn{ex: ex, opts: opts, dc: dc, chunk: int(chunk)}
	if opts.Chunk > 0 && opts.Chunk < c.chunk {
		c.chunk = opts.Chunk
	}
	if c.opts.IdlePoll <= 0 {
		c.opts.IdlePoll = 500 * time.Millisecond
	}
	if c.opts.MaxIdlePoll <= 0 {
		c.opts.MaxIdlePoll = 10 * time.Second
	}
	c.cond = sync.NewCond(&c.mu)
	c.wakePump = make(chan struct{}, 1)
	c.pumpDone = make(chan struct{})
	// Detached from the dial context, which ends when the dial returns, but
	// cancelled by Close.
	c.live, c.stopLive = context.WithCancel(context.WithoutCancel(ctx))
	go c.pump()
	return c, nil
}

func openStatusError(status byte) error {
	switch status {
	case protocol.MTPStatusDisabled:
		return errors.New("mtproto tunnel: this server has the bridge turned off")
	case protocol.MTPStatusBusy:
		return errors.New("mtproto tunnel: server is at capacity, try again later")
	case protocol.MTPStatusBadDC:
		return errors.New("mtproto tunnel: server refused the data centre")
	default:
		return fmt.Errorf("mtproto tunnel: open refused (status %d)", status)
	}
}

// kick wakes the pump without blocking if a wake is already queued.
func (c *TunnelConn) kick() {
	select {
	case c.wakePump <- struct{}{}:
	default:
	}
}

// pump owns the wire: one cell at a time, forever, until closed.
func (c *TunnelConn) pump() {
	defer close(c.pumpDone)
	idle := c.opts.IdlePoll
	for {
		c.mu.Lock()
		if c.closed || c.err != nil {
			c.mu.Unlock()
			return
		}
		// Nothing left to say to a far end that has closed: stop polling once
		// the buffered downstream has been handed to the reader.
		if c.upstreamEOF && len(c.in) == 0 {
			c.mu.Unlock()
			// Wake anyone parked in writeSegment before going. They are
			// waiting for an ack that advances outOff, and this goroutine is
			// the only thing that can produce one -- so leaving quietly left a
			// writer blocked until its deadline, or for ever when none was
			// set. The far end is gone, so the pending bytes never will be
			// acknowledged: io.EOF, the same end-of-stream the drain path
			// reports.
			c.fail(io.EOF)
			return
		}
		// Cells are taken WITHOUT consuming them: they stay pending until the
		// server acks, so a lost one is simply re-sent at the same offset.
		batch := c.nextBatchLocked()
		c.mu.Unlock()

		sent := time.Now()
		moved, closed, err := c.exchangeBatch(batch)
		rtt := time.Since(sent)
		// The far end closing is decided FIRST, because it is a fact about the
		// stream rather than about this batch. A window holds many cells, so
		// one of them can report the close while another fails on the link;
		// the error branch below then retried and the close was dropped on the
		// floor. The pump went round again, found upstreamEOF already set with
		// nothing buffered, and returned without failing the connection -- so a
		// writer parked waiting for an ack that could no longer come was left
		// with nothing to wake it.
		if closed {
			// Deliver whatever came back with those cells, then report a clean
			// end of stream — io.EOF, so gotd sees a normal close.
			c.resetRetries()
			c.fail(io.EOF)
			return
		}
		if err != nil {
			// A failed cell is not a dead tunnel: each carries its own stream
			// offset, so re-sending is idempotent and the relay re-acks. On a
			// lossy link cells WILL fail, and tearing down for one of them
			// restarts the key exchange from nothing.
			//
			// So the retry budget counts batches that achieved NOTHING, not
			// batches containing a failure: a window of sixteen cells rarely
			// comes back clean at a few percent loss, yet it is moving the
			// stream. Progress clears the budget; the failed cells are still
			// pending and go out next round with no back-off.
			if moved && !c.fatalExchangeErr(err) {
				c.resetRetries()
				continue
			}
			if !c.fatalExchangeErr(err) && c.retryBatch() {
				continue
			}
			c.fail(err)
			return
		}
		c.resetRetries()

		// A poll carries nothing and still costs a query. On a rate-limited
		// link that query is taken from the ones the stream needs, so the
		// floor tracks what a round trip actually costs: polling faster than
		// the link answers only starves the data.
		floor := c.opts.IdlePoll
		if rtt > floor {
			floor = rtt
			if floor > c.opts.MaxIdlePoll {
				floor = c.opts.MaxIdlePoll
			}
		}
		if moved {
			idle = floor // activity: poll briskly again
			continue
		}
		if idle < floor {
			idle = floor
		}
		// Guarantee against a misparse: end the tunnel rather than block on a
		// message that is never completed.
		if c.failIfStalled() {
			return
		}
		// Nothing moved in either direction — back off before polling again.
		select {
		case <-c.wakePump:
			idle = floor
		case <-time.After(idle):
			if idle < c.opts.MaxIdlePoll {
				idle *= 2
			}
		}
	}
}

// mtpBatchRetries is how many consecutive failed batches are tolerated before
// the tunnel is declared dead. Spread over a per-cell retry of its own, this
// is far past any plausible run of bad luck on a lossy link.
const mtpBatchRetries = 12

// errMTPRefused is the server declining, which no amount of retrying fixes.
var errMTPRefused = errors.New("mtproto tunnel: server refused")

// fatalExchangeErr reports whether an error means the tunnel itself is gone,
// as opposed to one cell not getting through.
func (c *TunnelConn) fatalExchangeErr(err error) bool {
	return errors.Is(err, ErrTunnelLost) || errors.Is(err, ErrTunnelClosed) ||
		errors.Is(err, errMTPRefused) || errors.Is(err, errMTPIncompleteMessage) ||
		// A lost session never comes back: the relay has forgotten the keys,
		// and the client marks it dead so every later cell fails at once.
		// Retrying one is twelve rounds of waiting for an answer that cannot
		// arrive, delaying the re-dial that actually fixes it.
		errors.Is(err, ErrMTProtoSessionLost) ||
		errors.Is(err, context.Canceled)
}

// retryBatch counts a failed batch and reports whether to try again, backing
// off a little so a link that is briefly gone is not hammered.
func (c *TunnelConn) retryBatch() bool {
	c.mu.Lock()
	c.batchFails++
	n := c.batchFails
	closed := c.closed || c.err != nil
	c.mu.Unlock()
	if closed || n > mtpBatchRetries {
		return false
	}
	wait := time.Duration(n) * 500 * time.Millisecond
	if wait > 5*time.Second {
		wait = 5 * time.Second
	}
	select {
	case <-time.After(wait):
	case <-c.live.Done():
		return false
	}
	return true
}

func (c *TunnelConn) resetRetries() {
	c.mu.Lock()
	c.batchFails = 0
	c.mu.Unlock()
}

// cell is one query waiting to go out.
type cell struct {
	plain []byte
	chunk int
}

// nextBatchLocked builds the cells to send together: as much pending upstream
// as the window allows, or a single poll when there is nothing to send.
//
// The window is what makes a slow link usable. One cell at a time moves one
// chunk per round trip, so a five-second round trip moves sixteen bytes every
// five seconds however fast the link is allowed to go; with the window the
// rate limit sets the pace instead of the latency. Caller holds c.mu.
func (c *TunnelConn) nextBatchLocked() []cell {
	window := c.opts.Window
	if window < 1 {
		window = 1
	}
	if len(c.out) == 0 {
		return []cell{{plain: protocol.BuildMTPPoll(c.inOff)}}
	}
	var batch []cell
	for off := 0; off < len(c.out) && len(batch) < window; off += c.chunk {
		n := min(c.chunk, len(c.out)-off)
		upSeq := c.outOff + uint16(off)
		end := upSeq + uint16(n)
		// A message ends inside this cell when a boundary falls in it. The
		// relay flushes once the stream is contiguous through that point, so
		// the cells may still arrive in any order.
		ends := false
		for _, at := range c.flushAt {
			if protocol.MTPSeqDelta(upSeq, at) > 0 && protocol.MTPSeqDelta(at, end) >= 0 {
				ends = true
				break
			}
		}
		chunk := append([]byte(nil), c.out[off:off+n]...)
		plain := protocol.BuildMTPData(upSeq, c.inOff, chunk)
		if ends {
			plain = protocol.BuildMTPDataEnd(upSeq, c.inOff, chunk)
		}
		batch = append(batch, cell{plain: plain, chunk: n})
	}
	return batch
}

// exchangeBatch sends the cells together and folds in every answer. Replies
// land in any order; acks are cumulative, so applying them as they arrive
// reaches the same place.
func (c *TunnelConn) exchangeBatch(batch []cell) (moved, closed bool, err error) {
	type result struct {
		r     *protocol.MTPReply
		chunk int
		err   error
	}
	results := make(chan result, len(batch))
	for _, cl := range batch {
		go func(cl cell) {
			reply, e := c.ex(c.live, cl.plain)
			if e != nil {
				results <- result{chunk: cl.chunk, err: fmt.Errorf("mtproto tunnel: %w", e)}
				return
			}
			r, pe := protocol.ParseMTPReply(reply)
			if pe != nil {
				results <- result{chunk: cl.chunk, err: fmt.Errorf("mtproto tunnel: reply: %w", pe)}
				return
			}
			results <- result{r: r, chunk: cl.chunk}
		}(cl)
	}
	// Every cell is waited for, so the window never runs ahead of itself and
	// the next batch is built from a settled view of the stream.
	for range batch {
		res := <-results
		if res.err != nil {
			if err == nil {
				err = res.err
			}
			continue
		}
		switch res.r.Status {
		case protocol.MTPStatusOK:
		case protocol.MTPStatusUpstreamClosed:
			closed = true
		case protocol.MTPStatusNoTunnel:
			if err == nil {
				err = ErrTunnelLost
			}
			continue
		default:
			// Including a status this client does not know: a newer server
			// must be able to stop an old client, not leave it polling a
			// tunnel that will never answer.
			if err == nil {
				err = fmt.Errorf("%w (status %d)", errMTPRefused, res.r.Status)
			}
			continue
		}
		if c.apply(res.r, res.chunk) {
			moved = true
		}
		if c.opts.OnQuery != nil {
			c.opts.OnQuery(res.chunk, len(res.r.Down))
		}
		c.reportProgress(res.r)
	}
	return moved, closed, err
}

// failIfStalled ends a connection whose caller stopped mid-message.
//
// A message is only held back because the rest is coming; if it never does,
// the far end is waiting for bytes that will never be sent and the tunnel is
// finished either way. Ending it says so, which gotd acts on, instead of
// leaving the caller blocked for ever. With real framing this never fires:
// a message is incomplete only while a write is in progress.
func (c *TunnelConn) failIfStalled() bool {
	c.mu.Lock()
	stalled := c.frames.midFrame() && len(c.out) == 0 && !c.lastWrite.IsZero() &&
		time.Since(c.lastWrite) >= mtpStallGuard
	c.mu.Unlock()
	if !stalled {
		return false
	}
	c.fail(errMTPIncompleteMessage)
	return true
}

// reportProgress publishes how far the current message has got.
func (c *TunnelConn) reportProgress(r *protocol.MTPReply) {
	if c.opts.OnProgress == nil {
		return
	}
	c.mu.Lock()
	var p TunnelProgress
	p.DownLeft = int(r.DownLeft)
	if c.msgLive {
		total := protocol.MTPSeqDelta(c.msgStart, c.msgEnd)
		done := protocol.MTPSeqDelta(c.msgStart, c.outOff)
		if total > 0 {
			p.TotalCells = (total + c.chunk - 1) / c.chunk
			if done < 0 {
				done = 0
			}
			if done > total {
				done = total
			}
			p.SentCells = (done + c.chunk - 1) / c.chunk
		}
		// Finished: the next header starts a new message.
		if protocol.MTPSeqDelta(c.msgEnd, c.outOff) >= 0 {
			c.msgLive = false
		}
	}
	c.mu.Unlock()
	c.opts.OnProgress(p)
}

// apply folds one reply into the buffers and reports whether anything moved.
func (c *TunnelConn) apply(r *protocol.MTPReply, sent int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	moved := false

	// Upstream: drop the bytes the server took. Only ever forward, so a stale
	// reply from a retried cell cannot rewind the stream.
	if adv := protocol.MTPSeqDelta(c.outOff, r.UpAck); adv > 0 && adv <= len(c.out) {
		c.out = c.out[adv:]
		c.outOff = r.UpAck
		// Forget message boundaries the stream has passed.
		for len(c.flushAt) > 0 && protocol.MTPSeqDelta(c.flushAt[0], c.outOff) >= 0 {
			c.flushAt = c.flushAt[1:]
		}
		moved = true
	}

	// Downstream: accept the slice that starts where we are. A retransmit that
	// starts earlier is trimmed; a gap is dropped and re-requested, since the
	// next cell still carries the old inOff.
	if len(r.Down) > 0 {
		if skip := protocol.MTPSeqDelta(r.DownSeq, c.inOff); skip >= 0 && skip < len(r.Down) {
			c.in = append(c.in, r.Down[skip:]...)
			c.inOff += uint16(len(r.Down) - skip)
			moved = true
		}
	}
	// The far end hung up. Everything buffered above has been taken, so Read
	// drains and then reports EOF rather than polling a dead tunnel.
	if r.Status == protocol.MTPStatusUpstreamClosed {
		c.upstreamEOF = true
		moved = true
	}
	if moved {
		c.cond.Broadcast()
	}
	return moved
}

// fail records the terminating condition and wakes every blocked caller. A nil
// cause means a clean end of stream.
func (c *TunnelConn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		if err == nil {
			err = ErrTunnelClosed
		}
		c.err = err
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// Read implements net.Conn. Buffered downstream bytes are always returned
// before any error, so a connection that ends mid-reply loses nothing.
func (c *TunnelConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if len(c.in) > 0 {
			n := copy(b, c.in)
			c.in = c.in[n:]
			return n, nil
		}
		if c.err != nil {
			return 0, c.err
		}
		if c.upstreamEOF {
			return 0, io.EOF
		}
		if expired(c.readDeadline) {
			return 0, timeoutError{}
		}
		c.cond.Wait()
	}
}

// mtpMaxWriteSegment bounds one pass of writeSegment. Stream offsets are 16
// bits and compared by the half-window rule, which only has meaning while the
// two sides are less than 32 KiB apart: a single write past that would compare
// as already acknowledged and return before its bytes had left.
const mtpMaxWriteSegment = 8 << 10

// abridgedTag opens an abridged stream.
const abridgedTag = 0xef

// mtpMaxFrame is the largest abridged frame treated as plausible. Past it the
// stream is not the framing this scanner expects, and it stops guessing.
const mtpMaxFrame = 1 << 20

// errMTPIncompleteMessage ends a tunnel whose caller stopped mid-message.
var errMTPIncompleteMessage = errors.New("mtproto tunnel: caller stopped mid-message")

// mtpStallGuard is how long an incomplete message may sit with the caller
// silent before the tunnel gives up on it. Not how boundaries are found --
// the framing gives those exactly -- only insurance against hanging for ever
// if the stream is ever not the framing expected.
//
// Generous on purpose: firing it is FATAL to the tunnel, and on a link
// measured in queries per second ten seconds is one round trip, not evidence.
const mtpStallGuard = 3 * time.Minute

// mtpWriteBacklog caps bytes buffered while a frame is still incomplete. A
// partial frame returns at once so the rest can follow, but a caller that
// never completes one must still meet backpressure.
const mtpWriteBacklog = 16 << 10

// frameScanner finds where each message ends.
//
// The tunnel exposes a byte stream, which has no message boundaries -- but the
// relay needs them, because Telegram drops a connection left holding half a
// message. The stream says so itself: gotd's abridged codec writes a length
// header (1 byte, or 0x7f plus 3 little-endian bytes, each counting 4-byte
// words) and then exactly that many bytes. Reading the header gives the exact
// end of every message, with nothing to guess.
type frameScanner struct {
	tagSeen bool   // the 1-byte transport tag opens the stream
	hdr     []byte // header bytes seen so far
	need    int    // body bytes still expected; 0 means a header comes next
	broken  bool   // the stream is not this framing after all
}

// scan consumes b, whose first byte sits at stream offset base, and returns
// the offset just past each message that ends inside it.
func (f *frameScanner) scan(b []byte, base uint16) []uint16 {
	if f.broken {
		return nil
	}
	var ends []uint16
	for i := 0; i < len(b); {
		switch {
		case !f.tagSeen:
			// The abridged transport opens with a one-byte tag, which is not a
			// message: it rides along with the first one. Recognised by value
			// rather than by position, so a stream without it still parses.
			f.tagSeen = true
			if b[i] == abridgedTag {
				i++
			}
		case f.need > 0:
			take := len(b) - i
			if take > f.need {
				take = f.need
			}
			i += take
			f.need -= take
			if f.need == 0 {
				ends = append(ends, base+uint16(i))
			}
		case len(f.hdr) == 0 && b[i] != 0x7f:
			f.need = int(b[i]) * 4
			i++
			if f.need == 0 {
				ends = append(ends, base+uint16(i)) // an empty frame still ends
			}
		default:
			f.hdr = append(f.hdr, b[i])
			i++
			if len(f.hdr) == 4 {
				f.need = (int(f.hdr[1]) | int(f.hdr[2])<<8 | int(f.hdr[3])<<16) * 4
				f.hdr = f.hdr[:0]
				if f.need > mtpMaxFrame {
					f.broken = true
					return ends
				}
			}
		}
	}
	return ends
}

// midFrame reports whether a message is still incomplete.
func (f *frameScanner) midFrame() bool { return !f.broken && (f.need > 0 || len(f.hdr) > 0) }

// Write implements net.Conn. It blocks until the server has acknowledged every
// byte, which is the backpressure that keeps a slow DNS uplink from building an
// unbounded queue behind a fast MTProto caller.
func (c *TunnelConn) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		end := written + mtpMaxWriteSegment
		if end > len(b) {
			end = len(b)
		}
		n, err := c.writeSegment(b[written:end])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// writeSegment queues one segment. It waits for the acknowledgement unless the
// segment leaves a message incomplete, which returns at once so the rest can
// be queued behind it and the whole message travels as one.
func (c *TunnelConn) writeSegment(b []byte) (int, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return 0, err
	}
	// Where these bytes land in the stream, so a message end is an absolute
	// offset the pump can compare against.
	base := c.outOff + uint16(len(c.out))
	c.out = append(c.out, b...)
	target := c.outOff + uint16(len(c.out))
	// The stream itself says where each message ends.
	ends := c.frames.scan(b, base)
	// A message's extent is known the moment its header is read: progress can
	// be shown against a total rather than counted upwards for ever.
	if !c.msgLive && (len(ends) > 0 || c.frames.midFrame()) {
		c.msgStart, c.msgLive = base, true
	}
	if len(ends) > 0 {
		c.msgEnd = ends[len(ends)-1]
	} else if c.msgLive {
		c.msgEnd = base + uint16(len(b)) + uint16(c.frames.need)
	}
	c.flushAt = append(c.flushAt, ends...)
	c.lastWrite = time.Now()
	// A scanner that lost the framing falls back to the caller's own write
	// boundary, which is the best guess left.
	if c.frames.broken {
		c.flushAt = append(c.flushAt, target)
	}
	incomplete := c.frames.midFrame()
	backlog := len(c.out)
	c.mu.Unlock()
	c.kick()

	// Mid-message: return so the rest can be queued behind it and the whole
	// message leaves together, instead of the far end holding a fragment.
	if incomplete && backlog < mtpWriteBacklog {
		return len(b), nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if protocol.MTPSeqDelta(c.outOff, target) <= 0 {
			return len(b), nil
		}
		if c.err != nil {
			return 0, c.err
		}
		if expired(c.writeDeadline) {
			// These bytes are in c.out and some are on the wire, so "wrote 0"
			// would be a lie: a caller that resends them duplicates them. And
			// the backlog cap sends half-written messages down here, so what
			// is committed is a frame the far end can never parse.
			//
			// Nothing honest is left, so the tunnel ends; gotd dials a new one
			// and restarts the message on a clean stream.
			c.err = errMTPIncompleteMessage
			c.cond.Broadcast()
			return 0, timeoutError{}
		}
		c.cond.Wait()
	}
}

// Close implements net.Conn. The CLOSE cell is best-effort: the tunnel is gone
// locally either way, and the server expires it on its own.
// CloseLocal tears the tunnel down WITHOUT telling the relay.
//
// For the case where the next thing we do is open another tunnel on the same
// session: the relay replaces a session's tunnel on open and closes the old
// one itself, so a CLOSE here would spend a query saying what the open is
// about to say anyway.
func (c *TunnelConn) CloseLocal() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.err == nil {
		c.err = ErrTunnelClosed
	}
	c.cond.Broadcast()
	c.mu.Unlock()

	c.stopLive()
	c.kick()
	<-c.pumpDone
}

func (c *TunnelConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.err == nil {
		c.err = ErrTunnelClosed
	}
	c.cond.Broadcast()
	c.mu.Unlock()

	// Drop any exchange the pump has queued before waiting for it: on a
	// rate-limited link that wait is otherwise seconds, and the query it
	// finally spends is for a connection nobody will read.
	c.stopLive()
	c.kick()
	<-c.pumpDone

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = c.ex(ctx, protocol.BuildMTPClose())
	return nil
}

func (c *TunnelConn) LocalAddr() net.Addr  { return tunnelAddr{"thefeed"} }
func (c *TunnelConn) RemoteAddr() net.Addr { return tunnelAddr{fmt.Sprintf("telegram-dc%d", c.dc)} }

func (c *TunnelConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline, c.writeDeadline = t, t
	c.armDeadlineLocked()
	return nil
}

func (c *TunnelConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.armDeadlineLocked()
	return nil
}

func (c *TunnelConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	c.armDeadlineLocked()
	return nil
}

// armDeadlineLocked schedules a wake-up at the nearest deadline. Without it a
// blocked Read would sit in cond.Wait past its deadline, since nothing else
// would signal.
func (c *TunnelConn) armDeadlineLocked() {
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
		c.deadlineTimer = nil
	}
	next := time.Time{}
	for _, t := range []time.Time{c.readDeadline, c.writeDeadline} {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if next.IsZero() {
		return
	}
	d := time.Until(next)
	if d < 0 {
		d = 0
	}
	c.deadlineTimer = time.AfterFunc(d, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
}

func expired(t time.Time) bool { return !t.IsZero() && !time.Now().Before(t) }

type timeoutError struct{}

func (timeoutError) Error() string   { return "mtproto tunnel: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
