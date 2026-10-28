package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// MTProtoSession is the sealed-cell layer under the tunnel: it performs the
// eph↔ek handshake, then exchanges sealed cells on the server-assigned
// reference. It mirrors the messenger's session layer and shares its cell
// encoding, but derives keys in the tunnel's own key space (MTPSessionKey), so
// a messenger session can never drive a tunnel or vice versa.
//
// Without it the tunnel's selector would be the only thing naming a stream, and
// another holder of the same config could guess a live 24-bit selector and
// break a stranger's MTProto session.

// MTProtoQuery sends one DNS query name and returns the decoded response
// payload. Injected so the session can be tested without DNS.
type MTProtoQuery func(ctx context.Context, qname string) ([]byte, error)

// ErrMTProtoSessionLost means the server no longer knows this session; the
// caller must hand-shake again.
var ErrMTProtoSessionLost = errors.New("mtproto: session lost")

// ErrMTProtoBadReply means an answer came back that would not open under this
// session's key. Retryable on purpose: it is what a truncated or cached DNS
// answer looks like, and the cell that produced it can simply go again. A
// session that really is gone is reported by the relay in so many words.
var ErrMTProtoBadReply = errors.New("mtproto: reply did not open")

const (
	// mtpCellAttempts retransmits an identical cell on transport loss. Every op
	// is idempotent, so a duplicate just re-runs it.
	mtpCellAttempts = 3
	// mtpHandshakeTimeout bounds one handshake across its fragments.
	mtpHandshakeTimeout = 60 * time.Second
)

// MTProtoSession holds one live tunnel session.
type MTProtoSession struct {
	// onQuery, if set, fires once per outgoing cell (guarded by mu).
	onQuery func()
	// onStep, if set, reports progress through a step whose length is known
	// before it starts -- the handshake knows its fragment count, so the UI
	// can show "2 of 3" instead of a number climbing with no end in sight.
	onStep   func(done, total int)
	query    MTProtoQuery
	queryKey [protocol.KeySize]byte
	mode     protocol.QueryEncoding
	info     *protocol.MTPInfo
	budget   int
	// inFlight is how many cells may be outstanding at once, taken from the
	// fetcher's own rate budget so the window follows the configured rate.
	inFlight int

	mu      sync.Mutex
	ref     [protocol.ChatSelectorSize]byte
	ks      [protocol.KeySize]byte
	counter uint32
	live    bool
	chunk   byte
}

// NewMTProtoSession prepares a session against an advertised server. budget is
// the per-cell plaintext budget the client's scorer picked.
func NewMTProtoSession(query MTProtoQuery, queryKey [protocol.KeySize]byte, mode protocol.QueryEncoding, info *protocol.MTPInfo, budget int) (*MTProtoSession, error) {
	if !info.Usable() {
		return nil, errors.New("mtproto: this server does not offer a usable bridge")
	}
	if budget < protocol.ChatCellPlainMin || budget > protocol.ChatCellPlainMax {
		budget = protocol.ChatCellPlainSize
	}
	if protocol.MTPChunkSize(budget) == 0 {
		return nil, fmt.Errorf("mtproto: a %d-byte cell budget cannot carry the tunnel", budget)
	}
	return &MTProtoSession{query: query, queryKey: queryKey, mode: mode, info: info, budget: budget}, nil
}

// domain picks a bridge sub-domain, rotating by cell so queries spread across
// the advertised set the way feed and chat queries do.
// NewMTProtoSessionOn builds a session that runs its cells over f, the same
// way the messenger does: rate limit, resolver scatter, query mode and query
// key all come from the config's fetcher, so the tunnel inherits the user's
// connection settings instead of inventing its own.
func NewMTProtoSessionOn(f *Fetcher, info *protocol.MTPInfo, budget int) (*MTProtoSession, error) {
	if f == nil {
		return nil, errors.New("mtproto: no fetcher")
	}
	// Declared up front so the query closure can report progress through the
	// session it is about to belong to.
	var sess *MTProtoSession
	query := func(ctx context.Context, qname string) ([]byte, error) {
		if onq := sessionOnQuery(sess); onq != nil {
			onq()
		}
		if err := f.rateWait(ctx); err != nil {
			return nil, err
		}
		picked := f.pickWeightedResolvers(f.scatter)
		if len(picked) == 0 {
			return nil, errors.New("mtproto: no active resolvers")
		}
		if f.debug {
			f.log("[mtproto] query qname=%s resolvers=[%s]", qname, strings.Join(picked, ", "))
		}
		return f.scatterQuery(ctx, picked, qname)
	}
	s, err := NewMTProtoSession(query, f.queryKey, f.QueryMode(), info, budget)
	if err != nil {
		return nil, err
	}
	sess = s
	s.inFlight = f.InFlightBudget()
	return s, nil
}

// sessionOnQuery reads the progress callback under the session lock. On a
// degraded network a handshake is minutes of single queries, so the caller
// needs a way to show that something IS happening.
func sessionOnQuery(s *MTProtoSession) func() {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onQuery
}

// InFlight is how many cells may be outstanding at once.
func (s *MTProtoSession) InFlight() int {
	if s.inFlight < 1 {
		return 1
	}
	return s.inFlight
}

// SetOnStep sets a callback reporting progress through a step of known length.
func (s *MTProtoSession) SetOnStep(fn func(done, total int)) {
	s.mu.Lock()
	s.onStep = fn
	s.mu.Unlock()
}

func (s *MTProtoSession) stepFn() func(done, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onStep
}

// SetOnQuery sets a callback fired once per outgoing cell.
func (s *MTProtoSession) SetOnQuery(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onQuery = fn
}

func (s *MTProtoSession) domain(i int) string {
	d := s.info.Domains
	return d[i%len(d)]
}

// Handshake establishes the session. The ephemeral public key does not fit one
// cell, so it is fragmented; those fragments are the only part that cannot be
// sealed, since they are what establishes the key. Opening the sealed reply
// proves the server performed the same ECDH — and, because the wire version is
// mixed into that key, that it speaks the same version.
func (s *MTProtoSession) Handshake(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, mtpHandshakeTimeout)
	defer cancel()

	eph, err := protocol.GenerateEphemeralKey()
	if err != nil {
		return err
	}
	ks, err := protocol.MTPSessionKey(eph, s.info.EkPub, protocol.MTProtoVersion, s.queryKey)
	if err != nil {
		return err
	}
	var tag [protocol.ChatSelectorSize]byte
	if _, err := rand.Read(tag[:]); err != nil {
		return err
	}
	protocol.ChatMarkHandshakeSelector(&tag)

	// Fixed-size, random-tailed fragments indexed by the (masked) cell counter:
	// the query name carries no framing an observer could fingerprint, and the
	// cells are shaped exactly like a messenger handshake.
	frags, err := protocol.SplitMTPHello(eph.PublicKey().Bytes(), protocol.ChatCellPayloadSize, rand.Reader)
	if err != nil {
		return err
	}

	if step := s.stepFn(); step != nil {
		step(0, len(frags))
	}
	var last []byte
	for i, frag := range frags {
		qname, err := protocol.EncodeChatCell(s.queryKey, s.mode, tag, uint32(i), frag, s.domain(i))
		if err != nil {
			return err
		}
		last, err = s.sendWithRetry(ctx, qname)
		if err != nil {
			return fmt.Errorf("mtproto: handshake fragment %d: %w", i, err)
		}
		if step := s.stepFn(); step != nil {
			step(i+1, len(frags))
		}
	}

	plain, _, err := openSealedResponse(ks, tag, protocol.ChatBootstrapCounter(), last)
	if err != nil {
		return fmt.Errorf("mtproto: handshake reply: %w", err)
	}
	status, ref, chunk, err := protocol.ParseMTPHelloAck(plain)
	if err != nil {
		return fmt.Errorf("mtproto: handshake ack: %w", err)
	}
	if status != protocol.MTPStatusOK {
		return mtpStatusError(status)
	}

	s.mu.Lock()
	s.ref, s.ks, s.chunk, s.counter, s.live = ref, ks, chunk, 0, true
	s.mu.Unlock()
	return nil
}

// Chunk is the stream chunk size agreed at handshake.
func (s *MTProtoSession) Chunk() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.chunk)
}

// Exchange sends one sealed cell and returns the reply plaintext. It satisfies
// MTProtoExchange, so the tunnel's net.Conn sits straight on top.
func (s *MTProtoSession) Exchange(ctx context.Context, plaintext []byte) ([]byte, error) {
	s.mu.Lock()
	if !s.live {
		s.mu.Unlock()
		return nil, ErrMTProtoSessionLost
	}
	// Never seal at a counter that reaches the reserved bootstrap/response
	// regions; re-handshake instead of colliding with them.
	if s.counter >= protocol.ChatBootstrapCounter() {
		s.live = false
		s.mu.Unlock()
		return nil, ErrMTProtoSessionLost
	}
	ref, ks, ctr := s.ref, s.ks, s.counter
	s.counter++
	budget := s.budget
	s.mu.Unlock()

	// Deterministic per-cell jitter, keyed by (selector, counter): control cells
	// vary in length like feed queries, and a retransmit reproduces the exact
	// name so resolver caches stay warm. DATA is sealed exactly — see MTPCellPad.
	jitter := protocol.ChatCellJitter(s.queryKey, ref, ctr)
	padded := protocol.MTPCellPad(plaintext, budget, jitter)
	payload, err := protocol.SealChatCellPayloadN(ks, ref, ctr, plaintext, padded)
	if err != nil {
		return nil, err
	}
	qname, err := protocol.EncodeChatCell(s.queryKey, s.mode, ref, ctr, payload, s.domain(int(ctr)))
	if err != nil {
		return nil, err
	}
	data, err := s.sendWithRetry(ctx, qname)
	if err != nil {
		return nil, err
	}
	if protocol.ChatIsSessionLost(data) {
		s.invalidate()
		return nil, ErrMTProtoSessionLost
	}
	plain, status, err := openSealedResponse(ks, ref, ctr, data)
	if err != nil {
		// A reply that will not open is a BAD REPLY, not a lost session: over
		// DNS a truncated answer or a stale cached one is ordinary. The relay
		// says "session lost" in so many words, checked just above.
		//
		// So do NOT invalidate here -- that fails every later cell and buys a
		// fresh key exchange, tens of round trips, over one garbled answer.
		// The cell is idempotent; let the caller send it again.
		return nil, fmt.Errorf("%w: %v", ErrMTProtoBadReply, err)
	}
	// The tunnel ops carry their own status byte inside the body, so a
	// transport-level status only matters when it reports a dead session.
	if status == protocol.MTPStatusNoTunnel && len(plain) == 0 {
		return []byte{protocol.MTPStatusNoTunnel}, nil
	}
	return plain, nil
}

func (s *MTProtoSession) invalidate() {
	s.mu.Lock()
	s.live = false
	s.mu.Unlock()
}

// Live reports whether a handshake has completed and not been invalidated.
func (s *MTProtoSession) Live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// sendWithRetry retransmits the identical cell on transport loss: UDP has no
// delivery guarantee and every op is idempotent, so a duplicate is harmless.
func (s *MTProtoSession) sendWithRetry(ctx context.Context, qname string) ([]byte, error) {
	var err error
	for attempt := 0; ; attempt++ {
		var data []byte
		data, err = s.query(ctx, qname)
		if err == nil {
			return data, nil
		}
		if attempt+1 >= mtpCellAttempts {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
}

// openSealedResponse unseals a reply, returning its body and the status byte
// the cell layer carried.
func openSealedResponse(ks [protocol.KeySize]byte, ref [protocol.ChatSelectorSize]byte, ctr uint32, data []byte) ([]byte, byte, error) {
	status, body, err := protocol.OpenChatResponse(ks, ref, ctr, data)
	if err != nil {
		return nil, 0, err
	}
	return body, status, nil
}

func mtpStatusError(status byte) error {
	switch status {
	case protocol.MTPStatusDisabled:
		return errors.New("mtproto: this server has the bridge turned off")
	case protocol.MTPStatusBusy:
		return errors.New("mtproto: server is at capacity, try again later")
	case protocol.MTPStatusBadDC:
		return errors.New("mtproto: server refused the data centre")
	case protocol.MTPStatusNoTunnel:
		return ErrMTProtoSessionLost
	default:
		return fmt.Errorf("mtproto: refused (status %d)", status)
	}
}
