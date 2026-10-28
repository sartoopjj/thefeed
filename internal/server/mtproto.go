package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/td/telegram/dcs"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// MTProtoRelay moves opaque bytes between a client's DNS tunnel and a Telegram
// data centre. It never holds a user's auth key and cannot read a single
// message: the MTProto session is negotiated client↔Telegram and the relay only
// sees ciphertext.
//
// It dials ONLY addresses from Telegram's own published DC list. Accepting a
// caller-supplied host would turn the server into an open proxy — anyone could
// point it anywhere, and its operator would answer for that traffic.

const (
	// mtpMaxDownPerReply is how many downstream bytes ride back on one cell.
	// A DNS response carries MaxBlockPayload, less the reply header and the
	// seal; TestReplyFillsTheDNSResponse measures what is actually left.
	// Downstream is where a conversation's bulk travels, so every byte short
	// of the limit is an extra query the user waits for.
	mtpMaxDownPerReply = 580
	// mtpDefaultBuffer caps per-tunnel downstream held in memory. Once full the
	// relay stops reading from Telegram, so a fast DC cannot outrun a slow DNS
	// client and exhaust the server's memory. 32 KiB is ~60 replies of runway
	// between client polls — ample for text — and keeps 1000 active users near
	// 32 MB rather than 64.
	mtpDefaultBuffer = 32 << 10
	// mtpWriteTimeout bounds a blocked upstream write so one stuck DC
	// connection can't pin a relay slot forever.
	mtpWriteTimeout = 30 * time.Second
)

// MTProtoConfig configures the relay. Zero values take the defaults.
type MTProtoConfig struct {
	Enabled bool
	// Domains are the dedicated sub-domains tunnel cells arrive on, separate
	// from the feed and chat domains.
	Domains []string
	// MaxTunnels bounds concurrently ACTIVE users: one tunnel is one user's
	// connection to Telegram, never shared. Idle ones expire, so the number of
	// users served over a day is far higher. 0 means no cap — the real ceiling
	// is then the process file-descriptor limit, and a failed dial degrades to
	// Busy rather than taking the server down.
	//
	// MEMORY BUDGET, worst case per tunnel: BufferBytes downstream +
	// mtpMaxPending (16 KiB) upstream + one socket + one goroutine. With the
	// 32 KiB default that is ~48 KiB of heap per tunnel, so 3000 concurrent
	// tunnels is ~140 MiB plus kernel socket buffers. Buffers are grown on
	// demand, so typical use is far below the cap — but an attacker picks the
	// worst case, which is what to size for when leaving MaxTunnels at 0.
	MaxTunnels int
	// IdleTTL expires a tunnel with no cells. The client re-opens on next use,
	// which costs a reconnect, not a fresh login.
	IdleTTL time.Duration
	// BufferBytes is the per-tunnel downstream cap.
	BufferBytes int
	// DownBytesPerMin throttles one tunnel's downstream traffic. The relay
	// carries opaque bytes, so this is what bounds a client that implements
	// file downloads. 0 leaves it uncapped.
	DownBytesPerMin int
	// DialTimeout bounds the connection to a DC.
	DialTimeout time.Duration
	// TestDCs dials Telegram's test data centres instead of production.
	TestDCs bool
	// Debug logs the tunnel lifecycle (open, upstream/downstream byte counts,
	// close). Off by default: one line per cell would drown the log.
	Debug bool
	// DCAddrs overrides the data-centre addresses by id ("host:port"). Empty
	// means Telegram's published list. Lets an operator pin a particular
	// front-end, and lets a test point the relay at a local stand-in.
	DCAddrs map[uint8]string
	// QueryKey is the config's derived query key, mixed into the session
	// derivation so the config passphrase is required to open a tunnel.
	QueryKey [protocol.KeySize]byte
	// Ek is the server's x25519 handshake key — the same key the messenger
	// publishes. Safe to share: the two derive session keys in different key
	// spaces (MTPSessionKey vs ChatSessionKey).
	Ek *ecdh.PrivateKey
}

type mtpTunnel struct {
	// pend holds upstream bytes until the client marks the message complete
	// (MTPOpDataEnd), so the far end never sees a trickle (guarded by mu).
	pend []byte
	// debug mirrors MTProtoConfig.Debug for this tunnel.
	debug bool
	mu    sync.Mutex
	cond  *sync.Cond

	// conn is nil until the first whole message is ready to go out. Telegram
	// closes a connection that says nothing within ~1.6s, far less than the
	// tens of seconds a message takes to arrive over DNS, so the dial waits
	// until there is something to send instead of racing that timer.
	conn    net.Conn
	host    string // dc address, dialled on first flush
	dialing sync.Mutex
	dialDC  uint8
	dialTO  time.Duration
	// wmu pairs taking bytes out of pend with writing them, so two cells being
	// answered at once cannot hand Telegram their slices in the wrong order.
	// Lock order is always wmu, then dialing, then mu.
	wmu sync.Mutex

	upOff   uint16 // next upstream offset expected
	down    []byte // downstream waiting for the client
	downOff uint16 // stream offset of down[0]

	upstreamDone bool // the DC hung up (or errored); deliver what is left, then report
	closed       bool
	lastSeen     time.Time
	bufCap       int

	// ahead holds cells that arrived before the gap in front of them, keyed by
	// their stream offset. A client with several queries in flight cannot
	// control which answer lands first, so out-of-order arrival is normal and
	// discarding it would waste the whole point of sending in parallel.
	ahead map[uint16][]byte
	// flushAt is where a message ends. The flush waits for the stream to
	// become contiguous through it, so the far end is still handed whole
	// messages even though the cells raced.
	flushAt    uint16
	flushArmed bool
	// started is set once a whole message has gone out. Telegram's first
	// impression is a complete message; after that, silence is the only thing
	// it minds, so later bytes stream as they arrive.
	started bool

	// Downstream throughput budget. The relay cannot see WHAT it carries --
	// the stream is encrypted end to end with Telegram -- so volume is the
	// only lever against a client that decides to pull files through it.
	winStart time.Time
	winBytes int
	rateCap  int
}

// MTProtoRelay is the server-side half of the tunnel.
type MTProtoRelay struct {
	cfg  MTProtoConfig
	addr map[uint8]string // dc id → host:port, from Telegram's published list

	mu sync.Mutex
	// sessions are keyed by the server-assigned reference; each owns one
	// tunnel. handshakes hold partial ephemeral keys, keyed by the client's
	// handshake tag, and expire quickly so a flood cannot pin memory.
	sessions   map[[protocol.ChatSelectorSize]byte]*mtpSession
	handshakes map[[protocol.ChatSelectorSize]byte]*mtpHandshake
}

type mtpSession struct {
	ks [protocol.KeySize]byte

	tmu      sync.Mutex
	tunnel   *mtpTunnel
	lastSeen time.Time
	// ctlCtr/ctlReply remember the last OPEN or CLOSE and what it answered.
	//
	// DATA and POLL are idempotent -- each cell carries its own stream offset
	// -- which is why the client retransmits them freely on loss. OPEN and
	// CLOSE are not: OPEN replaces the live tunnel with offsets at zero, CLOSE
	// tears the current one down, so a duplicate from a retry or a replay
	// wrecks a tunnel mid-stream. Counters rise within a session, so one
	// remembered answer suffices: an op at or below it gets that answer back.
	ctlCtr   uint32
	ctlSeen  bool
	ctlReply []byte
}

// repeatCtl reports the remembered answer when counter is not newer than the
// last control op this session carried out.
func (s *mtpSession) repeatCtl(counter uint32) ([]byte, bool) {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if !s.ctlSeen || counter > s.ctlCtr {
		return nil, false
	}
	return s.ctlReply, true
}

func (s *mtpSession) rememberCtl(counter uint32, reply []byte) {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.ctlCtr, s.ctlSeen = counter, true
	s.ctlReply = append([]byte(nil), reply...)
}

func (s *mtpSession) currentTunnel() *mtpTunnel {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	return s.tunnel
}

func (s *mtpSession) setTunnel(t *mtpTunnel) *mtpTunnel {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	old := s.tunnel
	s.tunnel = t
	return old
}

type mtpHandshake struct {
	frags       map[uint32][]byte // by cell counter
	payloadSize int
	expires     time.Time
	// ack is the sealed reply a COMPLETED handshake produced, kept so that
	// re-sending its last fragment answers the same thing again. The client
	// reads the last fragment's reply as the ack, so a lost reply makes it
	// retry that fragment; forgetting the handshake on completion turned the
	// retry into a fresh, incomplete one and the client got a short
	// "nothing yet" where it expected the ack ("sealed too short").
	ack []byte
	// born is when this attempt started. Every refresh is measured against it
	// so no entry can be kept alive for ever by being poked: a real handshake
	// takes seconds, and one still unfinished after mtpHandshakeMaxLife is not
	// going to finish.
	born time.Time
}

// deadline is how long this attempt may still be held, never past its total
// lifetime from birth.
func (h *mtpHandshake) deadline(now time.Time) time.Time {
	want := now.Add(mtpHandshakeTTL)
	if cap := h.born.Add(mtpHandshakeMaxLife); !h.born.IsZero() && want.After(cap) {
		return cap
	}
	return want
}

// mtpHandshakeTTL bounds how long a partial handshake is remembered. Minutes,
// not seconds: a client at one query per second must be able to land both
// HELLO fragments inside it, or the second reads as a new attempt.
const mtpHandshakeTTL = 5 * time.Minute

// mtpHandshakeMaxLife bounds an attempt's TOTAL life, however often it is
// touched -- the TTL alone lets an entry re-sent just under it live for ever
// and hold its slot against the cap.
const mtpHandshakeMaxLife = 10 * time.Minute

// mtpMaxHandshakes caps partial handshakes held at once. The TTL above is long
// enough that it alone no longer limits a flood, so the count does: each entry
// is a few hundred bytes, and a client whose handshake is refused simply
// retries.
const mtpMaxHandshakes = 2048

// mtpMaxPending caps the upstream bytes the relay holds for ONE tunnel while
// waiting for the client to mark a message complete.
//
// Sized for thousands of concurrent tunnels, not for one: client->server
// MTProto messages are requests, well under a kilobyte even during the
// handshake, so 16 KiB is already generous. The number matters because it is
// paid PER TUNNEL and an attacker chooses how much of it to use -- see the
// budget note on MTProtoConfig.MaxTunnels.
const mtpMaxPending = 16 << 10

// NewMTProtoRelay builds a relay. It returns nil when disabled so callers can
// treat "no relay" and "relay off" the same way.
func NewMTProtoRelay(cfg MTProtoConfig) *MTProtoRelay {
	if !cfg.Enabled {
		return nil
	}
	if cfg.MaxTunnels < 0 {
		cfg.MaxTunnels = 0 // negative is meaningless; treat as uncapped
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 5 * time.Minute
	}
	if cfg.BufferBytes <= 0 {
		cfg.BufferBytes = mtpDefaultBuffer
	}
	if cfg.DownBytesPerMin < 0 {
		cfg.DownBytesPerMin = 0
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 20 * time.Second
	}
	list := dcs.Prod()
	if cfg.TestDCs {
		list = dcs.Test()
	}
	addr := make(map[uint8]string)
	if len(cfg.DCAddrs) > 0 {
		for id, a := range cfg.DCAddrs {
			addr[id] = a
		}
		return &MTProtoRelay{
			cfg:        cfg,
			addr:       addr,
			sessions:   make(map[[protocol.ChatSelectorSize]byte]*mtpSession),
			handshakes: make(map[[protocol.ChatSelectorSize]byte]*mtpHandshake),
		}
	}
	for _, o := range list.Options {
		// IPv4, non-media, non-CDN only: one stable address per DC id is all a
		// relay needs, and media/CDN endpoints speak a different flow.
		if o.Ipv6 || o.MediaOnly || o.CDN || o.ID <= 0 || o.ID > 255 {
			continue
		}
		if _, seen := addr[uint8(o.ID)]; !seen {
			addr[uint8(o.ID)] = net.JoinHostPort(o.IPAddress, fmt.Sprint(o.Port))
		}
	}
	return &MTProtoRelay{
		cfg:        cfg,
		addr:       addr,
		sessions:   make(map[[protocol.ChatSelectorSize]byte]*mtpSession),
		handshakes: make(map[[protocol.ChatSelectorSize]byte]*mtpHandshake),
	}
}

// Domains returns the sub-domains tunnel cells arrive on.
func (r *MTProtoRelay) Domains() []string {
	if r == nil {
		return nil
	}
	return r.cfg.Domains
}

// Info is the capability payload clients fetch to discover the tunnel: the
// handshake key, the sub-domains, and the widest chunk this server offers.
// The client still negotiates the real chunk at OPEN.
func (r *MTProtoRelay) Info() protocol.MTPInfo {
	if r == nil {
		return protocol.MTPInfo{}
	}
	return protocol.MTPInfo{
		MinVersion: protocol.MTProtoVersion,
		MaxVersion: protocol.MTProtoVersion,
		Enabled:    true,
		Domains:    r.cfg.Domains,
		EkPub:      r.cfg.Ek.PublicKey().Bytes(),
		Chunk:      uint8(protocol.MTPChunkSize(protocol.ChatCellPlainMax)),
		TestDCs:    r.cfg.TestDCs,
	}
}

// Stats reports live tunnel use, for the server report.
func (r *MTProtoRelay) Stats() (active, max int) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions), r.cfg.MaxTunnels
}

// LiveTunnels counts the sockets actually open to Telegram right now.
//
// Not the same as the session count: a session outlives the tunnel on it, so
// someone who has closed a connection and not opened the next one yet still
// has a session but no socket. The two together say what the relay is really
// carrying -- sessions are users, tunnels are the connections they cost.
func (r *MTProtoRelay) LiveTunnels() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	all := make([]*mtpSession, 0, len(r.sessions))
	for _, sess := range r.sessions {
		all = append(all, sess)
	}
	r.mu.Unlock()

	n := 0
	for _, sess := range all {
		sess.tmu.Lock()
		t := sess.tunnel
		sess.tmu.Unlock()
		if t == nil {
			continue
		}
		t.mu.Lock()
		open := !t.closed && t.conn != nil
		t.mu.Unlock()
		if open {
			n++
		}
	}
	return n
}

// HandleCell processes one tunnel cell off the wire and returns the response
// payload, already sealed where a session exists.
//
// A handshake-tagged selector carries an unsealed HELLO fragment; anything else
// names a session whose key must open the payload. That is what stops another
// holder of the same config from driving a stranger's tunnel by guessing a
// 24-bit selector: without the session key the seal does not open.
func (r *MTProtoRelay) HandleCell(selector [protocol.ChatSelectorSize]byte, counter uint32, payload []byte, domain string, now time.Time) []byte {
	if r == nil {
		return protocol.ChatSessionLostResp
	}
	if protocol.ChatIsHandshakeSelector(selector) {
		return r.handshake(selector, counter, payload, now)
	}

	// Request counters live BELOW the reserved regions: bootstrap at
	// 0x400000, responses at reqCounter|0x800000. A cell arriving at or above
	// bootstrap is a buggy or hostile client, and it must be refused before
	// anything is sealed -- the reply's nonce is derived from this very
	// counter, so answering a request numbered like a response would re-seal
	// under a nonce already used for a different plaintext. The chat relay has
	// always refused this (chat.go); the MTProto relay, built from it, did not.
	if counter >= protocol.ChatBootstrapCounter() {
		if r.cfg.Debug {
			log.Printf("[mtproto] refused: counter %d is in the reserved range", counter)
		}
		return protocol.ChatSessionLostResp
	}

	r.mu.Lock()
	sess := r.sessions[selector]
	// Read under the lock, not in the log line below it: a map read racing a
	// write is undefined however harmless the number is, and `go test -race`
	// would only ever catch it on a build with debug logging on.
	live := len(r.sessions)
	r.mu.Unlock()
	if sess == nil {
		// Which of the two session-lost causes fired decides the fix, and they
		// are indistinguishable from the client.
		if r.cfg.Debug {
			log.Printf("[mtproto] session lost: no such session (%d live)", live)
		}
		return protocol.ChatSessionLostResp
	}

	plain, err := protocol.OpenChatCellPayload(sess.ks, selector, counter, payload)
	if err != nil {
		// A cell that does not open is not this session's: say nothing useful.
		if r.cfg.Debug {
			log.Printf("[mtproto] session lost: cell would not open (counter=%d): %v", counter, err)
		}
		return protocol.ChatSessionLostResp
	}
	// Only now, with the seal verified, does this count as the session being
	// used. Refreshing it beforehand let anyone who knew a selector -- it
	// travels in the clear -- hold a session, its upstream socket and its
	// buffers open indefinitely with cells that never opened.
	sess.tmu.Lock()
	sess.lastSeen = now
	sess.tmu.Unlock()

	status, body := r.dispatch(selector, sess, counter, plain, now)
	return protocol.SealChatResponse(sess.ks, selector, counter, status, body)
}

// handshake reassembles the client ephemeral key across unsealed fragments
// and, on the last one, derives the session key and assigns a reference.
//
// Fragments carry no framing: the index is the cell counter and the count comes
// from the payload size, so nothing in the query name identifies the protocol.
// rememberAckLocked parks a completed handshake's reply so a retransmitted
// fragment is answered identically. Caller holds r.mu.
func (r *MTProtoRelay) rememberAckLocked(tag [protocol.ChatSelectorSize]byte, ack []byte, size int, now time.Time) {
	hs := r.handshakes[tag]
	if hs == nil {
		hs = &mtpHandshake{born: now}
		r.handshakes[tag] = hs
	}
	// The size is what a retransmit is matched on, so it must be right even
	// when the entry had to be recreated.
	hs.payloadSize = size
	// The fragments are no longer needed; only the answer is. Emptied rather
	// than nilled: whether a later fragment can reach the write below depends
	// on two conditions staying exact complements, and getting that wrong
	// writes to a nil map -- which panics while holding r.mu and deadlocks the
	// relay for every user, not just this one.
	hs.frags = map[uint32][]byte{}
	hs.ack = ack
	hs.expires = hs.deadline(now)
}

// evictOldestHandshakeLocked drops the attempt closest to expiry, to make room
// for a new one. Caller holds r.mu.
func (r *MTProtoRelay) evictOldestHandshakeLocked() {
	var oldest [protocol.ChatSelectorSize]byte
	var at time.Time
	found := false
	for tag, hs := range r.handshakes {
		if !found || hs.expires.Before(at) {
			oldest, at, found = tag, hs.expires, true
		}
	}
	if found {
		delete(r.handshakes, oldest)
		if r.cfg.Debug {
			log.Printf("[mtproto] handshake table full (%d): evicted the oldest", len(r.handshakes)+1)
		}
	}
}

func (r *MTProtoRelay) handshake(tag [protocol.ChatSelectorSize]byte, counter uint32, payload []byte, now time.Time) []byte {
	if !r.cfg.Enabled || r.cfg.Ek == nil {
		return protocol.ChatSessionLostResp
	}
	total := protocol.MTPHelloFragments(len(payload))
	if total == 0 || total > protocol.MTPMaxHelloFragments || counter >= uint32(total) {
		return protocol.ChatSessionLostResp
	}

	r.mu.Lock()
	hs := r.handshakes[tag]
	// Already finished: this is the client re-sending a fragment whose reply it
	// never saw. Hand back the same sealed ack rather than starting over -- a
	// retransmit over DNS must be idempotent here exactly as it is for data.
	if hs != nil && hs.ack != nil && hs.payloadSize == len(payload) && !now.After(hs.expires) {
		hs.expires = hs.deadline(now)
		ack := hs.ack
		r.mu.Unlock()
		return ack
	}
	// A fragment of a different size belongs to a different attempt: start over
	// rather than mixing two ephemeral keys into one buffer.
	if hs == nil || hs.payloadSize != len(payload) || now.After(hs.expires) {
		// New attempt, so it counts against the cap; continuing one never
		// does.
		//
		// At capacity evict the OLDEST rather than refuse the newcomer:
		// refusing lets whoever filled the table first keep it for ever at a
		// few queries a second, locking out every real user. Evicting makes a
		// flood displace itself, and costs a real client nothing.
		if hs == nil && len(r.handshakes) >= mtpMaxHandshakes {
			r.evictOldestHandshakeLocked()
		}
		hs = &mtpHandshake{frags: make(map[uint32][]byte), payloadSize: len(payload), born: now}
		r.handshakes[tag] = hs
	}
	hs.expires = hs.deadline(now)
	if hs.frags == nil {
		hs.frags = map[uint32][]byte{}
	}
	hs.frags[counter] = append([]byte(nil), payload...)
	eph, complete := protocol.JoinMTPHello(hs.frags, hs.payloadSize)
	r.mu.Unlock()

	if !complete {
		// Nothing to say yet, and nothing that could be mistaken for a reply.
		return protocol.ChatSessionLostResp
	}

	// The wire version is mixed into this derivation, so a client speaking a
	// different version simply derives a different key and cannot open the
	// reply — no version byte needs to appear on the wire.
	ks, err := protocol.MTPSessionKey(r.cfg.Ek, eph, protocol.MTProtoVersion, r.cfg.QueryKey)
	if err != nil {
		return protocol.ChatSessionLostResp
	}

	var ref [protocol.ChatSelectorSize]byte
	if _, err := rand.Read(ref[:]); err != nil {
		return protocol.ChatSessionLostResp
	}
	// A reference must never look like a handshake tag, or the next cell would
	// be routed back into this path instead of the session table.
	protocol.ChatClearHandshakeSelector(&ref)

	r.mu.Lock()
	if r.cfg.MaxTunnels > 0 && len(r.sessions) >= r.cfg.MaxTunnels {
		busy := protocol.SealChatResponse(ks, tag, protocol.ChatBootstrapCounter(),
			protocol.MTPStatusOK, protocol.BuildMTPHelloAck(protocol.MTPStatusBusy, ref, 0))
		r.rememberAckLocked(tag, busy, len(payload), now)
		r.mu.Unlock()
		return busy
	}
	r.sessions[ref] = &mtpSession{ks: ks, lastSeen: now}

	chunk := protocol.MTPChunkSize(protocol.ChatCellPlainMax)
	ack := protocol.BuildMTPHelloAck(protocol.MTPStatusOK, ref, byte(chunk))
	sealed := protocol.SealChatResponse(ks, tag, protocol.ChatBootstrapCounter(), protocol.MTPStatusOK, ack)
	// Kept so a retried last fragment gets this very answer back.
	r.rememberAckLocked(tag, sealed, len(payload), now)
	r.mu.Unlock()
	return sealed
}

// dispatch runs one tunnel op inside an established session.
func (r *MTProtoRelay) dispatch(ref [protocol.ChatSelectorSize]byte, sess *mtpSession, counter uint32, plaintext []byte, now time.Time) (byte, []byte) {
	if len(plaintext) == 0 {
		return protocol.MTPStatusBadRequest, nil
	}
	switch plaintext[0] {
	case protocol.MTPOpOpen:
		dc, err := protocol.ParseMTPOpen(plaintext)
		if err != nil {
			return protocol.MTPStatusBadRequest, nil
		}
		// A repeat must be answered, not carried out: re-opening would hand
		// the client a tunnel whose stream starts at zero while its own is
		// half way through a message.
		if reply, ok := sess.repeatCtl(counter); ok {
			return protocol.MTPStatusOK, reply
		}
		reply := r.open(ref, sess, dc, now)
		sess.rememberCtl(counter, reply)
		return protocol.MTPStatusOK, reply

	case protocol.MTPOpClose:
		// Closes the TUNNEL, not the session. gotd sends this on every
		// re-dial, and the session holds the keys the next tunnel needs: drop
		// it here and a session shared across dials dies on the first close.
		// The sweeper reclaims it once it really is idle.
		//
		// Deduplicated for the same reason as OPEN: a stale CLOSE landing
		// after the next OPEN would tear down a tunnel it never knew about.
		if reply, ok := sess.repeatCtl(counter); ok {
			return protocol.MTPStatusOK, reply
		}
		if t := sess.setTunnel(nil); t != nil {
			t.close()
		}
		reply := []byte{protocol.MTPStatusOK}
		sess.rememberCtl(counter, reply)
		return protocol.MTPStatusOK, reply

	case protocol.MTPOpData, protocol.MTPOpDataEnd, protocol.MTPOpPoll:
		req, err := protocol.ParseMTPRequest(plaintext)
		if err != nil {
			return protocol.MTPStatusBadRequest, nil
		}
		t := sess.currentTunnel()
		if t == nil {
			return protocol.MTPStatusOK, []byte{protocol.MTPStatusNoTunnel}
		}
		return protocol.MTPStatusOK, t.handle(req)
	}
	return protocol.MTPStatusBadRequest, nil
}

// open dials a DC for an established session and attaches the tunnel.
func (r *MTProtoRelay) open(ref [protocol.ChatSelectorSize]byte, sess *mtpSession, dc uint8, now time.Time) []byte {
	host, ok := r.addr[dc]
	if !ok {
		return protocol.BuildMTPOpenReply(protocol.MTPStatusBadDC, 0)
	}

	// The DC is NOT dialled here. Telegram hangs up after about 1.6 seconds of
	// silence, and the first message needs tens of seconds to arrive one DNS
	// cell at a time, so a connection opened now is always dead before the
	// client's first byte reaches it.
	t := &mtpTunnel{
		host: host, dialDC: dc, dialTO: r.cfg.DialTimeout,
		lastSeen: now, bufCap: r.cfg.BufferBytes,
		rateCap: r.cfg.DownBytesPerMin, debug: r.cfg.Debug,
	}
	t.cond = sync.NewCond(&t.mu)

	// Re-opening on a live session replaces the old tunnel rather than leaking
	// it: a client that lost its state just dials again.
	if old := sess.setTunnel(t); old != nil {
		old.close()
	}

	chunk := protocol.MTPChunkSize(protocol.ChatCellPlainMax)
	if r.cfg.Debug {
		log.Printf("[mtproto] open dc%d -> %s accepted, dial deferred (chunk=%d)", dc, host, chunk)
	}
	return protocol.BuildMTPOpenReply(protocol.MTPStatusOK, byte(chunk))
}

// Sweep expires idle sessions and stale partial handshakes.
func (r *MTProtoRelay) Sweep(now time.Time) int {
	if r == nil {
		return 0
	}
	var dead []*mtpSession
	r.mu.Lock()
	for ref, sess := range r.sessions {
		sess.tmu.Lock()
		idle := now.Sub(sess.lastSeen) > r.cfg.IdleTTL
		sess.tmu.Unlock()
		if idle {
			delete(r.sessions, ref)
			dead = append(dead, sess)
		}
	}
	for tag, hs := range r.handshakes {
		if now.After(hs.expires) {
			delete(r.handshakes, tag)
		}
	}
	r.mu.Unlock()
	for _, sess := range dead {
		if t := sess.setTunnel(nil); t != nil {
			t.close()
		}
	}
	return len(dead)
}

// RunSweeper expires idle sessions until ctx ends.
func (r *MTProtoRelay) RunSweeper(ctx context.Context) {
	if r == nil {
		return
	}
	tick := r.cfg.IdleTTL / 4
	if tick < 30*time.Second {
		tick = 30 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.closeAll()
			return
		case <-t.C:
			r.Sweep(time.Now())
		}
	}
}

func (r *MTProtoRelay) closeAll() {
	r.mu.Lock()
	all := make([]*mtpSession, 0, len(r.sessions))
	for ref, sess := range r.sessions {
		all = append(all, sess)
		delete(r.sessions, ref)
	}
	r.handshakes = make(map[[protocol.ChatSelectorSize]byte]*mtpHandshake)
	r.mu.Unlock()
	for _, sess := range all {
		if t := sess.setTunnel(nil); t != nil {
			t.close()
		}
	}
}

// ensureConn dials the DC the first time there is something to send and starts
// the read loop. Dialling happens off t.mu so cells still being answered for
// this tunnel are not blocked behind a TCP connect.
func (t *mtpTunnel) ensureConn() (net.Conn, error) {
	t.dialing.Lock()
	defer t.dialing.Unlock()

	t.mu.Lock()
	conn, closed := t.conn, t.closed
	t.mu.Unlock()
	if closed {
		return nil, errTunnelGone
	}
	if conn != nil {
		return conn, nil
	}

	conn, err := net.DialTimeout("tcp", t.host, t.dialTO)
	if err != nil {
		log.Printf("[mtproto] dial dc%d: %v", t.dialDC, err)
		return nil, err
	}
	t.mu.Lock()
	// Closed while we were dialling: drop it rather than leak a live socket.
	if t.closed {
		t.mu.Unlock()
		conn.Close()
		return nil, errTunnelGone
	}
	t.conn = conn
	t.mu.Unlock()
	if t.debug {
		log.Printf("[mtproto] dial dc%d -> %s ok", t.dialDC, t.host)
	}
	go t.readLoop()
	return conn, nil
}

// errTunnelGone means the tunnel was torn down before its DC was dialled.
var errTunnelGone = errors.New("mtproto: tunnel closed before dial")

// readLoop drains the DC into the downstream buffer, pausing when the buffer is
// full so a fast DC cannot outrun a slow DNS client.
func (t *mtpTunnel) readLoop() {
	buf := make([]byte, 4096)
	for {
		t.mu.Lock()
		for !t.closed && len(t.down) >= t.bufCap {
			t.cond.Wait()
		}
		closed := t.closed
		conn := t.conn
		t.mu.Unlock()
		if closed || conn == nil {
			return
		}

		n, err := conn.Read(buf)
		if n > 0 {
			t.mu.Lock()
			t.down = append(t.down, buf[:n]...)
			t.cond.Broadcast()
			t.mu.Unlock()
		}
		if err != nil {
			t.mu.Lock()
			t.upstreamDone = true
			t.cond.Broadcast()
			t.mu.Unlock()
			return
		}
	}
}

// flushUpstream hands Telegram whatever of the stream is ready, and reports how
// many bytes went out.
//
// Taking the bytes and writing them is ONE step, under wmu: cells are answered
// concurrently, so two of them extracting slices and then racing to the socket
// would deliver the stream out of order. Only cells that have something to send
// contend here; polls and bare acks never take this lock.
func (t *mtpTunnel) flushUpstream() (int, byte) {
	t.wmu.Lock()
	defer t.wmu.Unlock()

	t.mu.Lock()
	var toWrite []byte
	switch {
	case t.flushArmed && protocol.MTPSeqDelta(t.flushAt, t.upOff) >= 0 && len(t.pend) > 0:
		toWrite = t.pend
		t.pend = nil
		t.flushArmed = false
		t.started = true
	case t.started && len(t.pend) > 0:
		// Live connection: Telegram closes after about 30 seconds with nothing
		// received, and one message can take longer than that to arrive cell by
		// cell. Measured against a real DC, a partial frame is harmless and any
		// byte resets that timer, so contiguous bytes go out as they arrive.
		toWrite = t.pend
		t.pend = nil
	}
	t.mu.Unlock()
	if len(toWrite) == 0 {
		return 0, protocol.MTPStatusOK
	}

	// The first flush dials, so Telegram's silence timer starts with a whole
	// message already in hand.
	conn, err := t.ensureConn()
	if err != nil {
		// The bytes were taken out of pend and acked, so there is no way to
		// deliver them now: the stream is broken, and the tunnel has to end
		// rather than silently lose a message.
		t.close()
		// Out of descriptors is this relay's problem, not the DC's: Busy tells
		// the client to come back instead of blaming Telegram.
		if isResourceExhausted(err) {
			return 0, protocol.MTPStatusBusy
		}
		return 0, protocol.MTPStatusUpstreamClosed
	}
	_ = conn.SetWriteDeadline(time.Now().Add(mtpWriteTimeout))
	if _, err := conn.Write(toWrite); err != nil {
		t.mu.Lock()
		t.upstreamDone = true
		t.cond.Broadcast()
		t.mu.Unlock()
	}
	return len(toWrite), protocol.MTPStatusOK
}

// handle applies one DATA or POLL request and builds the reply.
func (t *mtpTunnel) handle(req *protocol.MTPRequest) []byte {
	t.mu.Lock()
	// Accept only the slice that starts where the stream is. An earlier offset
	// is a replay of a cell whose reply was lost: ignore it and re-ack, so
	// retrying over DNS can never duplicate bytes.
	isData := req.Op == protocol.MTPOpData || req.Op == protocol.MTPOpDataEnd
	if isData && len(req.Chunk) > 0 {
		if !t.acceptLocked(req) {
			t.mu.Unlock()
			if t.debug {
				log.Printf("[mtproto] upstream overflow: dropping tunnel")
			}
			t.close()
			return []byte{protocol.MTPStatusBadRequest}
		}
	}
	// A message ends where this cell ends; the flush waits for everything in
	// front of it to arrive.
	if req.Op == protocol.MTPOpDataEnd {
		end := req.UpSeq + uint16(len(req.Chunk))
		if !t.flushArmed || protocol.MTPSeqDelta(t.flushAt, end) > 0 {
			t.flushAt, t.flushArmed = end, true
		}
	}
	t.mu.Unlock()

	wrote, status := t.flushUpstream()
	if status != protocol.MTPStatusOK {
		return []byte{status}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Drop what the client has acknowledged, then hand back the next slice.
	if adv := protocol.MTPSeqDelta(t.downOff, req.DownAck); adv > 0 && adv <= len(t.down) {
		t.down = t.down[adv:]
		t.downOff = req.DownAck
		// Acknowledged is the only thing that definitely left: charge it here,
		// once, rather than every time these bytes were offered.
		t.chargeLocked(adv)
		t.cond.Broadcast() // the read loop may be parked on a full buffer
	}

	n := len(t.down)
	if n > mtpMaxDownPerReply {
		n = mtpMaxDownPerReply
	}
	n = t.throttleLocked(n)
	// The one line that answers "is the far end saying anything?": bytes
	// written upstream, bytes handed back, and what is still buffered.
	if t.debug {
		log.Printf("[mtproto] data up=%d down=%d buffered=%d upstreamDone=%v",
			wrote, n, len(t.down)-n, t.upstreamDone)
	}
	// What is still held for this client, so the UI can say "receiving" with a
	// number instead of showing an unexplained wait.
	left := len(t.down) - n
	if left > 0xffff {
		left = 0xffff
	}
	reply := protocol.MTPReply{
		Status:   protocol.MTPStatusOK,
		UpAck:    t.upOff,
		DownSeq:  t.downOff,
		DownLeft: uint16(left),
		Down:     t.down[:n],
	}
	// Report the far end only once everything buffered has been handed over,
	// so a session that ends mid-reply loses nothing.
	if t.upstreamDone && len(t.down) == n {
		reply.Status = protocol.MTPStatusUpstreamClosed
	}
	return protocol.BuildMTPReply(reply)
}

// acceptLocked folds one upstream cell into the stream, holding anything that
// arrived early until the bytes in front of it turn up. Reports false when the
// client is past what the relay will hold for it. Caller holds t.mu.
func (t *mtpTunnel) acceptLocked(req *protocol.MTPRequest) bool {
	d := protocol.MTPSeqDelta(t.upOff, req.UpSeq)
	if d < 0 {
		// Already taken: a replay whose reply was lost. The ack re-tells it.
		return true
	}
	if old, dup := t.ahead[req.UpSeq]; dup {
		// A LONGER cell at an offset already held replaces it. The client
		// re-chunks what is unacknowledged every round, so a partial ack plus
		// a fresh write can grow the cell starting here. Keep the short one
		// and the bytes between are in nobody's hands: the client counts them
		// sent, the relay never had them, and the stream stalls on a gap
		// nothing retransmits. The shorter is always a prefix of the longer.
		if len(req.Chunk) <= len(old) {
			return true
		}
		if t.heldLocked()-len(old)+len(req.Chunk) > mtpMaxPending {
			return false
		}
		t.ahead[req.UpSeq] = append([]byte(nil), req.Chunk...)
		return true
	}
	// HARD CAP, checked BEFORE anything is kept and counting everything held:
	// a client that never completes a message must not be able to make the
	// relay buffer for ever.
	if t.heldLocked()+len(req.Chunk) > mtpMaxPending {
		return false
	}
	if d > 0 {
		// Early: hold it until the bytes in front of it arrive.
		if t.ahead == nil {
			t.ahead = make(map[uint16][]byte)
		}
		t.ahead[req.UpSeq] = append([]byte(nil), req.Chunk...)
		return true
	}
	t.pend = append(t.pend, req.Chunk...)
	t.upOff += uint16(len(req.Chunk))
	t.drainAheadLocked()
	return true
}

// heldLocked is every upstream byte the relay is holding for this tunnel.
func (t *mtpTunnel) heldLocked() int {
	held := len(t.pend)
	for _, c := range t.ahead {
		held += len(c)
	}
	return held
}

// drainAheadLocked appends cells that were waiting on the gap just filled.
func (t *mtpTunnel) drainAheadLocked() {
	for len(t.ahead) > 0 {
		chunk, ok := t.ahead[t.upOff]
		if !ok {
			return
		}
		delete(t.ahead, t.upOff)
		t.pend = append(t.pend, chunk...)
		t.upOff += uint16(len(chunk))
	}
}

// rollWindowLocked starts a new minute if the last one is over.
func (t *mtpTunnel) rollWindowLocked(now time.Time) {
	if t.winStart.IsZero() || now.Sub(t.winStart) >= time.Minute {
		t.winStart, t.winBytes = now, 0
	}
}

// throttleLocked trims a reply to what is left of this tunnel's minute budget.
// Nothing is dropped: the bytes stay buffered and go out in the next window,
// so a heavy client slows down instead of breaking. Caller holds t.mu.
//
// It only TRIMS, never charges. The same leading bytes are re-offered in every
// reply until the client acks them, so charging here would bill one delivery
// once per cell in flight and divide the cap by the window size. chargeLocked
// books what the ack proves arrived.
func (t *mtpTunnel) throttleLocked(n int) int {
	if t.rateCap <= 0 || n == 0 {
		return n
	}
	t.rollWindowLocked(time.Now())
	left := t.rateCap - t.winBytes
	if left <= 0 {
		return 0
	}
	if n > left {
		n = left
	}
	return n
}

// chargeLocked books bytes the client has acknowledged against the minute
// budget. Caller holds t.mu.
func (t *mtpTunnel) chargeLocked(n int) {
	if t.rateCap <= 0 || n <= 0 {
		return
	}
	t.rollWindowLocked(time.Now())
	t.winBytes += n
}

func (t *mtpTunnel) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	conn := t.conn
	t.cond.Broadcast()
	t.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

// isResourceExhausted reports whether a dial failed because the process ran out
// of descriptors or ports, rather than because the far end was unreachable.
func isResourceExhausted(err error) bool {
	return errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.EADDRNOTAVAIL)
}

// ErrMTProtoDisabled is returned when wiring asks for a relay that is off.
var ErrMTProtoDisabled = errors.New("mtproto relay disabled")
