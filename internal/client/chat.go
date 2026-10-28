package client

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	cryptoRand "crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Chat client driver: capability discovery (signed ChatInfo, fail-closed), a
// per-connection eph↔ek session (one handshake, then cheap sealed ops),
// registration (register handshake), the chunked upload session with
// union-bitmap selective repeat, inbox poll/fetch/decrypt, ack, and per-peer
// delivery status. The web layer owns persistence (seed, contact names) and
// rendering; this driver owns the wire. Every in-context query is one uniform
// ≤40-char sealed cell; an UnknownSession reply (TTL expiry / server reboot)
// triggers a single transparent re-handshake + retry.

// Chat errors. ChatStatusError carries a server-reported status.
var (
	ErrChatNoServerKey    = errors.New("chat requires a pinned server key (sk)")
	ErrChatDisabled       = errors.New("chat not enabled on this server")
	ErrChatServerDisabled = errors.New("chat is turned off by this server")
	// ErrChatNoResolvers means there is no healthy DNS resolver to send over.
	// A sentinel so the UI can say the link is down, rather than reporting the
	// catch-all "something went wrong" for the one failure a user can act on.
	ErrChatNoResolvers = errors.New("chat: no active resolvers")
	ErrChatUnverified  = errors.New("chat info signature missing or invalid")
	// ErrChatVersion means the server speaks a chat protocol this client can't
	// (e.g. an old server still on the previous wire) — a stable mismatch, so it
	// is backed off long and never retried in a tight loop.
	ErrChatVersion = errors.New("chat protocol version incompatible")
	// ErrChatUnreachable means a transport/handshake failure (e.g. the server
	// is rebooting) — transient, retry — not "no chat here".
	ErrChatUnreachable = errors.New("chat server unreachable")

	// errChatSessionLost is internal: the server doesn't know our session
	// (expiry/reboot) → re-handshake.
	errChatSessionLost = errors.New("chat: session lost")
)

// ChatStatusError is a non-OK chat response status.
type ChatStatusError struct {
	Op           byte
	Status       byte
	Remaining    uint16
	ResetUnix    uint32
	LastAccepted uint32
}

func (e *ChatStatusError) Error() string {
	return fmt.Sprintf("chat: op %d failed with status %d", e.Op, e.Status)
}

// ChatIdentity holds the seed-derived chat keys.
type ChatIdentity struct {
	Seed     []byte
	Identity ed25519.PrivateKey
	Enc      *ecdh.PrivateKey
	Addr     [protocol.AddressSize]byte
}

// NewChatIdentity derives a chat identity from a seed.
func NewChatIdentity(seed []byte) (*ChatIdentity, error) {
	id, err := protocol.DeriveIdentityKey(seed)
	if err != nil {
		return nil, err
	}
	enc, err := protocol.DeriveEncryptionKey(seed)
	if err != nil {
		return nil, err
	}
	return &ChatIdentity{
		Seed:     append([]byte(nil), seed...),
		Identity: id,
		Enc:      enc,
		Addr:     protocol.Address(id.Public().(ed25519.PublicKey)),
	}, nil
}

var chatAddrEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// ChatAddressString renders an address for sharing (lowercase base32, 20 chars).
func ChatAddressString(addr [protocol.AddressSize]byte) string {
	return strings.ToLower(chatAddrEnc.EncodeToString(addr[:]))
}

// ParseChatAddress parses a shared address string.
func ParseChatAddress(s string) ([protocol.AddressSize]byte, error) {
	var addr [protocol.AddressSize]byte
	raw, err := chatAddrEnc.DecodeString(strings.ToUpper(strings.TrimSpace(s)))
	if err != nil || len(raw) != protocol.AddressSize {
		return addr, fmt.Errorf("invalid chat address")
	}
	copy(addr[:], raw)
	return addr, nil
}

// CanonicalChatAddress round-trips a user-supplied address through decode+encode
// so padding bits are zeroed and the string is always the same for a given address.
func CanonicalChatAddress(s string) (string, error) {
	addr, err := ParseChatAddress(s)
	if err != nil {
		return "", err
	}
	return ChatAddressString(addr), nil
}

// ChatProgress reports upload/download progress: done out of total units.
type ChatProgress func(done, total int)

// ChatIncoming is one decrypted inbox message.
type ChatIncoming struct {
	From [protocol.AddressSize]byte
	Seq  uint32
	Text string
}

// ChatSendResult reports a committed send.
type ChatSendResult struct {
	Seq       uint32
	Remaining uint16
	ResetUnix uint32
}

// ChatClient drives the chat protocol for one identity on one server config.
type ChatClient struct {
	f  *Fetcher
	id *ChatIdentity

	// opSeq serializes whole multi-op sequences (send / fetch / ack / register)
	// on this client. They share one connection session (sessRef/ksession/
	// sendCounter); without this a concurrent poll's re-handshake could swap the
	// session out from under an in-progress upload, stranding it on a session
	// with no upload (FIN → unknown_session). Held around the full sequence, NOT
	// the per-op c.mu critical sections.
	opSeq sync.Mutex

	// fragMu serializes whole OP_FRAG sequences (a control op too big for the
	// budget, split across cells). The server reassembles one fragmented op at a
	// time per session, so two concurrent fragmented ops — e.g. an unserialized
	// PeerStatus racing a send — must not interleave. Independent of opSeq and
	// taken only when actually fragmenting (single-cell ops skip it).
	fragMu sync.Mutex

	mu          sync.Mutex
	info        *protocol.ChatInfo
	infoAt      time.Time
	infoStale   bool
	registered  bool
	clockOffset int64 // serverUnix - localUnix, learned from handshakes
	quotaRem    uint16
	quotaReset  uint32
	quotaKnown  bool
	keyCache    map[[protocol.AddressSize]byte]*protocol.RegisterEnvelope

	// per-connection session
	sessRef     [protocol.ChatSelectorSize]byte
	ksession    [protocol.KeySize]byte
	sessUp      bool
	sendCounter uint32
	// lastHandshake throttles re-handshakes: the session-lost sentinel (0xE5) is
	// unauthenticated, so an on-path attacker can inject it to force endless
	// re-handshakes. Spacing handshakes by chatRehandshakeMinGap caps that
	// amplification while still letting a genuine reboot/expiry recover.
	lastHandshake time.Time
	// onHandshake, if set, reports handshake cell progress (done, total). Set
	// it with SetOnHandshake: polls run in parallel, so the field is read from
	// one goroutine while another is assigning it.
	onHandshake ChatProgress
	// budget is the per-cell op-plaintext budget B (RFC §8.2): smaller = shorter
	// queries, more of them. The cell is self-describing by length, so the server
	// needs no negotiation; the value is clamped to [ChatCellPlainMin, Max].
	budget int
}

const (
	chatInfoMaxAge      = 1 * time.Hour
	chatControlAttempts = 6
	// chatProbeAttempts: ChatInfo fetch retries; high to ride out a flaky network.
	chatProbeAttempts   = 10
	chatCellAttempts    = 3
	chatMaxUploadRounds = 20
	chatMaxFinRounds    = 6
	chatMaxRestarts     = 3

	// chatSendUploadAttempts caps whole-send retries (idempotent by seq); high so
	// a cold start on a bad network still gets through.
	chatSendUploadAttempts = 20

	// chatMaxSessionCounter forces a re-handshake before the per-session op
	// counter can reach the reserved counter regions (bootstrap 0x400000,
	// response bit 0x800000) whose nonces would otherwise collide with a live
	// request's — AES-CTR keystream reuse. The margin below 0x400000 dwarfs the
	// most ops any single op() can issue (a max upload is ≤ a few thousand), so
	// the proactive re-handshake never lands mid-operation.
	chatMaxSessionCounter = 0x3F0000

	// chatRehandshakeMinGap is the minimum spacing between handshakes, throttling
	// an injected-0xE5 loop (see ChatClient.lastHandshake).
	chatRehandshakeMinGap = time.Second
)

// NewChatClient creates a chat driver bound to a fetcher and identity.
func NewChatClient(f *Fetcher, id *ChatIdentity) *ChatClient {
	return &ChatClient{
		f:        f,
		id:       id,
		keyCache: make(map[[protocol.AddressSize]byte]*protocol.RegisterEnvelope),
		budget:   protocol.ChatCellPlainSize, // default = today's wire
	}
}

// chunkSize is the message-body bytes carried by one DATA cell at the current
// budget (op(1) + idx(1) + chunk = budget).
func (c *ChatClient) chunkSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.budget - 2
}

// SetOnHandshake sets the handshake-progress callback. Guarded: parallel polls
// read it while the UI re-points it at a new progress sink.
func (c *ChatClient) SetOnHandshake(p ChatProgress) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onHandshake = p
}

// onHandshakeFunc returns the current handshake-progress callback, or nil.
func (c *ChatClient) onHandshakeFunc() ChatProgress {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onHandshake
}

// SetBudget sets the per-cell op-plaintext budget B (clamped to the valid
// range). Smaller B = shorter query names but more cells per message.
func (c *ChatClient) SetBudget(b int) {
	if b < protocol.ChatCellPlainMin {
		b = protocol.ChatCellPlainMin
	}
	if b > protocol.ChatCellPlainMax {
		b = protocol.ChatCellPlainMax
	}
	c.mu.Lock()
	c.budget = b
	c.mu.Unlock()
}

// Identity returns the client identity.
func (c *ChatClient) Identity() *ChatIdentity { return c.id }

// Quota returns the latest server-reported send quota.
func (c *ChatClient) Quota() (remaining uint16, resetUnix uint32, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.quotaRem, c.quotaReset, c.quotaKnown
}

// Registered reports whether this identity established a session (and thus
// registered) on the server during this process lifetime.
func (c *ChatClient) Registered() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registered
}

func (c *ChatClient) serverNow() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint32(time.Now().Unix() + c.clockOffset)
}

func (c *ChatClient) updateClock(serverUnix uint32) {
	if serverUnix == 0 {
		return
	}
	c.mu.Lock()
	c.clockOffset = int64(serverUnix) - time.Now().Unix()
	c.mu.Unlock()
}

func (c *ChatClient) updateQuota(remaining uint16, reset uint32) {
	c.mu.Lock()
	c.quotaRem, c.quotaReset, c.quotaKnown = remaining, reset, true
	c.mu.Unlock()
}

// EnsureInfo fetches and verifies the ChatInfo capability payload once (and
// re-fetches after expiry or when marked stale). Fail-closed: chat requires a
// pinned sk and a VERIFIED signature.
func (c *ChatClient) EnsureInfo(ctx context.Context) (*protocol.ChatInfo, error) {
	c.mu.Lock()
	if c.info != nil && !c.infoStale && time.Since(c.infoAt) < chatInfoMaxAge {
		info := c.info
		c.mu.Unlock()
		return info, nil
	}
	seenBefore := c.info != nil
	c.mu.Unlock()

	if c.f.serverPubKey == nil {
		return nil, ErrChatNoServerKey
	}

	block0, err := c.f.fetchBlockAttempts(ctx, protocol.ChatInfoChannel, 0, chatProbeAttempts)
	if err != nil {
		return nil, ErrChatUnreachable
	}
	if len(block0) < 2 {
		return nil, ErrChatUnreachable
	}
	totalBlocks := int(binary.BigEndian.Uint16(block0))
	if totalBlocks < 1 {
		totalBlocks = 1
	}
	const maxChatInfoBlocks = 8
	if totalBlocks > maxChatInfoBlocks {
		totalBlocks = maxChatInfoBlocks
	}

	blocks := make([][]byte, totalBlocks)
	blocks[0] = block0
	var extraRaw []byte
	bctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var blkErr error
	var errMu sync.Mutex
	for i := 1; i < totalBlocks; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b, err := c.f.FetchBlock(bctx, protocol.ChatInfoChannel, uint16(idx))
			if err != nil {
				errMu.Lock()
				if blkErr == nil {
					blkErr = err
					cancel()
				}
				errMu.Unlock()
				return
			}
			blocks[idx] = b
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if b, err := c.f.fetchBlockAttempts(bctx, protocol.ChatInfoChannel, uint16(totalBlocks), chatProbeAttempts); err == nil {
			extraRaw = b
		}
	}()
	wg.Wait()
	cancel()
	if blkErr != nil {
		return nil, ErrChatUnreachable
	}

	var raw []byte
	for _, b := range blocks {
		raw = append(raw, b...)
	}
	if verr := c.f.verifyExtraBytes(protocol.ChatInfoChannel, raw, extraRaw); verr != nil {
		// Active tamper (bad signature / rollback / content mismatch) is ALWAYS
		// fail-closed — we never serve unverified info.
		if errors.Is(verr, ErrExtraBlockInvalid) {
			return nil, ErrChatUnverified
		}
		// Otherwise the signature block was absent/unfetchable. On FIRST contact
		// we can't tell a transient fetch failure from a malicious signature-strip,
		// so fail-closed. But once we've verified this server's signature before
		// (seenBefore), we know it signs — so a later absence is a transient glitch:
		// classify it unreachable (short retry backoff) rather than the 30-min
		// "disabled" one that silenced working servers.
		if seenBefore {
			return nil, ErrChatUnreachable
		}
		return nil, ErrChatUnverified
	}

	info, err := protocol.ParseChatInfo(raw[2:])
	if err != nil {
		return nil, ErrChatDisabled
	}
	if !info.Enabled {
		return nil, ErrChatServerDisabled
	}
	if len(info.EkPub) != protocol.X25519KeySize || len(info.Domains) == 0 {
		return nil, ErrChatDisabled
	}
	if protocol.ChatProtocolVersion < info.MinVersion || protocol.ChatProtocolVersion > info.MaxVersion {
		return nil, fmt.Errorf("%w: server requires %d-%d", ErrChatVersion, info.MinVersion, info.MaxVersion)
	}

	c.mu.Lock()
	c.info = info
	c.infoAt = time.Now()
	c.infoStale = false
	c.mu.Unlock()
	return info, nil
}

// chatDomain picks the chat domain for unit i, rotated by attempt.
func chatDomain(info *protocol.ChatInfo, i, attempt int) string {
	return info.Domains[(i+attempt)%len(info.Domains)]
}

// ---- session ----

func (c *ChatClient) setSession(ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte) {
	c.mu.Lock()
	c.sessRef = ref
	c.ksession = ks
	c.sessUp = true
	c.sendCounter = 0
	c.registered = true
	c.mu.Unlock()
}

func (c *ChatClient) invalidateSession() {
	c.mu.Lock()
	c.sessUp = false
	c.mu.Unlock()
}

// ensureSession establishes the per-connection session if needed: an auth
// handshake (assuming we're registered), falling back to a register handshake
// on UnknownSender, with one clock-corrected retry on a skew BadAuth.
func (c *ChatClient) ensureSession(ctx context.Context, info *protocol.ChatInfo) error {
	c.mu.Lock()
	// A near-exhausted counter is treated as "no session" so we re-handshake
	// before any op (covering multi-exchange uploads) rather than mid-stream.
	up := c.sessUp && c.sendCounter < chatMaxSessionCounter
	last := c.lastHandshake
	c.mu.Unlock()
	if up {
		return nil
	}
	// Throttle re-handshakes so an injected session-lost (0xE5) can't drive a
	// tight handshake/register loop. A first handshake (zero lastHandshake) and a
	// genuine, spaced-out recovery are unaffected.
	if wait := chatRehandshakeMinGap - time.Since(last); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	c.mu.Lock()
	c.lastHandshake = time.Now()
	c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		ref, ks, status, srvUnix, err := c.handshake(ctx, info, protocol.ChatHandshakeAuth)
		if err != nil {
			return err
		}
		c.updateClock(srvUnix)
		switch status {
		case protocol.ChatStatusOK:
			c.setSession(ref, ks)
			return nil
		case protocol.ChatStatusUnknownSender:
			rref, rks, rst, rsu, rerr := c.handshake(ctx, info, protocol.ChatHandshakeRegister)
			if rerr != nil {
				return rerr
			}
			c.updateClock(rsu)
			if rst != protocol.ChatStatusOK {
				return &ChatStatusError{Status: rst}
			}
			c.f.log("[chat] registered %s…", ChatAddressString(c.id.Addr)[:8])
			c.setSession(rref, rks)
			return nil
		case protocol.ChatStatusBadAuth:
			// Clock skew — srvUnix corrected our offset; retry auth once.
			continue
		default:
			return &ChatStatusError{Status: status}
		}
	}
	return ErrChatUnreachable
}

// handshake runs one eph↔ek handshake of the given kind and returns the
// server's status, server time, and (on OK) the session ref + key.
func (c *ChatClient) handshake(ctx context.Context, info *protocol.ChatInfo, kind byte) (ref [protocol.ChatSelectorSize]byte, ks [protocol.KeySize]byte, status byte, serverUnix uint32, err error) {
	eph, err := protocol.GenerateEphemeralKey()
	if err != nil {
		return
	}
	ks, err = protocol.ChatSessionKey(eph, info.EkPub, protocol.ChatProtocolVersion, c.f.queryKey)
	if err != nil {
		return
	}
	var tag [protocol.ChatSelectorSize]byte
	if _, err = cryptoRand.Read(tag[:]); err != nil {
		return
	}
	protocol.ChatMarkHandshakeSelector(&tag)
	domain := chatDomain(info, 0, 0)

	var sealedBoot []byte
	switch kind {
	case protocol.ChatHandshakeAuth:
		ts := c.serverNow()
		kss, kerr := protocol.ChatServerSharedKey(c.id.Enc, info.EkPub, c.id.Enc.PublicKey().Bytes(), info.EkPub)
		if kerr != nil {
			err = kerr
			return
		}
		proof := protocol.ChatAccountProof(kss, eph.PublicKey().Bytes(), c.id.Addr, ts, domain)
		boot := protocol.BuildChatAuthBootstrapPlain(c.id.Addr, ts, proof)
		sealedBoot = protocol.SealChat(ks, tag[:], protocol.ChatBootstrapCounter(), boot)
	case protocol.ChatHandshakeRegister:
		rec, rerr := protocol.EncodeRegisterEnvelope(c.id.Identity, c.id.Enc.PublicKey().Bytes(), c.serverNow())
		if rerr != nil {
			err = rerr
			return
		}
		sealedBoot = protocol.SealChat(ks, tag[:], protocol.ChatBootstrapCounter(), rec)
	default:
		err = fmt.Errorf("chat: bad handshake kind")
		return
	}

	stream := protocol.BuildChatHandshakeStream(eph.PublicKey().Bytes(), protocol.ChatProtocolVersion, kind, sealedBoot)
	n := (len(stream) + protocol.ChatCellPayloadSize - 1) / protocol.ChatCellPayloadSize
	for round := 0; round < chatControlAttempts; round++ {
		if round > 0 {
			select {
			case <-ctx.Done():
				err = ctx.Err()
				return
			case <-time.After(time.Duration(round) * 300 * time.Millisecond):
			}
		}
		var last []byte
		for i := 0; i < n; i++ {
			start := i * protocol.ChatCellPayloadSize
			end := start + protocol.ChatCellPayloadSize
			if end > len(stream) {
				end = len(stream)
			}
			chunk := make([]byte, protocol.ChatCellPayloadSize)
			nn := copy(chunk, stream[start:end])
			if nn < len(chunk) {
				// Random-pad the last cell's tail (the server reads only `total`
				// bytes, computed from the kind byte): a zero pad would leave a
				// visible zero run in the QNAME.
				if _, re := cryptoRand.Read(chunk[nn:]); re != nil {
					err = re
					return
				}
			}
			qname, qe := protocol.EncodeChatCell(c.f.queryKey, c.f.QueryMode(), tag, uint32(i), chunk, domain)
			if qe != nil {
				err = qe
				return
			}
			if data, de := c.sendQuery(ctx, qname); de == nil {
				last = data
			}
			if onHS := c.onHandshakeFunc(); onHS != nil {
				onHS(i+1, n)
			}
		}
		if st, body, oe := protocol.OpenChatResponse(ks, tag, protocol.ChatBootstrapCounter(), last); oe == nil {
			if len(body) >= 4 {
				serverUnix = binary.BigEndian.Uint32(body)
			}
			if st == protocol.ChatStatusOK && len(body) >= 4+protocol.ChatSelectorSize {
				copy(ref[:], body[4:4+protocol.ChatSelectorSize])
			}
			return ref, ks, st, serverUnix, nil
		}
	}
	err = ErrChatUnreachable
	return
}

// sendQuery sends one chat cell query and returns the decoded response payload.
func (c *ChatClient) sendQuery(ctx context.Context, qname string) ([]byte, error) {
	if err := c.f.rateWait(ctx); err != nil {
		return nil, err
	}
	picked := c.f.pickWeightedResolvers(c.f.scatter)
	if len(picked) == 0 {
		return nil, ErrChatNoResolvers
	}
	if c.f.debug {
		c.f.log("[chat] query qname=%s resolvers=[%s]", qname, strings.Join(picked, ", "))
	}
	return c.f.scatterQuery(ctx, picked, qname)
}

// exchange seals and sends one in-context op, returning the response status and
// body. errChatSessionLost means the server doesn't know our session.
func (c *ChatClient) exchange(ctx context.Context, plaintext []byte) (status byte, body []byte, err error) {
	c.mu.Lock()
	if !c.sessUp || c.info == nil {
		c.mu.Unlock()
		return 0, nil, errChatSessionLost
	}
	// Hard stop: never seal at a counter that reaches the reserved regions
	// (bootstrap/response). ensureSession re-handshakes well before this, so in
	// practice it only fires if some path drove the counter here directly.
	if c.sendCounter >= protocol.ChatBootstrapCounter() {
		c.mu.Unlock()
		return 0, nil, errChatSessionLost
	}
	ref := c.sessRef
	ks := c.ksession
	ctr := c.sendCounter
	c.sendCounter++
	budget := c.budget
	domain := chatDomain(c.info, int(ctr), 0)
	c.mu.Unlock()

	// Deterministic per-cell jitter: pad to budget+j so cells vary in length
	// (blending with the feed), keyed by (selector, counter) so a retransmit is
	// byte-identical and resolver-cacheable. OP_FRAG cells get no jitter — their
	// chunk is concatenated server-side, so trailing pad can't ride along.
	padded := budget
	if protocol.ChatPlainOp(plaintext) != protocol.ChatOpFrag {
		padded += protocol.ChatCellJitter(c.f.queryKey, ref, ctr)
	}
	payload, err := protocol.SealChatCellPayloadN(ks, ref, ctr, plaintext, padded)
	if err != nil {
		return 0, nil, err
	}
	qname, err := protocol.EncodeChatCell(c.f.queryKey, c.f.QueryMode(), ref, ctr, payload, domain)
	if err != nil {
		return 0, nil, err
	}
	// Retransmit the identical cell on transport loss (UDP has no delivery
	// guarantee): every op is idempotent and the server handles each counter
	// statelessly, so a duplicate — or a retry whose first copy did arrive —
	// just re-runs the op and returns the same answer.
	var data []byte
	for attempt := 0; ; attempt++ {
		data, err = c.sendQuery(ctx, qname)
		if err == nil {
			break
		}
		if attempt+1 >= chatCellAttempts {
			return 0, nil, err
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	if protocol.ChatIsSessionLost(data) {
		return 0, nil, errChatSessionLost
	}
	st, b, oe := protocol.OpenChatResponse(ks, ref, ctr, data)
	if oe != nil {
		return 0, nil, errChatSessionLost
	}
	return st, b, nil
}

// sendOp sends one in-context op, fragmenting it across OP_FRAG cells when its
// plaintext exceeds the current budget (Compact, B<op size). A single-cell op
// goes straight through exchange. The fragmenting path is serialized by fragMu
// so concurrent oversized ops can't interleave on the server's per-session
// reassembly buffer; only the completing fragment returns the inner op result.
func (c *ChatClient) sendOp(ctx context.Context, plaintext []byte) (byte, []byte, error) {
	c.mu.Lock()
	budget := c.budget
	c.mu.Unlock()
	if len(plaintext) <= budget {
		return c.exchange(ctx, plaintext)
	}
	frags := protocol.SplitChunks(plaintext, budget-3) // op+idx+total header
	if len(frags) > 255 {
		return 0, nil, fmt.Errorf("chat: op too large to fragment (%d cells)", len(frags))
	}
	c.fragMu.Lock()
	defer c.fragMu.Unlock()
	for i, frag := range frags {
		st, body, err := c.exchange(ctx, protocol.BuildChatFragPlain(uint8(i), uint8(len(frags)), frag))
		if err != nil {
			return 0, nil, err
		}
		if i == len(frags)-1 {
			return st, body, nil // completing fragment carries the inner op's response
		}
		if st != protocol.ChatStatusFragMore {
			return st, body, nil // unexpected early completion/error — surface it
		}
	}
	return 0, nil, fmt.Errorf("chat: fragmentation produced no cells")
}

// op ensures a session, runs one in-context op, and transparently re-handshakes
// once on session loss (reboot/expiry). A second loss → ErrChatUnreachable.
func (c *ChatClient) op(ctx context.Context, info *protocol.ChatInfo, plaintext []byte) (byte, []byte, error) {
	if err := c.ensureSession(ctx, info); err != nil {
		return 0, nil, err
	}
	st, body, err := c.sendOp(ctx, plaintext)
	if errors.Is(err, errChatSessionLost) {
		c.invalidateSession()
		if err = c.ensureSession(ctx, info); err != nil {
			return 0, nil, err
		}
		st, body, err = c.sendOp(ctx, plaintext)
		if errors.Is(err, errChatSessionLost) {
			return 0, nil, ErrChatUnreachable
		}
	}
	if err != nil {
		return 0, nil, err
	}
	return st, body, nil
}

// markBitmap unions a server bitmap into acked; returns the new count.
func markBitmap(bm []byte, acked []bool, count *int) {
	for i := range acked {
		if i/8 < len(bm) && bm[i/8]&(1<<(7-uint(i%8))) != 0 && !acked[i] {
			acked[i] = true
			*count++
		}
	}
}

func parseQuota(body []byte) (remaining uint16, reset uint32) {
	if len(body) >= 6 {
		remaining = binary.BigEndian.Uint16(body)
		reset = binary.BigEndian.Uint32(body[2:])
	}
	return
}

// Register establishes a session (registering the identity on first contact).
func (c *ChatClient) Register(ctx context.Context, _ ChatProgress) error {
	c.opSeq.Lock()
	defer c.opSeq.Unlock()
	info, err := c.EnsureInfo(ctx)
	if err != nil {
		return err
	}
	return c.ensureSession(ctx, info)
}

// FetchPeerKey returns the verified registration record for addr (cached). The
// hash(identity)==address check defeats key substitution by the server.
func (c *ChatClient) FetchPeerKey(ctx context.Context, addr [protocol.AddressSize]byte) (*protocol.RegisterEnvelope, error) {
	c.mu.Lock()
	if rec, ok := c.keyCache[addr]; ok {
		c.mu.Unlock()
		return rec, nil
	}
	c.mu.Unlock()

	info, err := c.EnsureInfo(ctx)
	if err != nil {
		return nil, err
	}
	st, body, err := c.op(ctx, info, protocol.BuildChatKeyFetchPlain(addr))
	if err != nil {
		return nil, err
	}
	if st != protocol.ChatStatusOK {
		return nil, &ChatStatusError{Op: protocol.ChatOpKeyFetch, Status: st}
	}
	rec, err := protocol.ParseRegisterEnvelope(body)
	if err != nil {
		return nil, err
	}
	if err := rec.Verify(); err != nil {
		return nil, err
	}
	if rec.Address() != addr {
		return nil, fmt.Errorf("chat: key record does not match address")
	}
	c.mu.Lock()
	c.keyCache[addr] = rec
	c.mu.Unlock()
	return rec, nil
}

// IsRegistered reports whether peer can be messaged here. (false, nil) = no
// record; a non-nil error is transient.
func (c *ChatClient) IsRegistered(ctx context.Context, peer [protocol.AddressSize]byte) (bool, error) {
	if _, err := c.FetchPeerKey(ctx, peer); err != nil {
		var serr *ChatStatusError
		if errors.As(err, &serr) &&
			(serr.Status == protocol.ChatStatusNotFound || serr.Status == protocol.ChatStatusUnknownRecipient) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// chatPermanentSendErr reports whether a send error is final (retrying is futile).
// ErrChatDisabled is excluded on purpose: EnsureInfo returns it for a transient
// first-contact failure, so it must stay retryable.
func chatPermanentSendErr(err error) bool {
	if errors.Is(err, ErrChatNoServerKey) ||
		errors.Is(err, ErrChatServerDisabled) || errors.Is(err, ErrChatUnverified) {
		return true
	}
	var serr *ChatStatusError
	return errors.As(err, &serr)
}

// SendMessage encrypts and uploads one message. seq must be greater than the
// pair's last accepted seq (use NextSeq to recover it).
func (c *ChatClient) SendMessage(ctx context.Context, peer [protocol.AddressSize]byte, seq uint32, text string, progress ChatProgress) (*ChatSendResult, error) {
	if len(text) == 0 {
		return nil, fmt.Errorf("chat: empty message")
	}
	c.opSeq.Lock()
	defer c.opSeq.Unlock()

	// Clamp progress to a high-water mark so restarts/retries never go backwards.
	mono := progress
	if progress != nil {
		hi := 0
		mono = func(done, total int) {
			if done < hi {
				done = hi
			} else {
				hi = done
			}
			progress(done, total)
		}
	}

	// Retry the whole send (info, key, handshake, upload), not just the upload.
	// Idempotent by seq; after a failed upload, reconcile via PeerStatus so a
	// dropped FIN-OK counts as delivered.
	var (
		lastErr error
		info    *protocol.ChatInfo
		env     []byte
	)
	for attempt := 0; attempt < chatSendUploadAttempts; attempt++ {
		// Build the prerequisites once; the cached fetches make later passes cheap.
		if env == nil {
			nfo, ierr := c.EnsureInfo(ctx)
			if ierr != nil {
				if chatPermanentSendErr(ierr) {
					return nil, ierr // no key / chat off / unverified — retrying won't help
				}
				lastErr = ierr
			} else if len(text) > int(nfo.Limits.MaxMsgBytes) {
				return nil, fmt.Errorf("chat: message must be 1-%d bytes", nfo.Limits.MaxMsgBytes)
			} else if peerRec, perr := c.FetchPeerKey(ctx, peer); perr != nil {
				if chatPermanentSendErr(perr) {
					return nil, perr // unknown recipient (keyfetch not_found) — permanent
				}
				lastErr = perr
			} else {
				contentKey, e := protocol.ChatContentKey(c.id.Enc, peerRec.EncPub, c.id.Addr, peer, seq)
				if e != nil {
					return nil, e
				}
				kss, e := protocol.ChatServerSharedKey(c.id.Enc, nfo.EkPub, c.id.Enc.PublicKey().Bytes(), nfo.EkPub)
				if e != nil {
					return nil, e
				}
				if env, e = protocol.EncodeChatMessage(contentKey, kss, c.id.Addr, peer, seq, text); e != nil {
					return nil, e
				}
				info = nfo
				cs := c.chunkSize()
				c.f.log("[chat] sending to %s seq=%d (%dB, %d blocks)…",
					ChatAddressString(peer)[:8], seq, len(env), (len(env)+cs-1)/cs)
			}
		}

		if env != nil {
			st, lastAccepted, remaining, reset, uerr := c.upload(ctx, info, peer, env, mono)
			if uerr == nil {
				if st == protocol.ChatStatusOK {
					c.updateQuota(remaining, reset)
					c.f.log("[chat] sent to %s seq=%d (%d sends left this hour)", ChatAddressString(peer)[:8], seq, remaining)
					return &ChatSendResult{Seq: seq, Remaining: remaining, ResetUnix: reset}, nil
				}
				// Authoritative rejection (quota/replay/…): surface it, don't retry.
				// Drop the session so a later same-dst+len message can't resume onto
				// a partial upload a SEND_START-stage reject left behind.
				c.invalidateSession()
				serr := &ChatStatusError{Op: protocol.ChatOpFin, Status: st, Remaining: remaining, ResetUnix: reset}
				if st == protocol.ChatStatusReplay {
					serr.LastAccepted = lastAccepted
				}
				return nil, serr
			}
			lastErr = uerr
			// Did the message land despite the transport error? (lost FIN-OK)
			if acc, _, perr := c.peerStatus(ctx, peer); perr == nil && acc >= seq {
				rem, rst, _ := c.Quota()
				c.f.log("[chat] send to %s seq=%d confirmed on server after transport error", ChatAddressString(peer)[:8], seq)
				return &ChatSendResult{Seq: seq, Remaining: rem, ResetUnix: rst}, nil
			}
		}

		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		if attempt+1 >= chatSendUploadAttempts {
			break
		}
		backoff := time.Duration(attempt+1) * 500 * time.Millisecond
		if backoff > 2*time.Second {
			backoff = 2 * time.Second // capped so 20 attempts stay reasonably snappy
		}
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
		case <-time.After(backoff):
			continue
		}
		break
	}
	// Gave up mid-upload: drop the session so a different next message can't
	// resume onto the leftover partial.
	c.invalidateSession()
	if lastErr == nil {
		lastErr = ErrChatUnreachable
	}
	return nil, lastErr
}

// upload runs SEND_START / DATA (selective repeat) / FIN against the session,
// restarting from SEND_START if the session is lost mid-upload.
func (c *ChatClient) upload(ctx context.Context, info *protocol.ChatInfo, peer [protocol.AddressSize]byte, env []byte, progress ChatProgress) (status byte, lastAccepted uint32, remaining uint16, reset uint32, err error) {
	chunks := protocol.SplitChunks(env, c.chunkSize())
	total := len(chunks)
	crc := crc32.ChecksumIEEE(env)
	report := func(done int) {
		if progress != nil {
			progress(done, total+1)
		}
	}
	report(0)

	sendData := func(acked []bool, count *int) bool { // returns false on session loss
		for i := 0; i < total; i++ {
			if acked[i] {
				continue
			}
			d, _ := protocol.BuildChatDataPlain(uint8(i), chunks[i])
			st, body, e := c.exchange(ctx, d)
			if errors.Is(e, errChatSessionLost) {
				return false
			}
			if e != nil {
				continue // retried next round
			}
			if st == protocol.ChatStatusUnknownSession {
				return false // session swapped/evicted: restart from SEND_START
			}
			if st == protocol.ChatStatusOK {
				before := *count
				markBitmap(body, acked, count)
				if *count != before {
					report(*count)
				}
			}
		}
		return true
	}

	for restart := 0; restart < chatMaxRestarts; restart++ {
		st, body, e := c.op(ctx, info, protocol.BuildChatSendStartPlain(peer, uint16(len(env))))
		if e != nil {
			return 0, 0, 0, 0, e
		}
		if st != protocol.ChatStatusOK {
			remaining, reset = parseQuota(body)
			return st, 0, remaining, reset, nil
		}
		acked := make([]bool, total)
		ackedCount := 0
		if len(body) >= 6 {
			markBitmap(body[6:], acked, &ackedCount)
		}

		lost := false
		for round := 0; round < chatMaxUploadRounds && ackedCount < total; round++ {
			if !sendData(acked, &ackedCount) {
				lost = true
				break
			}
		}
		if lost {
			c.invalidateSession()
			continue // re-handshake on the next op and restart the upload
		}
		if ackedCount < total {
			return 0, 0, 0, 0, fmt.Errorf("chat: upload did not complete")
		}

		for fr := 0; fr < chatMaxFinRounds; fr++ {
			st, body, e := c.op(ctx, info, protocol.BuildChatFinPlain(crc))
			if e != nil {
				return 0, 0, 0, 0, e
			}
			if st == protocol.ChatStatusUnknownSession {
				// The session was replaced under us (a concurrent poll/ack
				// re-handshaked, or it was evicted) so its upload is gone. But the
				// FIN may already have committed (a lost FIN-OK), so return rather
				// than re-uploading: SendMessage reconciles via PeerStatus first and
				// only re-sends if the message truly didn't land. Avoids the upload
				// progress sticking near the end while the message is in fact
				// delivered.
				c.invalidateSession()
				return 0, 0, 0, 0, fmt.Errorf("chat: session lost at fin")
			}
			if st == protocol.ChatStatusIncomplete {
				// Authoritative bitmap: anything not set is truly missing.
				ackedCount = 0
				for i := range acked {
					acked[i] = i/8 < len(body) && body[i/8]&(1<<(7-uint(i%8))) != 0
					if acked[i] {
						ackedCount++
					}
				}
				if !sendData(acked, &ackedCount) {
					c.invalidateSession()
					break // restart from SEND_START
				}
				continue
			}
			report(total + 1)
			if len(body) >= 10 {
				lastAccepted = binary.BigEndian.Uint32(body)
				remaining = binary.BigEndian.Uint16(body[4:])
				reset = binary.BigEndian.Uint32(body[6:])
			}
			return st, lastAccepted, remaining, reset, nil
		}
	}
	return 0, 0, 0, 0, fmt.Errorf("chat: upload restarts exhausted")
}

// FetchInbox polls the inbox and decrypts waiting messages. A message that
// fails transiently (network, sender key not yet propagated) is withheld along
// with any later message from the same sender, so the caller never acks past a
// seq that's still on the server.
func (c *ChatClient) FetchInbox(ctx context.Context, onQuery ChatProgress) ([]ChatIncoming, error) {
	c.opSeq.Lock()
	defer c.opSeq.Unlock()
	info, err := c.EnsureInfo(ctx)
	if err != nil {
		return nil, err
	}
	st, body, err := c.op(ctx, info, protocol.BuildChatStatusPlain())
	if err != nil {
		return nil, err
	}
	if st != protocol.ChatStatusOK {
		return nil, &ChatStatusError{Op: protocol.ChatOpStatus, Status: st}
	}
	if len(body) < 7 {
		return nil, fmt.Errorf("chat: short status response")
	}
	c.updateQuota(binary.BigEndian.Uint16(body), binary.BigEndian.Uint32(body[2:]))

	type entry struct {
		src    [protocol.AddressSize]byte
		seq    uint32
		length uint16
		blocks uint8
	}
	count := int(body[6])
	const entryLen = protocol.AddressSize + 4 + 2 + 1
	if len(body) < 7+count*entryLen {
		return nil, fmt.Errorf("chat: truncated status response")
	}
	entries := make([]entry, count)
	for i := 0; i < count; i++ {
		off := 7 + i*entryLen
		copy(entries[i].src[:], body[off:])
		entries[i].seq = binary.BigEndian.Uint32(body[off+protocol.AddressSize:])
		entries[i].length = binary.BigEndian.Uint16(body[off+protocol.AddressSize+4:])
		entries[i].blocks = body[off+protocol.AddressSize+6]
	}
	if count > 0 {
		c.f.log("[chat] inbox: %d new message(s) waiting, downloading…", count)
	} else if c.f.debug {
		c.f.log("[chat] inbox check: empty")
	}

	totalQueries := 1
	for _, e := range entries {
		totalQueries += int(e.blocks) + 1
	}
	done := 1
	report := func() {
		if onQuery != nil {
			onQuery(done, totalQueries)
		}
	}
	report()

	var out []ChatIncoming
	// First seq per sender that failed transiently (network/key not yet
	// propagated): the caller must NOT ack at or past it — the message is still
	// on the server and refetched next poll. Permanent failures (corrupt or
	// undecryptable envelope) don't hold the line: they can never be delivered.
	hold := map[[protocol.AddressSize]byte]uint32{}
	markHold := func(src [protocol.AddressSize]byte, seq uint32) {
		if cur, ok := hold[src]; !ok || seq < cur {
			hold[src] = seq
		}
	}
	for _, e := range entries {
		env := make([]byte, 0, e.length)
		fetchFailed := false
		for blk := uint8(0); blk < e.blocks; blk++ {
			fst, fbody, ferr := c.op(ctx, info, protocol.BuildChatFetchPlain(protocol.ChatPeerHandle(e.src), e.seq, blk))
			if ferr != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				c.f.log("[chat] fetch %x seq %d block %d: %v", e.src[:4], e.seq, blk, ferr)
				fetchFailed = true
				break
			}
			if fst != protocol.ChatStatusOK {
				c.f.log("[chat] fetch %x seq %d block %d: status %d", e.src[:4], e.seq, blk, fst)
				fetchFailed = true
				break
			}
			env = append(env, fbody...)
			done++
			report()
		}
		if fetchFailed {
			markHold(e.src, e.seq)
			continue
		}

		rec, err := c.FetchPeerKey(ctx, e.src)
		done++
		report()
		if err != nil {
			c.f.log("[chat] sender key %x: %v", e.src[:4], err)
			markHold(e.src, e.seq)
			continue
		}
		m, err := protocol.ParseChatMessage(env)
		if err != nil {
			c.f.log("[chat] envelope %x seq %d: %v", e.src[:4], e.seq, err)
			continue
		}
		contentKey, err := protocol.ChatContentKey(c.id.Enc, rec.EncPub, e.src, c.id.Addr, m.Seq)
		if err != nil {
			continue
		}
		text, err := m.Open(contentKey)
		if err != nil {
			c.f.log("[chat] decrypt %x seq %d: %v", e.src[:4], e.seq, err)
			continue
		}
		out = append(out, ChatIncoming{From: e.src, Seq: m.Seq, Text: text})
	}
	// Drop any delivered message at/after a sender's first transient failure so
	// the caller's per-sender ack watermark can't free a skipped earlier seq.
	if len(hold) > 0 {
		kept := out[:0]
		for _, m := range out {
			if h, ok := hold[m.From]; ok && m.Seq >= h {
				continue
			}
			kept = append(kept, m)
		}
		out = kept
	}
	return out, nil
}

// Ack confirms delivery of peer's messages up to upToSeq, freeing inbox quota
// and driving the sender's ✓✓.
func (c *ChatClient) Ack(ctx context.Context, peer [protocol.AddressSize]byte, upToSeq uint32) error {
	c.opSeq.Lock()
	defer c.opSeq.Unlock()
	info, err := c.EnsureInfo(ctx)
	if err != nil {
		return err
	}
	// E2E receipt: prove to the sender (peer) that we received peer→us up to
	// upToSeq. The peer enc key is cached from inbox decryption; on a miss we
	// still ack (freeing quota), just without proof — the sender then sees no
	// verified ✓✓ rather than a forgeable one.
	var receipt [protocol.ChatReceiptMACSize]byte
	if rec, kerr := c.FetchPeerKey(ctx, peer); kerr == nil {
		if rk, derr := protocol.ChatReceiptKey(c.id.Enc, rec.EncPub); derr == nil {
			receipt = protocol.ChatReceiptMAC(rk, peer, c.id.Addr, upToSeq)
		}
	}
	handle := protocol.ChatPeerHandle(peer)
	st, _, err := c.op(ctx, info, protocol.BuildChatAckPlain(handle, upToSeq, receipt))
	if err != nil {
		return err
	}
	if st != protocol.ChatStatusOK {
		return &ChatStatusError{Op: protocol.ChatOpAck, Status: st}
	}
	c.f.log("[chat] delivered ack to %s up to seq=%d", ChatAddressString(peer)[:8], upToSeq)
	return nil
}

// PeerStatus returns (last_accepted ✓, last_delivered ✓✓) for our own
// messages sent to peer. Serialized with opSeq so it cannot swap the session
// out from under an in-progress upload.
func (c *ChatClient) PeerStatus(ctx context.Context, peer [protocol.AddressSize]byte) (accepted, delivered uint32, err error) {
	c.opSeq.Lock()
	defer c.opSeq.Unlock()
	return c.peerStatus(ctx, peer)
}

// peerStatus is the unserialized core; used internally when opSeq is already held.
func (c *ChatClient) peerStatus(ctx context.Context, peer [protocol.AddressSize]byte) (accepted, delivered uint32, err error) {
	info, err := c.EnsureInfo(ctx)
	if err != nil {
		return 0, 0, err
	}
	st, body, err := c.op(ctx, info, protocol.BuildChatSendStatusPlain(peer))
	if err != nil {
		return 0, 0, err
	}
	switch st {
	case protocol.ChatStatusOK:
		if len(body) < 8 {
			return 0, 0, fmt.Errorf("chat: short sendstatus response")
		}
		accepted = binary.BigEndian.Uint32(body)
		delivered = binary.BigEndian.Uint32(body[4:])
		// ✓✓ is only trustworthy with the recipient's E2E receipt: a malicious
		// server can suppress it (delivered falls back to "unproven" = 0) but
		// can't forge one. ✓ (accepted) stays as the server reports — it only
		// confirms an upload the sender already witnessed.
		if delivered > 0 {
			var receipt [protocol.ChatReceiptMACSize]byte
			if len(body) >= 8+protocol.ChatReceiptMACSize {
				copy(receipt[:], body[8:])
			}
			if !c.verifyReceipt(ctx, peer, delivered, receipt) {
				delivered = 0
			}
		}
		return accepted, delivered, nil
	case protocol.ChatStatusNotFound:
		return 0, 0, nil
	default:
		return 0, 0, &ChatStatusError{Op: protocol.ChatOpSendStatus, Status: st}
	}
}

// verifyReceipt checks the recipient's E2E delivery proof for upToSeq. Both
// ends bind (sender, recipient) in originator-first order, so here sender is us
// and recipient is peer.
func (c *ChatClient) verifyReceipt(ctx context.Context, peer [protocol.AddressSize]byte, upToSeq uint32, receipt [protocol.ChatReceiptMACSize]byte) bool {
	rec, err := c.FetchPeerKey(ctx, peer)
	if err != nil {
		return false
	}
	rk, err := protocol.ChatReceiptKey(c.id.Enc, rec.EncPub)
	if err != nil {
		return false
	}
	want := protocol.ChatReceiptMAC(rk, c.id.Addr, peer, upToSeq)
	return hmac.Equal(want[:], receipt[:])
}

// NextSeq returns the next usable message sequence for peer, recovered from the
// server (client amnesia safe).
func (c *ChatClient) NextSeq(ctx context.Context, peer [protocol.AddressSize]byte) (uint32, error) {
	accepted, _, err := c.PeerStatus(ctx, peer)
	if err != nil {
		return 0, err
	}
	return accepted + 1, nil
}
