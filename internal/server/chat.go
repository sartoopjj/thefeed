package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// ChatService handles chat requests on the dedicated chat domain(s). Every
// query is one uniform sealed cell. A per-connection eph↔ek handshake yields a
// session (Ksession + a server-assigned selector); all later ops are sealed
// under Ksession, so the op, the destination and the read metadata are all
// encrypted. Sessions, uploads and handshake reassemblies are RAM-only and
// bounded; accounts/inboxes live in the ChatStore.
type ChatService struct {
	store    *ChatStore
	queryKey [protocol.KeySize]byte
	limits   protocol.ChatLimits
	domains  []string

	mu         sync.Mutex
	ek         *ecdh.PrivateKey // current session/encryption key
	ekPub      []byte
	prevEks    []*chatEk // rotated-out keys still inside their grace window
	ekSave     func(*ecdh.PrivateKey) error
	sessions   map[[protocol.ChatSelectorSize]byte]*chatSession
	handshakes map[[protocol.ChatSelectorSize]byte]*chatHandshake
	perAccount map[[protocol.AddressSize]byte]int

	statQueries   atomic.Int64
	statInits     atomic.Int64 // completed handshakes
	statCommits   atomic.Int64
	statRegisters atomic.Int64
	// statStaleCells counts cells refused because their counter was already
	// answered (or too far behind the window to tell). On a healthy link this
	// stays at or near zero; a rising count means duplicates are arriving
	// very late, which is worth seeing in the report.
	statStaleCells atomic.Int64
}

type chatSession struct {
	account   [protocol.AddressSize]byte
	ksession  [protocol.KeySize]byte
	created   time.Time
	lastSeen  time.Time
	upload    *chatUpload
	frag      *chatFragReasm // in-flight OP_FRAG reassembly (one op at a time)
	opMu      sync.Mutex     // serializes ops so concurrent retransmits can't race
	respCache chatRespCache  // sealed response bytes keyed by request counter
	// maxCounter / seenMask are the anti-replay window: maxCounter is the
	// highest counter answered, and seenMask records which of the
	// chatReplayWindow counters at or below it have been. Exact inside the
	// window, so out-of-order arrival is served normally and only a genuine
	// repeat is refused. See handleInContext.
	maxCounter uint32
	seenMask   uint64
	seenAny    bool
}

// chatReplayWindow is how far back the server can still tell "already
// answered" from "not yet seen". One bit per counter in seenMask.
//
// It only has to cover reordering between a cell's FIRST arrivals, and the
// client is strictly sequential -- exchange consumes one counter and awaits
// its answer before the next, including every chunk of an upload (see
// sendData) -- so in practice the gap is 0 and anything above 1 is margin.
// 64 is the free size: it is one word, and it leaves room for a future client
// that pipelines without this turning into dropped cells.
const chatReplayWindow = 64

// markCounter records that this counter is being answered and reports whether
// it already was. Caller holds c.mu.
//
// Inside the window the answer is exact, so genuine out-of-order arrival is
// served. Below the window the server cannot prove the counter is new, so it
// is treated as a repeat: anything that far behind is a stray duplicate or a
// replay, never a cell the client is still waiting on.
func (s *chatSession) markCounter(counter uint32) (replay bool) {
	if !s.seenAny {
		s.seenAny, s.maxCounter, s.seenMask = true, counter, 1
		return false
	}
	switch {
	case counter > s.maxCounter:
		if shift := counter - s.maxCounter; shift >= chatReplayWindow {
			s.seenMask = 1
		} else {
			s.seenMask = (s.seenMask << shift) | 1
		}
		s.maxCounter = counter
		return false
	default:
		back := s.maxCounter - counter
		if back >= chatReplayWindow {
			return true // too far behind to tell; refuse rather than reseal
		}
		if s.seenMask&(1<<back) != 0 {
			return true
		}
		s.seenMask |= 1 << back
		return false
	}
}

// chatRespCache caches the sealed response for each request counter so that a
// retransmitted request (same counter) gets the identical ciphertext — avoiding
// AES-CTR nonce reuse when server state changed between retransmits.
type chatRespCache struct {
	keys [chatRespCacheSize]uint32
	vals [chatRespCacheSize][]byte
	pos  int
}

const chatRespCacheSize = 16

func (c *chatRespCache) get(counter uint32) ([]byte, bool) {
	for i := range c.keys {
		if c.keys[i] == counter && c.vals[i] != nil {
			return c.vals[i], true
		}
	}
	return nil, false
}

func (c *chatRespCache) put(counter uint32, resp []byte) {
	c.keys[c.pos] = counter
	c.vals[c.pos] = resp
	c.pos = (c.pos + 1) % chatRespCacheSize
}

// chatFragReasm reassembles one oversized control op split across OP_FRAG cells.
// The client serializes fragmented ops (fragMu), so one buffer per session is
// enough.
type chatFragReasm struct {
	total int
	reasm *protocol.ChunkReassembler
}

// chatEk is a rotated-out server encryption key kept until retireAt so a client
// whose cached (signed) ChatInfo still names the old key can finish a handshake.
// Past retireAt it is dropped — and with it the ability to recompute any session
// key derived from it, giving the session layer forward secrecy at the rotation
// granularity. Live sessions are unaffected: their Ksession is derived once at
// handshake and cached on the session.
type chatEk struct {
	priv     *ecdh.PrivateKey
	pub      []byte
	retireAt time.Time
}

type chatUpload struct {
	dst       [protocol.AddressSize]byte
	totalLen  int
	chunkSize int // body bytes per DATA cell (client budget B - 2), fixed for the upload
	reasm     *protocol.ChunkReassembler
}

type chatHandshake struct {
	chunks  [][]byte // stream payloads by index
	created time.Time
}

const (
	// chatMaxSessions is a RAM-bound soft cap on concurrent live sessions.
	chatMaxSessions = 50000
	// chatMaxPerAccount bounds concurrent sessions per account (flood guard).
	chatMaxPerAccount = 32
	chatHandshakeTTL  = 30 * time.Second
	chatOpenSkew      = 10 * time.Minute
	chatSweepEvery    = 5 * time.Minute
	// chatEnvelopeOverhead bounds a SEND_START total length headroom over the
	// configured max message bytes (ver+seq+gcm tag+srvmac+cflag, plus slack).
	chatEnvelopeOverhead = 48

	// chatEkRotatePeriod is how often the server rotates its session/encryption
	// key, and chatEkGrace how long the rotated-out key keeps completing
	// handshakes. Grace exceeds the period (and dwarfs the ~1h client ChatInfo
	// refresh) so no client is ever locked out across a rotation; past grace the
	// old private key is dropped, giving the session layer forward secrecy.
	chatEkRotatePeriod = 7 * 24 * time.Hour
	chatEkGrace        = 8 * 24 * time.Hour
)

// RunEkRotation rotates the server encryption key on chatEkRotatePeriod, then
// re-publishes (re-signs) ChatInfo so clients learn the new key. Runs until ctx
// is done.
func (c *ChatService) RunEkRotation(ctx context.Context, republish func()) {
	t := time.NewTicker(chatEkRotatePeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := c.RotateEk(time.Now(), chatEkGrace); err != nil {
				log.Printf("[chat] ek rotation failed: %v", err)
				continue
			}
			republish()
			log.Printf("[chat] rotated server encryption key")
		}
	}
}

// NewChatService creates the chat handler. domains are the dedicated chat
// sub-domains (already validated against feed domains by the DNS layer).
func NewChatService(store *ChatStore, ek *ecdh.PrivateKey, queryKey [protocol.KeySize]byte, limits protocol.ChatLimits, domains []string) *ChatService {
	return &ChatService{
		store:      store,
		ek:         ek,
		ekPub:      ek.PublicKey().Bytes(),
		queryKey:   queryKey,
		limits:     limits,
		domains:    domains,
		sessions:   make(map[[protocol.ChatSelectorSize]byte]*chatSession),
		handshakes: make(map[[protocol.ChatSelectorSize]byte]*chatHandshake),
		perAccount: make(map[[protocol.AddressSize]byte]int),
	}
}

// Domains returns the chat sub-domains.
func (c *ChatService) Domains() []string { return c.domains }

// Info returns the ChatInfo payload advertised (signed) on the feed metadata
// path. EkPub is the current key; on rotation the caller re-publishes Info().
func (c *ChatService) Info() protocol.ChatInfo {
	c.mu.Lock()
	ekPub := append([]byte(nil), c.ekPub...)
	c.mu.Unlock()
	return protocol.ChatInfo{
		MinVersion: protocol.ChatProtocolVersion,
		MaxVersion: protocol.ChatProtocolVersion,
		Enabled:    true,
		Domains:    c.domains,
		EkPub:      ekPub,
		Limits:     c.limits,
	}
}

// SetEkPersist registers how a freshly rotated current key is saved to disk, so
// a restart loads the new key (not the pre-rotation one). Optional — tests leave
// it nil and rotate in RAM only.
func (c *ChatService) SetEkPersist(save func(*ecdh.PrivateKey) error) {
	c.mu.Lock()
	c.ekSave = save
	c.mu.Unlock()
}

// ekCandidatesLocked returns the keys a handshake may have used — current first,
// then any previous key still inside its grace window — and prunes the expired
// ones (dropping their private keys for forward secrecy). Caller holds mu.
func (c *ChatService) ekCandidatesLocked(now time.Time) []*chatEk {
	kept := c.prevEks[:0]
	for _, e := range c.prevEks {
		if now.Before(e.retireAt) {
			kept = append(kept, e)
		}
	}
	c.prevEks = kept
	out := make([]*chatEk, 0, 1+len(kept))
	out = append(out, &chatEk{priv: c.ek, pub: c.ekPub})
	return append(out, kept...)
}

// RotateEk swaps in a fresh current key, retires the old one into the grace
// window, persists the new key (if a saver is set), and returns the new public
// key so the caller can re-publish (re-sign) ChatInfo. Live sessions keep
// working; clients pick up the new key on their next ChatInfo refresh.
func (c *ChatService) RotateEk(now time.Time, grace time.Duration) ([]byte, error) {
	newKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.ekCandidatesLocked(now) // prune expired before retiring the old one
	c.prevEks = append(c.prevEks, &chatEk{priv: c.ek, pub: c.ekPub, retireAt: now.Add(grace)})
	c.ek, c.ekPub = newKey, newKey.PublicKey().Bytes()
	save := c.ekSave
	pub := append([]byte(nil), c.ekPub...)
	c.mu.Unlock()
	if save != nil {
		if err := save(newKey); err != nil {
			return pub, err
		}
	}
	return pub, nil
}

// RunSweeper periodically expires sessions, handshakes, old messages, and idle
// accounts until ctx is done.
func (c *ChatService) RunSweeper(ctx context.Context) {
	ticker := time.NewTicker(chatSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			c.mu.Lock()
			c.cleanupLocked(now)
			c.mu.Unlock()
			if msgs, accs, err := c.store.Sweep(now); err != nil {
				log.Printf("[chat] sweep: %v", err)
			} else if msgs > 0 || accs > 0 {
				log.Printf("[chat] sweep: expired %d message(s), deleted %d account(s)", msgs, accs)
			}
		}
	}
}

// StatsSnapshot returns the per-window counters for the hourly report and
// resets them. `accounts` and `sessions` are point-in-time gauges.
func (c *ChatService) StatsSnapshot() map[string]int64 {
	c.mu.Lock()
	sessions := int64(len(c.sessions))
	c.mu.Unlock()
	return map[string]int64{
		"queries":   c.statQueries.Swap(0),
		"inits":     c.statInits.Swap(0),
		"messages":  c.statCommits.Swap(0),
		"registers": c.statRegisters.Swap(0),
		"stale":     c.statStaleCells.Swap(0),
		"accounts":  int64(c.store.AccountCount()),
		"sessions":  sessions,
	}
}

func (c *ChatService) cleanupLocked(now time.Time) {
	idle := time.Duration(c.limits.SessionIdleSec) * time.Second
	hard := time.Duration(c.limits.SessionHardSec) * time.Second
	for ref, s := range c.sessions {
		if now.Sub(s.lastSeen) > idle || now.Sub(s.created) > hard {
			c.perAccount[s.account]--
			if c.perAccount[s.account] <= 0 {
				delete(c.perAccount, s.account)
			}
			delete(c.sessions, ref)
		}
	}
	for tag, h := range c.handshakes {
		if now.Sub(h.created) > chatHandshakeTTL {
			delete(c.handshakes, tag)
		}
	}
}

// HandleCell processes one decoded chat cell and returns the response bytes (to
// be wrapped by EncodeResponse). An empty slice means "incomplete handshake,
// keep going"; ChatSessionLostResp means "re-handshake".
func (c *ChatService) HandleCell(selector [protocol.ChatSelectorSize]byte, counter uint32, payload []byte, domain string, now time.Time) []byte {
	c.statQueries.Add(1)
	c.mu.Lock()
	sess := c.sessions[selector]
	c.mu.Unlock()
	if sess != nil {
		return c.handleInContext(sess, selector, counter, payload, now)
	}
	if protocol.ChatIsHandshakeSelector(selector) {
		return c.handleHandshakeCell(selector, uint8(counter), payload, domain, now)
	}
	// Unknown selector for an in-context cell → the session is gone (expiry or
	// reboot). Tell the client to re-handshake.
	return protocol.ChatSessionLostResp
}

// ---- handshake ----

// chatMaxHandshakeCells bounds how many cells one handshake may claim.
//
// The index is a byte, so one cell carrying index 255 grew the slice to 256
// entries: 6 KiB of slice headers for a ~50-byte query that proves nothing
// about who sent it. The tag table holds 50 000 of those, so 50 000 queries
// pinned ~300 MiB, and filling every slot reaches 14 KiB a tag. Held until a
// sweep that runs every few minutes, which an attacker simply works inside.
//
// A real handshake needs 12.
//
// COMPUTED from the protocol rather than written down, with a margin: a
// number typed here would be correct today and silently refuse real
// handshakes the moment an envelope grows. The largest stream is a Register:
// ephemeral key + version + kind + the sealed envelope.
var chatMaxHandshakeCells = func() int {
	boot := protocol.RegisterEnvelopeLen + protocol.ChatSealTagSize
	if auth := protocol.AddressSize + 4 + protocol.ChatAccountProofSize + protocol.ChatSealTagSize; auth > boot {
		boot = auth
	}
	total := protocol.X25519KeySize + 2 + boot
	cells := (total + protocol.ChatCellPayloadSize - 1) / protocol.ChatCellPayloadSize
	return cells + 3 // margin, so a small format change cannot break a handshake
}()

// add stores one cell of a handshake stream, reporting whether the index is
// one a real handshake could use.
func (h *chatHandshake) add(idx uint8, payload []byte) bool {
	if int(idx) >= chatMaxHandshakeCells {
		return false
	}
	for len(h.chunks) <= int(idx) {
		h.chunks = append(h.chunks, nil)
	}
	h.chunks[idx] = append([]byte(nil), payload...)
	return true
}

// assemble returns the full handshake stream once enough cells have arrived.
func (h *chatHandshake) assemble() ([]byte, bool) {
	if len(h.chunks) < 2 || h.chunks[0] == nil || h.chunks[1] == nil {
		return nil, false
	}
	// Stream = eph(32) ‖ proto_ver(1) ‖ kind(1) ‖ bootstrap. The kind byte is at
	// stream offset X25519KeySize+1, which lands in chunk[1].
	kindOff := protocol.X25519KeySize + 1 - protocol.ChatCellPayloadSize
	if kindOff < 0 || kindOff >= len(h.chunks[1]) {
		return nil, false
	}
	var bootLen int
	switch h.chunks[1][kindOff] {
	case protocol.ChatHandshakeAuth:
		bootLen = protocol.AddressSize + 4 + protocol.ChatAccountProofSize + protocol.ChatSealTagSize
	case protocol.ChatHandshakeRegister:
		bootLen = protocol.RegisterEnvelopeLen + protocol.ChatSealTagSize
	default:
		return nil, false
	}
	total := protocol.X25519KeySize + 2 + bootLen
	n := (total + protocol.ChatCellPayloadSize - 1) / protocol.ChatCellPayloadSize
	buf := make([]byte, 0, n*protocol.ChatCellPayloadSize)
	for i := 0; i < n; i++ {
		if i >= len(h.chunks) || h.chunks[i] == nil {
			return nil, false
		}
		buf = append(buf, h.chunks[i]...)
	}
	if len(buf) < total {
		return nil, false
	}
	return buf[:total], true
}

func (c *ChatService) handleHandshakeCell(setupTag [protocol.ChatSelectorSize]byte, idx uint8, payload []byte, domain string, now time.Time) []byte {
	c.mu.Lock()
	h := c.handshakes[setupTag]
	// The TTL is enforced HERE as well as in the sweep. The sweep runs every
	// few minutes, so without this a stale entry keeps collecting cells for
	// the whole gap -- and an attacker who knows the sweep interval simply
	// works inside it.
	if h != nil && now.Sub(h.created) > chatHandshakeTTL {
		delete(c.handshakes, setupTag)
		h = nil
	}
	if h == nil {
		if len(c.handshakes) >= chatMaxSessions {
			c.mu.Unlock()
			return []byte{} // shed load; client retries
		}
		h = &chatHandshake{created: now}
		c.handshakes[setupTag] = h
	}
	if !h.add(idx, payload) {
		c.mu.Unlock()
		return []byte{} // an index no handshake could use
	}
	stream, ok := h.assemble()
	if !ok {
		c.mu.Unlock()
		return []byte{} // incomplete — client keeps streaming / resending
	}
	delete(c.handshakes, setupTag)
	c.mu.Unlock()
	return c.completeHandshake(setupTag, stream, domain, now)
}

func (c *ChatService) completeHandshake(setupTag [protocol.ChatSelectorSize]byte, stream []byte, domain string, now time.Time) []byte {
	ephPub, protoVer, kind, sealedBoot, err := protocol.ParseChatHandshakeStream(stream)
	if err != nil {
		return []byte{}
	}
	// Only the version(s) this server speaks. A future multi-version server
	// branches its derivation/parsers here; today there is exactly one.
	if protoVer != protocol.ChatProtocolVersion {
		return []byte{}
	}
	// Try the current key first, then any previous key still inside its grace
	// window (a client whose cached signed ChatInfo predates a rotation seals its
	// bootstrap to the old ek).
	c.mu.Lock()
	cands := c.ekCandidatesLocked(now)
	c.mu.Unlock()
	var (
		ekPriv   *ecdh.PrivateKey
		ekPub    []byte
		ksession [protocol.KeySize]byte
		boot     []byte
	)
	for _, e := range cands {
		ks, derr := protocol.ChatSessionKey(e.priv, ephPub, protoVer, c.queryKey)
		if derr != nil {
			continue
		}
		b, oerr := protocol.OpenChat(ks, setupTag[:], protocol.ChatBootstrapCounter(), sealedBoot)
		if oerr != nil {
			continue
		}
		ekPriv, ekPub, ksession, boot = e.priv, e.pub, ks, b
		break
	}
	if ekPriv == nil {
		// No key opened it — wrong passphrase or corrupt; looks like a non-chat
		// query; drop.
		return []byte{}
	}

	// Every handshake reply (even errors) carries the server's unix time first,
	// so a clock-skewed client can correct its offset and retry the auth proof.
	hsResp := func(status byte, extra []byte) []byte {
		body := make([]byte, 4, 4+len(extra))
		binary.BigEndian.PutUint32(body, uint32(now.Unix()))
		body = append(body, extra...)
		return protocol.SealChatResponse(ksession, setupTag, protocol.ChatBootstrapCounter(), status, body)
	}

	var account [protocol.AddressSize]byte
	switch kind {
	case protocol.ChatHandshakeRegister:
		rec, e := protocol.ParseRegisterEnvelope(boot)
		if e != nil || rec.Verify() != nil {
			return hsResp(protocol.ChatStatusBadAuth, nil)
		}
		account = rec.Address()
		if err := c.store.Register(rec, boot, now); err != nil {
			if err == ErrChatAccountsFull {
				return hsResp(protocol.ChatStatusBusy, nil)
			}
			return hsResp(protocol.ChatStatusBusy, nil)
		}
		c.statRegisters.Add(1)
		return hsResp(protocol.ChatStatusRegistered, nil)
	case protocol.ChatHandshakeAuth:
		b, e := protocol.ParseChatAuthBootstrapPlain(boot)
		if e != nil {
			return hsResp(protocol.ChatStatusBadRequest, nil)
		}
		if diff := int64(b.TS) - now.Unix(); diff < -int64(chatOpenSkew.Seconds()) || diff > int64(chatOpenSkew.Seconds()) {
			return hsResp(protocol.ChatStatusBadAuth, nil)
		}
		_, encPub, ok, kerr := c.store.Keys(b.Addr, now)
		if kerr != nil {
			return hsResp(protocol.ChatStatusBusy, nil)
		}
		if !ok {
			return hsResp(protocol.ChatStatusUnknownSender, nil)
		}
		kss, kerr := protocol.ChatServerSharedKey(ekPriv, encPub, encPub, ekPub)
		if kerr != nil {
			return hsResp(protocol.ChatStatusBadRequest, nil)
		}
		if protocol.ChatAccountProof(kss, ephPub, b.Addr, b.TS, domain) != b.Proof {
			return hsResp(protocol.ChatStatusBadAuth, nil)
		}
		account = b.Addr
	default:
		return []byte{}
	}

	c.mu.Lock()
	if len(c.sessions) >= chatMaxSessions || c.perAccount[account] >= chatMaxPerAccount {
		c.mu.Unlock()
		return hsResp(protocol.ChatStatusBusy, nil)
	}
	ref := c.allocSelectorLocked()
	c.sessions[ref] = &chatSession{account: account, ksession: ksession, created: now, lastSeen: now}
	c.perAccount[account]++
	c.statInits.Add(1)
	c.mu.Unlock()

	ttl := uint16(c.limits.SessionHardSec)
	extra := make([]byte, 0, protocol.ChatSelectorSize+2)
	extra = append(extra, ref[:]...)
	extra = append(extra, byte(ttl>>8), byte(ttl))
	return hsResp(protocol.ChatStatusOK, extra)
}

// allocSelectorLocked mints a free, non-handshake session ref. Caller holds mu.
func (c *ChatService) allocSelectorLocked() [protocol.ChatSelectorSize]byte {
	var ref [protocol.ChatSelectorSize]byte
	for {
		_, _ = rand.Read(ref[:])
		protocol.ChatClearHandshakeSelector(&ref)
		if _, exists := c.sessions[ref]; !exists {
			return ref
		}
	}
}

// ---- in-context ops ----

func (c *ChatService) handleInContext(sess *chatSession, selector [protocol.ChatSelectorSize]byte, counter uint32, payload []byte, now time.Time) []byte {
	// In-context request counters live below the reserved regions (bootstrap /
	// response bit). A counter at/above bootstrap is a buggy or hostile client;
	// reject it rather than seal a response whose nonce could collide. (A forged
	// cell would fail the seal below anyway; this makes the invariant explicit.)
	if counter >= protocol.ChatBootstrapCounter() {
		return protocol.ChatSessionLostResp
	}
	pt, err := protocol.OpenChat(sess.ksession, selector[:], counter, payload)
	if err != nil {
		// Bad seal — corruption or a stale selector collision; ask to re-handshake.
		return protocol.ChatSessionLostResp
	}

	// Per-session lock serializes ops so that concurrent retransmits (the same
	// cell scattered across resolvers) cannot race past the cache check and
	// seal different responses under the same nonce.
	sess.opMu.Lock()
	defer sess.opMu.Unlock()

	c.mu.Lock()
	sess.lastSeen = now
	// Retransmit: if we already sealed a response for this counter, replay the
	// cached bytes — avoids AES-CTR nonce reuse when server state changed.
	if cached, ok := sess.respCache.get(counter); ok {
		c.mu.Unlock()
		return cached
	}
	// Answered before, but no longer in the cache above.
	//
	// INVARIANT: one nonce seals at most one plaintext. The seal is AES-CTR
	// and its nonce is fixed by (session key, selector, counter), so a second
	// response at the same counter must be byte-identical to the first or not
	// exist. The cache keeps that true for recent counters; this keeps it
	// true for older ones, where the cache can no longer answer.
	//
	// Dropped rather than answered with an error: a repeat on a live session
	// is a stray duplicate the client is no longer waiting on (it consumed
	// that counter once and moved on), or a deliberate one. Silence is what a
	// lost packet looks like, so the client's own retransmit logic copes --
	// and an error status would need that same nonce anyway.
	if replay := sess.markCounter(counter); replay {
		c.mu.Unlock()
		c.statStaleCells.Add(1)
		return []byte{}
	}
	c.mu.Unlock()

	resp := func(status byte, body []byte) []byte {
		sealed := protocol.SealChatResponse(sess.ksession, selector, counter, status, body)
		c.mu.Lock()
		sess.respCache.put(counter, sealed)
		c.mu.Unlock()
		return sealed
	}

	// Recover the per-cell budget B = plaintext length minus the deterministic
	// jitter the client folded in (RFC §8.2). OP_FRAG cells carry no jitter (its
	// chunk is concatenated, so trailing pad can't ride along). Reject anything
	// whose recovered budget is out of range. SendStart/Data read the chunk size
	// as B-2 from this.
	budget := len(pt)
	if protocol.ChatPlainOp(pt) != protocol.ChatOpFrag {
		budget -= protocol.ChatCellJitter(c.queryKey, selector, counter)
	}
	if budget < protocol.ChatCellPlainMin || budget > protocol.ChatCellPlainMax {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	return c.dispatch(sess, pt, budget, resp, now)
}

// dispatch routes one decoded op plaintext to its handler. Called directly with
// each cell's plaintext, and re-entered from opFrag with a reassembled op.
func (c *ChatService) dispatch(sess *chatSession, pt []byte, budget int, resp func(byte, []byte) []byte, now time.Time) []byte {
	switch protocol.ChatPlainOp(pt) {
	case protocol.ChatOpStatus:
		return c.opStatus(sess, resp, now)
	case protocol.ChatOpFetch:
		return c.opFetch(sess, pt, resp, now)
	case protocol.ChatOpAck:
		return c.opAck(sess, pt, resp, now)
	case protocol.ChatOpSendStatus:
		return c.opSendStatus(sess, pt, resp, now)
	case protocol.ChatOpKeyFetch:
		return c.opKeyFetch(pt, resp, now)
	case protocol.ChatOpSendStart:
		return c.opSendStart(sess, pt, budget, resp, now)
	case protocol.ChatOpData:
		return c.opData(sess, pt, resp)
	case protocol.ChatOpFin:
		return c.opFin(sess, pt, resp, now)
	case protocol.ChatOpFrag:
		return c.opFrag(sess, pt, budget, resp, now)
	default:
		return resp(protocol.ChatStatusBadRequest, nil)
	}
}

func appendQuota(body []byte, remaining uint16, reset uint32) []byte {
	body = append(body, byte(remaining>>8), byte(remaining))
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], reset)
	return append(body, b[:]...)
}

func (c *ChatService) opStatus(sess *chatSession, resp func(byte, []byte) []byte, now time.Time) []byte {
	entries, err := c.store.InboxStatus(sess.account, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	remaining, reset, _, err := c.store.SendQuota(sess.account, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if len(entries) > 255 {
		entries = entries[:255]
	}
	body := appendQuota(nil, remaining, reset)
	body = append(body, byte(len(entries)))
	for _, e := range entries {
		body = append(body, e.Src[:]...)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], e.Seq)
		body = append(body, b[:]...)
		body = append(body, byte(e.Len>>8), byte(e.Len), e.Blocks)
	}
	return resp(protocol.ChatStatusOK, body)
}

func (c *ChatService) opFetch(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	f, err := protocol.ParseChatFetchPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	src, ok, err := c.store.ResolvePeerHandle(sess.account, f.Peer, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	block, ok, err := c.store.FetchBlock(sess.account, src, f.Seq, f.Block, now)
	if err != nil || !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	return resp(protocol.ChatStatusOK, block)
}

func (c *ChatService) opAck(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	a, err := protocol.ParseChatAckPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	peer, ok, err := c.store.ResolvePeerHandle(sess.account, a.Peer, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	status, err := c.store.Ack(sess.account, peer, a.UpToSeq, a.Receipt[:], now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	return resp(status, nil)
}

func (c *ChatService) opSendStatus(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	s, err := protocol.ParseChatSendStatusPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	// Caller is the sender; counters live in the recipient's account.
	accepted, delivered, receipt, ok, err := c.store.PairState(s.Peer, sess.account, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	// accepted(4) ‖ delivered(4) ‖ receipt(6) = 14 ≤ one cell. The receipt is
	// the recipient's E2E proof of `delivered`; the server relays it untouched
	// (it can't forge one). Zero-pad when absent so the layout stays fixed.
	body := make([]byte, 8+protocol.ChatReceiptMACSize)
	binary.BigEndian.PutUint32(body, accepted)
	binary.BigEndian.PutUint32(body[4:], delivered)
	copy(body[8:], receipt)
	return resp(protocol.ChatStatusOK, body)
}

func (c *ChatService) opKeyFetch(pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	k, err := protocol.ParseChatKeyFetchPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	rec, ok, err := c.store.RegisterRecord(k.Addr, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if !ok {
		return resp(protocol.ChatStatusNotFound, nil)
	}
	return resp(protocol.ChatStatusOK, rec)
}

// opFrag accumulates one OP_FRAG cell; once the whole op has arrived it
// reassembles and dispatches it with the cell budget (so an upload's chunk size
// comes from the cell, not the reassembled SEND_START length).
func (c *ChatService) opFrag(sess *chatSession, pt []byte, budget int, resp func(byte, []byte) []byte, now time.Time) []byte {
	f, err := protocol.ParseChatFragPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	total := int(f.Total)
	if total < 1 || int(f.Index) >= total {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	c.mu.Lock()
	if sess.frag == nil || sess.frag.total != total {
		sess.frag = &chatFragReasm{total: total, reasm: protocol.NewChunkReassembler(total)}
	}
	sess.frag.reasm.Add(int(f.Index), f.Chunk)
	complete := sess.frag.reasm.Complete()
	var assembled []byte
	if complete {
		assembled = sess.frag.reasm.Assemble()
		sess.frag = nil
	}
	c.mu.Unlock()
	if !complete {
		return resp(protocol.ChatStatusFragMore, nil)
	}
	// The inner op is fixed-length and self-delimiting, so trailing seal pad in
	// the last fragment is ignored. A fragment-of-a-fragment is rejected.
	if protocol.ChatPlainOp(assembled) == protocol.ChatOpFrag {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	return c.dispatch(sess, assembled, budget, resp, now)
}

func (c *ChatService) opSendStart(sess *chatSession, pt []byte, budget int, resp func(byte, []byte) []byte, now time.Time) []byte {
	ss, err := protocol.ParseChatSendStartPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	maxLen := int(c.limits.MaxMsgBytes) + chatEnvelopeOverhead
	if ss.TotalLen == 0 || int(ss.TotalLen) > maxLen {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	status, remaining, reset, err := c.store.PrecheckMessage(sess.account, ss.Dst, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if status != protocol.ChatStatusOK {
		return resp(status, appendQuota(nil, remaining, reset))
	}
	// Chunk size is the client's budget B-2. budget is the CELL length (passed by
	// dispatch), not len(pt): a fragmented SEND_START reassembles to 15 bytes but
	// its DATA cells are still at the small budget.
	chunkSize := budget - 2
	chunks := (int(ss.TotalLen) + chunkSize - 1) / chunkSize
	if chunks < 1 || chunks > 255 {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	c.mu.Lock()
	// Resume the same in-progress message (same dst+len) so a retry skips
	// already-received chunks; a new session or different message starts fresh.
	if sess.upload == nil || sess.upload.dst != ss.Dst || sess.upload.totalLen != int(ss.TotalLen) {
		sess.upload = &chatUpload{dst: ss.Dst, totalLen: int(ss.TotalLen), chunkSize: chunkSize, reasm: protocol.NewChunkReassembler(chunks)}
	}
	bm := sess.upload.reasm.Bitmap()
	c.mu.Unlock()
	return resp(protocol.ChatStatusOK, append(appendQuota(nil, remaining, reset), bm...))
}

func (c *ChatService) opData(sess *chatSession, pt []byte, resp func(byte, []byte) []byte) []byte {
	d, err := protocol.ParseChatDataPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	c.mu.Lock()
	up := sess.upload
	if up == nil {
		c.mu.Unlock()
		return resp(protocol.ChatStatusUnknownSession, nil)
	}
	realLen := up.totalLen - int(d.Index)*up.chunkSize
	if realLen > up.chunkSize {
		realLen = up.chunkSize
	}
	if realLen < 0 || realLen > len(d.Chunk) {
		c.mu.Unlock()
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	up.reasm.Add(int(d.Index), d.Chunk[:realLen])
	bm := up.reasm.Bitmap()
	c.mu.Unlock()
	return resp(protocol.ChatStatusOK, bm)
}

func (c *ChatService) opFin(sess *chatSession, pt []byte, resp func(byte, []byte) []byte, now time.Time) []byte {
	fin, err := protocol.ParseChatFinPlain(pt)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	c.mu.Lock()
	up := sess.upload
	if up == nil {
		c.mu.Unlock()
		return resp(protocol.ChatStatusUnknownSession, nil)
	}
	if !up.reasm.Complete() {
		bm := up.reasm.Bitmap()
		c.mu.Unlock()
		return resp(protocol.ChatStatusIncomplete, bm)
	}
	assembled := up.reasm.Assemble()
	dst := up.dst
	sess.upload = nil
	c.mu.Unlock()

	if len(assembled) != up.totalLen || crc32.ChecksumIEEE(assembled) != fin.CRC32 {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	m, err := protocol.ParseChatMessage(assembled)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	_, encPub, ok, err := c.store.Keys(sess.account, now)
	if err != nil || !ok {
		return resp(protocol.ChatStatusUnknownSender, nil)
	}
	kss, err := protocol.ChatServerSharedKey(c.ek, encPub, encPub, c.ekPub)
	if err != nil {
		return resp(protocol.ChatStatusBadRequest, nil)
	}
	if m.VerifyServerMAC(kss, sess.account, dst) != nil {
		return resp(protocol.ChatStatusBadAuth, nil)
	}
	status, lastAccepted, remaining, reset, err := c.store.CommitMessage(sess.account, dst, m.Seq, assembled, now)
	if err != nil {
		return resp(protocol.ChatStatusBusy, nil)
	}
	if status == protocol.ChatStatusOK {
		c.statCommits.Add(1)
	}
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, lastAccepted)
	body = appendQuota(body, remaining, reset)
	return resp(status, body)
}
