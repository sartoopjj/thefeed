package client

import (
	"context"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// LogFunc is a callback for log messages.
type LogFunc func(msg string)

// noiseDomains are popular domains used to blend feed queries into normal-looking DNS traffic.
var noiseDomains = []string{
	"www.google.com", "www.cloudflare.com", "one.one.one.one",
	"www.youtube.com", "www.instagram.com", "www.amazon.com",
	"www.microsoft.com", "www.apple.com", "www.github.com",
	"www.wikipedia.org", "www.reddit.com", "www.twitter.com",
}

// resolverStat tracks per-resolver health metrics; fields are accessed with sync/atomic.
type resolverStat struct {
	success int64 // number of successful queries
	failure int64 // number of failed queries
	totalMs int64 // sum of latency in milliseconds over successful queries
}

func (s *resolverStat) score() float64 {
	success := atomic.LoadInt64(&s.success)
	failure := atomic.LoadInt64(&s.failure)
	totalMs := atomic.LoadInt64(&s.totalMs)
	total := success + failure
	if total == 0 {
		return 0.2 // no data yet → low initial weight
	}
	successRate := float64(success) / float64(total)
	var avgMs float64
	if success > 0 {
		avgMs = float64(totalMs) / float64(success)
	} else {
		avgMs = 30000 // 30 s effective penalty for 0% success resolvers
	}
	// Success rate dominates (squared); latency is a mild tiebreaker.
	score := successRate * successRate / (avgMs/5000.0 + 1.0)
	if score < 0.001 {
		score = 0.001
	}
	return score
}

// Fetcher fetches feed blocks over DNS.
type Fetcher struct {
	domain      string   // main domain (canonical); used for upstream send/admin
	domains     []string // main + extra sub-domains; feed reads spread across these
	queryKey    [protocol.KeySize]byte
	responseKey [protocol.KeySize]byte
	queryMode   protocol.QueryEncoding
	timeout     time.Duration

	// Resolver pools — allResolvers is what the user configured;
	// activeResolvers is kept up-to-date by ResolverChecker (only healthy ones).
	mu              sync.RWMutex
	allResolvers    []string
	activeResolvers []string

	// Rate limiting via token bucket; nil means unlimited.
	rateQPS float64
	rateCh  chan struct{}

	debug         bool
	noiseDisabled bool // suppress the decoy-DNS noise loop (per-profile chat fetchers)
	logFunc       LogFunc

	// onMetaChatAvail, if set, is called after every metadata fetch with the
	// server's ChatAvailable advertisement bit and whether that fetch was
	// signature-VERIFIED (a pinned key + valid signature). Lets the web layer
	// cache an authoritative "this config has chat" answer drawn from the feed's
	// regular metadata fetches, so the messenger needn't re-probe.
	onMetaChatAvail func(advertised, verified bool)
	// onMetaMTProtoAvail mirrors onMetaChatAvail for the MTProto tunnel bit,
	// so a client can mark tunnel-capable configs from ordinary feed traffic
	// without probing each one.
	onMetaMTProtoAvail func(advertised, verified bool)

	// Resolver scoring: per-resolver success/failure counters and latency.
	stats sync.Map // string (resolver:port) -> *resolverStat

	// scatter is how many resolvers to query simultaneously per DNS block request.
	// 1 = sequential (no scatter), 2+ = fan-out (use fastest response).
	scatter int

	// Shared-resolver-cache. cacheShare gates the feature; cacheEpoch is the
	// metadata-derived seed. Atomic so the web layer can refresh the epoch
	// from a parallel goroutine after each metadata fetch without a lock.
	cacheShare atomic.Bool
	cacheEpoch atomic.Uint32

	// Extended metadata state. extFailCount counts consecutive failures
	// within the current "round"; on hitting metadataExtMaxRetries,
	// extCooldownUntil is set so the next metadataExtCooldownDur uses the
	// legacy fetch path only. All in-RAM, cleared on restart.
	extFailCount     atomic.Int32
	extCooldownUntil atomic.Int64 // unix seconds

	// exchangeFn is the function used to send a DNS message to a resolver.
	// It defaults to a real dns.Client exchange and can be replaced in tests.
	exchangeFn func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error)

	// statsForward, when set, receives a copy of every RecordSuccess/RecordFailure
	// call so child fetchers (e.g. chat) can feed results back to the parent.
	statsForward func(resolver string, ok bool, latency time.Duration)

	// serverPubKey is the pinned server signing key (from the config's sk=).
	// When non-nil, channel/metadata content is verified against the signed
	// ExtraBlock served at block index == the channel's block count. nil
	// disables verification (no key pinned / old config).
	serverPubKey ed25519.PublicKey
	// lastExtraTS holds the newest ExtraBlock timestamp seen per channel id,
	// for in-session rollback protection (reject a signed-but-older block).
	lastExtraTS sync.Map // uint16 -> int64
}

// ErrUnverified means no valid ExtraBlock could be fetched (e.g. an old
// server that doesn't emit one). Content integrity could not be checked;
// callers decide policy (feed: accept with a warning; messenger: reject).
var ErrUnverified = errors.New("content unverified: no signature block")

// ErrExtraBlockInvalid means an ExtraBlock WAS returned but failed
// verification (bad signature, wrong channel, rolled-back timestamp, or
// content-digest mismatch) — i.e. an active tamper attempt. Always hard-fail.
var ErrExtraBlockInvalid = errors.New("content signature invalid")

// SetServerPublicKey pins the server signing key (base64url, no padding, as
// printed by genserverkey / the sk= URI field). Empty disables verification.
func (f *Fetcher) SetServerPublicKey(b64 string) error {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		f.serverPubKey = nil
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		// Tolerate standard base64 too, in case a key was pasted that way.
		if raw2, err2 := base64.StdEncoding.DecodeString(b64); err2 == nil {
			raw = raw2
		} else {
			return fmt.Errorf("decode server public key: %w", err)
		}
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("server public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	f.serverPubKey = ed25519.PublicKey(raw)
	return nil
}

// HasServerKey reports whether a server signing key is pinned.
func (f *Fetcher) HasServerKey() bool { return f.serverPubKey != nil }

// verifyExtraBytes verifies an already-fetched ExtraBlock (raw — fetched in
// the same parallel batch as the content, so no extra round-trip) against the
// pinned key, the channel id, the rollback guard, and the content digest.
// Returns nil when no key is pinned, ErrUnverified when raw is absent or
// unparseable (old server — tolerated), or ErrExtraBlockInvalid on an active
// tamper (always hard-fail at the caller).
func (f *Fetcher) verifyExtraBytes(channelID uint16, content, raw []byte) error {
	if f.serverPubKey == nil {
		return nil
	}
	if len(raw) == 0 {
		return ErrUnverified
	}
	eb, err := protocol.ParseExtraBlock(raw)
	if err != nil {
		return ErrUnverified
	}
	if err := protocol.VerifyExtraBlock(f.serverPubKey, channelID, eb); err != nil {
		f.log("[verify] channel %d: %v", channelID, err)
		return ErrExtraBlockInvalid
	}
	// Rollback guard: never accept a signed block older than the newest we've
	// already seen for this channel in this session.
	if prev, ok := f.lastExtraTS.Load(channelID); ok {
		if eb.Timestamp < prev.(int64) {
			f.log("[verify] channel %d: rollback (ts %d < %d)", channelID, eb.Timestamp, prev.(int64))
			return ErrExtraBlockInvalid
		}
	}
	if err := eb.VerifyChannelContent(content); err != nil {
		f.log("[verify] channel %d: %v", channelID, err)
		return ErrExtraBlockInvalid
	}
	f.lastExtraTS.Store(channelID, eb.Timestamp)
	return nil
}

// fetchRawAndVerify fetches blocks [0..count-1] of channelID plus, when a key
// is pinned, the signature ExtraBlock at index == count — all in one parallel
// batch so verification adds no extra round-trip — then verifies the signature
// over the raw block concatenation it returns. Pass block0 if already fetched
// (count-prefixed channels need it to learn count), or nil to fetch it here.
// A content-block failure is fatal; a missing ExtraBlock (old server) is
// tolerated; a present-but-invalid one returns ErrExtraBlockInvalid.
func (f *Fetcher) fetchRawAndVerify(ctx context.Context, channelID uint16, count int, block0 []byte) ([]byte, error) {
	if count <= 0 {
		return nil, fmt.Errorf("invalid block count %d", count)
	}
	blocks := make([][]byte, count)
	if block0 != nil {
		blocks[0] = block0
	}
	needExtra := f.serverPubKey != nil
	var extraRaw []byte

	bctx, cancel := context.WithCancel(ctx)
	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		blkErr error
	)
	start := 1
	if block0 == nil {
		start = 0
	}
	for i := start; i < count; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b, err := f.FetchBlock(bctx, channelID, uint16(idx))
			if err != nil {
				errMu.Lock()
				if blkErr == nil {
					blkErr = fmt.Errorf("block %d: %w", idx, err)
					cancel()
				}
				errMu.Unlock()
				return
			}
			blocks[idx] = b
		}(i)
	}
	if needExtra {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Tolerated failure: a missing ExtraBlock (old server) leaves
			// extraRaw nil → unverified, not fatal.
			if b, err := f.FetchBlock(bctx, channelID, uint16(count)); err == nil {
				extraRaw = b
			}
		}()
	}
	wg.Wait()
	cancel()
	if blkErr != nil {
		return nil, blkErr
	}

	var raw []byte
	for _, b := range blocks {
		raw = append(raw, b...)
	}
	if needExtra {
		if verr := f.verifyExtraBytes(channelID, raw, extraRaw); verr == ErrExtraBlockInvalid {
			return nil, verr
		}
	}
	return raw, nil
}

// NewFetcher creates a new DNS block fetcher.
func NewFetcher(domain, passphrase string, resolvers []string) (*Fetcher, error) {
	qk, rk, err := protocol.DeriveKeys(passphrase)
	if err != nil {
		return nil, fmt.Errorf("derive keys: %w", err)
	}

	r := make([]string, len(resolvers))
	copy(r, resolvers)

	f := &Fetcher{
		domain:       strings.TrimSuffix(domain, "."),
		queryKey:     qk,
		responseKey:  rk,
		queryMode:    protocol.QuerySingleLabel,
		allResolvers: r,
		// activeResolvers starts empty — the ResolverChecker fills it in after
		// the first health-check scan so no fetch is attempted with unvalidated resolvers.
		timeout: 25 * time.Second,
		scatter: 4, // query 4 resolvers in parallel by default
	}
	f.domains = []string{f.domain}
	f.exchangeFn = func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		c := &dns.Client{Timeout: f.timeout, Net: "udp"}
		return c.ExchangeContext(ctx, m, addr)
	}
	return f, nil
}

// SetDomains sets the extra sub-domains feed queries are spread across, in
// addition to the main domain from NewFetcher (which stays first/canonical).
// Blanks, the main domain, and duplicates are ignored. Call at init.
func (f *Fetcher) SetDomains(extra []string) {
	domains := []string{f.domain}
	for _, d := range extra {
		d = strings.TrimSuffix(strings.TrimSpace(d), ".")
		if d == "" {
			continue
		}
		dup := false
		for _, existing := range domains {
			if strings.EqualFold(existing, d) {
				dup = true
				break
			}
		}
		if !dup {
			domains = append(domains, d)
		}
	}
	f.domains = domains
}

// pickDomain selects which domain a feed-block query goes to. With one domain
// it returns it unchanged. With several it spreads deterministically by
// (channel, block) — stable across users so the shared-resolver cache still
// works — and rotates by attempt so a blocked/filtered domain is routed around
// on retry.
func (f *Fetcher) pickDomain(channel, block uint16, attempt int) string {
	n := len(f.domains)
	if n <= 1 {
		return f.domain
	}
	base := uint32(channel)*2654435761 + uint32(block)*40503
	idx := (int(base>>16) + attempt) % n
	return f.domains[idx]
}

// SetRateLimit sets the maximum queries per second (0 = unlimited). Must be called before Start.
func (f *Fetcher) SetRateLimit(qps float64) {
	f.rateQPS = qps
}

// ScanConcurrency returns how many resolvers the scanner should probe in
// parallel, derived from the configured rate limit.
// Rule: concurrency = max(1, floor(rateQPS)).
// If rateQPS is 0 (unlimited), falls back to the default of 10.
func (f *Fetcher) ScanConcurrency() int {
	if f.rateQPS <= 0 {
		return 10
	}
	n := int(f.rateQPS)
	if n < 10 {
		n = 10
	}
	return n
}

// InFlightBudget is how many queries may be outstanding at once, derived from
// the configured rate. A query occupies roughly a second of the budget, so the
// rate is also the sensible ceiling on cells in flight -- and it is what keeps
// a send window from growing without bound on a link that answers slowly.
func (f *Fetcher) InFlightBudget() int {
	if f.rateQPS <= 0 {
		return 10
	}
	n := int(f.rateQPS)
	if n < 1 {
		n = 1
	}
	return n
}

// SetTimeout sets the per-query DNS timeout.
func (f *Fetcher) SetTimeout(d time.Duration) {
	f.timeout = d
}

// SetLogFunc sets the debug log callback.
func (f *Fetcher) SetLogFunc(fn LogFunc) {
	f.logFunc = fn
}

// SetOnMetaChatAvail sets the callback invoked after each metadata fetch with
// the server's ChatAvailable bit and whether that metadata was signature-verified.
func (f *Fetcher) SetOnMetaChatAvail(fn func(advertised, verified bool)) {
	f.onMetaChatAvail = fn
}

// SetOnMetaMTProtoAvail sets the callback for the MTProto-advertised bit.
func (f *Fetcher) SetOnMetaMTProtoAvail(fn func(advertised, verified bool)) {
	f.onMetaMTProtoAvail = fn
}

// SetDebug enables or disables debug logging of generated query names.
func (f *Fetcher) SetDebug(debug bool) {
	f.debug = debug
}

// SetQueryMode sets the DNS query encoding mode.
func (f *Fetcher) SetQueryMode(mode protocol.QueryEncoding) {
	f.queryMode = mode
}

// SetCacheShare toggles the shared-resolver-cache feature. When on, queries
// to eligible channels use a deterministic suffix derived from the cache
// epoch so public resolvers can serve identical responses across users.
func (f *Fetcher) SetCacheShare(on bool) { f.cacheShare.Store(on) }

// SetCacheEpoch updates the deterministic seed. Pass the latest Metadata
// NextFetch (or any monotonically-advancing per-server value) after every
// successful metadata refresh; advancing it invalidates cached responses.
func (f *Fetcher) SetCacheEpoch(epoch uint32) { f.cacheEpoch.Store(epoch) }

// encodeBlockQuery picks between the random and the deterministic encoder
// based on the feature flag, the cached epoch, and channel eligibility.
// No epoch (NextFetch=0) → random, to avoid sharing cache across a stale
// or unknown server state.
func (f *Fetcher) encodeBlockQuery(channel, block uint16, attempt int) (string, error) {
	share := f.cacheShare.Load()
	eligible := protocol.ChannelEligibleForSharedCache(channel)
	epoch := f.cacheEpoch.Load()
	if share && eligible && epoch != 0 {
		var seed [4]byte
		binary.BigEndian.PutUint32(seed[:], epoch)
		// Stable domain (attempt ignored) so shared-cache queries stay
		// identical across users and retries.
		domain := f.pickDomain(channel, block, 0)
		if f.debug {
			f.log("[debug] det query ch=%d blk=%d epoch=%d dom=%s", channel, block, epoch, domain)
		}
		return protocol.EncodeQueryDeterministic(f.queryKey, channel, block, domain, f.queryMode, seed[:])
	}
	domain := f.pickDomain(channel, block, attempt)
	if f.debug {
		f.log("[debug] rnd query ch=%d blk=%d share=%v eligible=%v epoch=%d dom=%s", channel, block, share, eligible, epoch, domain)
	}
	return protocol.EncodeQuery(f.queryKey, channel, block, domain, f.queryMode)
}

// SetActiveResolvers updates the healthy resolver pool. Called by ResolverChecker.
func (f *Fetcher) SetActiveResolvers(resolvers []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeResolvers = make([]string, len(resolvers))
	copy(f.activeResolvers, resolvers)
	f.log("active resolvers updated: %d/%d healthy", len(resolvers), len(f.allResolvers))
}

// SetResolvers replaces the full resolver list and resets the active pool.
func (f *Fetcher) SetResolvers(resolvers []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allResolvers = make([]string, len(resolvers))
	copy(f.allResolvers, resolvers)
	f.activeResolvers = make([]string, len(resolvers))
	copy(f.activeResolvers, resolvers)
}

// UpdateResolverPool replaces the full resolver list and removes any active
// resolvers that are no longer in the bank.
func (f *Fetcher) UpdateResolverPool(resolvers []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bankSet := make(map[string]bool, len(resolvers))
	for _, r := range resolvers {
		k := r
		if !strings.Contains(k, ":") {
			k += ":53"
		}
		bankSet[k] = true
	}
	filtered := make([]string, 0, len(f.activeResolvers))
	for _, r := range f.activeResolvers {
		k := r
		if !strings.Contains(k, ":") {
			k += ":53"
		}
		if bankSet[k] {
			filtered = append(filtered, r)
		}
	}
	f.allResolvers = make([]string, len(resolvers))
	copy(f.allResolvers, resolvers)
	f.activeResolvers = filtered
	f.log("resolver pool updated: %d total, %d active", len(f.allResolvers), len(f.activeResolvers))
}

// RemoveActiveResolver removes a resolver from the active pool.
func (f *Fetcher) RemoveActiveResolver(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	filtered := make([]string, 0, len(f.activeResolvers))
	for _, r := range f.activeResolvers {
		if r != addr {
			filtered = append(filtered, r)
		}
	}
	f.activeResolvers = filtered
	f.log("removed resolver %s, %d active remaining", addr, len(filtered))
}

// ResetStats clears all resolver scoring data.
func (f *Fetcher) ResetStats() {
	f.stats.Range(func(key, _ any) bool {
		f.stats.Delete(key)
		return true
	})
	f.log("resolver scoreboard reset")
}

// QueryTotals returns cumulative (responses, errors) summed across all
// resolvers. Snapshotting the delta around an operation tells the caller how
// many queries it issued (responses+errors) and how many were lost (errors) —
// the same success/failure signal the resolver scoreboard is built on.
func (f *Fetcher) QueryTotals() (responses, errs int64) {
	f.stats.Range(func(_, val any) bool {
		s := val.(*resolverStat)
		responses += atomic.LoadInt64(&s.success)
		errs += atomic.LoadInt64(&s.failure)
		return true
	})
	return
}

// ExportStats returns a snapshot of all resolver stats.
func (f *Fetcher) ExportStats() map[string][3]int64 {
	out := make(map[string][3]int64)
	f.stats.Range(func(key, val any) bool {
		s := val.(*resolverStat)
		out[key.(string)] = [3]int64{
			atomic.LoadInt64(&s.success),
			atomic.LoadInt64(&s.failure),
			atomic.LoadInt64(&s.totalMs),
		}
		return true
	})
	return out
}

// ImportStats loads previously exported stats into this fetcher.
func (f *Fetcher) ImportStats(m map[string][3]int64) {
	for key, vals := range m {
		f.stats.Store(key, &resolverStat{
			success: vals[0],
			failure: vals[1],
			totalMs: vals[2],
		})
	}
}

// AllResolvers returns all user-configured resolvers.
func (f *Fetcher) AllResolvers() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make([]string, len(f.allResolvers))
	copy(result, f.allResolvers)
	return result
}

// QueryMode returns the configured query encoding (single base32 label or the
// feed's multi-label hex). Chat cells honor it so they blend with feed traffic.
func (f *Fetcher) QueryMode() protocol.QueryEncoding { return f.queryMode }

// Resolvers returns the currently active (healthy) resolver list.
func (f *Fetcher) Resolvers() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make([]string, len(f.activeResolvers))
	copy(result, f.activeResolvers)
	return result
}

// ResolverInfo holds public stats for a single resolver.
type ResolverInfo struct {
	Addr    string  `json:"addr"`
	Score   float64 `json:"score"`
	Success int64   `json:"success"`
	Failure int64   `json:"failure"`
	AvgMs   float64 `json:"avgMs"`
}

// ResolverScoreboard returns stats for all active resolvers sorted by score descending.
func (f *Fetcher) ResolverScoreboard() []ResolverInfo {
	resolvers := f.Resolvers()
	infos := make([]ResolverInfo, 0, len(resolvers))
	for _, r := range resolvers {
		key := r
		if !strings.Contains(key, ":") {
			key += ":53"
		}
		info := ResolverInfo{Addr: r}
		if v, ok := f.stats.Load(key); ok {
			s := v.(*resolverStat)
			info.Success = atomic.LoadInt64(&s.success)
			info.Failure = atomic.LoadInt64(&s.failure)
			if info.Success > 0 {
				info.AvgMs = float64(atomic.LoadInt64(&s.totalMs)) / float64(info.Success)
			}
			info.Score = s.score()
		} else {
			info.Score = 0.2
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Score > infos[j].Score })
	return infos
}

// SetScatter sets the number of resolvers queried simultaneously per DNS block request.
// 1 = sequential (no scatter). Values > 1 fan out to N resolvers and use the fastest response.
// Must be called before Start().
func (f *Fetcher) SetScatter(n int) {
	if n < 1 {
		n = 1
	}
	f.scatter = n
}

// SetStatsForward registers a callback that receives a copy of every
// success/failure so a child fetcher can feed results back to the parent.
func (f *Fetcher) SetStatsForward(fn func(resolver string, ok bool, latency time.Duration)) {
	f.statsForward = fn
}

// RecordSuccess records a successful DNS query for the given resolver.
func (f *Fetcher) RecordSuccess(resolver string, latency time.Duration) {
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}
	v, _ := f.stats.LoadOrStore(resolver, &resolverStat{})
	s := v.(*resolverStat)
	atomic.AddInt64(&s.success, 1)
	atomic.AddInt64(&s.totalMs, latency.Milliseconds())
	if f.statsForward != nil {
		f.statsForward(resolver, true, latency)
	}
}

// RecordFailure records a failed DNS query for the given resolver.
func (f *Fetcher) RecordFailure(resolver string) {
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}
	v, _ := f.stats.LoadOrStore(resolver, &resolverStat{})
	s := v.(*resolverStat)
	atomic.AddInt64(&s.failure, 1)
	if f.statsForward != nil {
		f.statsForward(resolver, false, 0)
	}
}

// resolverScore returns the health score for a resolver (higher = better).
func (f *Fetcher) resolverScore(resolver string) float64 {
	key := resolver
	if !strings.Contains(key, ":") {
		key += ":53"
	}
	if v, ok := f.stats.Load(key); ok {
		return v.(*resolverStat).score()
	}
	return 1.0 // no data yet → neutral weight
}

// pickWeightedResolvers picks up to n resolvers from the active pool using
// weighted-random selection (higher score → more likely to be chosen).
func (f *Fetcher) pickWeightedResolvers(n int) []string {
	resolvers := f.Resolvers()
	if len(resolvers) == 0 {
		return nil
	}
	if n <= 0 {
		n = 1
	}
	if n >= len(resolvers) {
		// Return all resolvers sorted by score descending.
		type scored struct {
			r string
			s float64
		}
		ss := make([]scored, len(resolvers))
		for i, r := range resolvers {
			ss[i] = scored{r, f.resolverScore(r)}
		}
		sort.Slice(ss, func(i, j int) bool { return ss[i].s > ss[j].s })
		out := make([]string, len(ss))
		for i, s := range ss {
			out[i] = s.r
		}
		return out
	}
	// Weighted random sampling without replacement.
	weights := make([]float64, len(resolvers))
	total := 0.0
	for i, r := range resolvers {
		w := f.resolverScore(r)
		if w < 0.001 {
			w = 0.001 // every resolver keeps a minimal chance
		}
		weights[i] = w
		total += w
	}
	picked := make([]string, 0, n)
	for len(picked) < n && total > 0 {
		r := rand.Float64() * total
		cumul := 0.0
		chosen := -1
		for i, w := range weights {
			if w == 0 {
				continue
			}
			cumul += w
			if r < cumul {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			// Floating-point edge case: pick last non-zero entry.
			for i := len(weights) - 1; i >= 0; i-- {
				if weights[i] > 0 {
					chosen = i
					break
				}
			}
		}
		if chosen < 0 {
			break
		}
		picked = append(picked, resolvers[chosen])
		total -= weights[chosen]
		weights[chosen] = 0
	}
	return picked
}

// scatterQuery sends qname to all given resolvers concurrently and returns
// the first successful response. The winning response cancels the others.
func (f *Fetcher) scatterQuery(ctx context.Context, resolvers []string, qname string) ([]byte, error) {
	if len(resolvers) == 1 {
		return f.queryResolver(ctx, resolvers[0], qname)
	}
	type result struct {
		data []byte
		err  error
	}
	resultCh := make(chan result, len(resolvers))
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i, r := range resolvers {
		go func(resolver string, idx int) {
			// Stagger launches: first resolver fires immediately, others wait
			// a random 50–300 ms to avoid a simultaneous burst.
			if idx > 0 {
				jitter := time.Duration(50+rand.Intn(250)) * time.Millisecond
				select {
				case <-time.After(jitter):
				case <-subCtx.Done():
					return
				}
			}
			data, err := f.queryResolver(subCtx, resolver, qname)
			select {
			case resultCh <- result{data, err}:
			case <-subCtx.Done():
			}
		}(r, i)
	}
	var lastErr error
	for i := 0; i < len(resolvers); i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-resultCh:
			if r.err == nil {
				cancel() // cancel remaining in-flight queries
				return r.data, nil
			}
			lastErr = r.err
		}
	}
	return nil, lastErr
}

// SetNoiseDisabled suppresses the decoy-DNS noise generator for this fetcher.
// Used by the per-profile chat fetchers, which piggyback on the main feed
// fetcher's cover traffic — running one noise loop per profile would multiply
// background DNS for no benefit. Call before Start.
func (f *Fetcher) SetNoiseDisabled(v bool) { f.noiseDisabled = v }

// Start launches background goroutines (rate limiter and, unless disabled, the
// noise generator). ctx controls their lifetime — cancel it to cleanly stop
// them. Call once per fetcher configuration; creating a new fetcher replaces
// the old one.
func (f *Fetcher) Start(ctx context.Context) {
	if f.rateQPS > 0 {
		f.log("fetcher started: %d configured resolvers, rate=%.1f q/s, scatter=%d", len(f.allResolvers), f.rateQPS, f.scatter)
		f.rateCh = make(chan struct{}, 1)
		go f.runRateLimiter(ctx)
		if !f.noiseDisabled {
			go f.runNoise(ctx)
		}
	}
}

// runRateLimiter issues one token into rateCh every 1/QPS seconds.
// The channel capacity is 1, so tokens do not accumulate (no burst).
func (f *Fetcher) runRateLimiter(ctx context.Context) {
	interval := time.Duration(float64(time.Second) / f.rateQPS)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case f.rateCh <- struct{}{}:
			default: // bucket full; discard extra token to prevent burst
			}
		}
	}
}

// runNoise sends decoy A-record queries to popular domains at a low rate
// to make feed traffic blend into normal DNS usage without exhausting resolver limits.
func (f *Fetcher) runNoise(ctx context.Context) {
	const baseInterval = 10 * time.Second
	for {
		// Random delay: 10–30 seconds.
		jitter := time.Duration(rand.Int63n(int64(20 * time.Second)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(baseInterval + jitter):
		}

		resolvers := f.Resolvers()
		if len(resolvers) == 0 {
			continue
		}
		resolver := resolvers[rand.Intn(len(resolvers))]
		if !strings.Contains(resolver, ":") {
			resolver += ":53"
		}
		target := noiseDomains[rand.Intn(len(noiseDomains))]

		go func(r, d string) {
			c := &dns.Client{Timeout: f.timeout}
			m := new(dns.Msg)
			m.SetQuestion(dns.Fqdn(d), dns.TypeA)
			m.RecursionDesired = true
			_, _, _ = c.Exchange(m, r)
		}(resolver, target)
	}
}

func (f *Fetcher) log(format string, args ...any) {
	if f.logFunc != nil {
		f.logFunc(fmt.Sprintf(format, args...))
	}
}

// logProgress logs a progress bar: "prefix [====>    ] 45%"
func (f *Fetcher) logProgress(prefix string, current, total float64) {
	if f.logFunc == nil || total <= 0 {
		return
	}

	percent := int((current / total) * 100)
	barLen := 20
	filled := int((current / total) * float64(barLen))
	empty := barLen - filled

	bar := "["
	for i := 0; i < filled; i++ {
		bar += "="
	}
	if filled < barLen {
		bar += ">"
	}
	for i := 0; i < empty-1; i++ {
		bar += " "
	}
	bar += "]"

	f.logFunc(fmt.Sprintf("%s %s %d%%", prefix, bar, percent))
}

// rateWait blocks until a rate-limit token is available or ctx is cancelled.
// Returns nil when a token was acquired, ctx.Err() when cancelled.
func (f *Fetcher) rateWait(ctx context.Context) error {
	if f.rateCh == nil {
		// Unlimited: just propagate any existing cancellation.
		return ctx.Err()
	}
	select {
	case <-f.rateCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FetchBlock fetches a single encrypted block from the given channel.
// It enqueues through the rate limiter and respects ctx cancellation.
// On transient failure it retries up to 20 times with a short back-off.
func (f *Fetcher) FetchBlock(ctx context.Context, channel, block uint16) ([]byte, error) {
	return f.fetchBlockAttempts(ctx, channel, block, 20)
}

// fetchBlockAttempts is FetchBlock with a caller-chosen retry budget — used
// for probe fetches (e.g. chat capability discovery) where a missing channel
// is an expected answer, not a transient failure.
func (f *Fetcher) fetchBlockAttempts(ctx context.Context, channel, block uint16, maxAttempts int) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Brief back-off before retry; bail immediately if ctx is done.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}

		if err := f.rateWait(ctx); err != nil {
			return nil, err
		}

		qname, err := f.encodeBlockQuery(channel, block, attempt)
		if err != nil {
			return nil, fmt.Errorf("encode query: %w", err)
		}

		scatter := f.scatter
		if scatter < 1 {
			scatter = 1
		}
		picked := f.pickWeightedResolvers(scatter)
		if len(picked) == 0 {
			return nil, fmt.Errorf("no active resolvers")
		}
		if f.debug {
			f.log("[debug] query ch=%d blk=%d attempt=%d qname=%s resolvers=[%s]",
				channel, block, attempt+1, qname, strings.Join(picked, ","))
		}

		data, err := f.scatterQuery(ctx, picked, qname)
		if err == nil {
			if f.debug {
				f.log("[debug] response ch=%d blk=%d len=%d", channel, block, len(data))
			}
			return data, nil
		}
		lastErr = fmt.Errorf("scatter query failed: %w", err)
		if attempt+1 < maxAttempts {
			f.log("block ch=%d blk=%d attempt %d/%d failed, retrying: %v", channel, block, attempt+1, maxAttempts, lastErr)
		}
	}
	return nil, lastErr
}

// Extended-metadata tuning. Constants here, not fields, because they don't
// vary per fetcher and tests can patch by replacing the literal call site.
const (
	metadataExtMaxRetries  = 10               // hash mismatches before falling back to legacy fetch
	metadataExtCooldownDur = 10 * time.Minute // after the round gives up, skip the extended fast path
	metadataLegacyCap      = 64               // legacy fetch max blocks; bumped from 10

	// metadataBlockPreallocHint is a *client-side* guess at average per-block
	// payload size, used only to pre-size the assembled-V0 buffer so we don't
	// realloc on every Append. Decoupled from the server's MinBlockPayload —
	// we don't want to couple the client to a server-side knob that may
	// move to env config later.
	metadataBlockPreallocHint = 256
)

// FetchMetadata returns the current metadata. Block 0 of channel 0 may
// carry an extended header (magic + block_count + hash) packed into the
// otherwise-unused Marker + Timestamp fields. New servers emit that;
// old servers don't. We always fetch block 0 first; if it has the magic
// we use the fast parallel path, otherwise we fall back to the legacy
// "fetch-until-parse-succeeds" loop. The cooldown only kicks in when the
// fast path is repeatedly broken (e.g., snapshot churn that hash-verify
// can't ride out), in which case we go straight to legacy for a while.
func (f *Fetcher) FetchMetadata(ctx context.Context) (*protocol.Metadata, error) {
	block0, err := f.FetchBlock(ctx, protocol.MetadataChannel, 0)
	if err != nil {
		return nil, fmt.Errorf("fetch metadata block 0: %w", err)
	}

	extended, count, hash, hdrErr := protocol.PeekExtendedHeader(block0)
	if extended && !f.extInCooldown() {
		meta, err := f.fetchMetadataExtended(ctx, block0, count, hash)
		if err == nil {
			f.extFailCount.Store(0)
			f.cacheEpoch.Store(meta.NextFetch)
			return meta, nil
		}
		if n := f.extFailCount.Add(1); n >= metadataExtMaxRetries {
			f.extCooldownUntil.Store(time.Now().Add(metadataExtCooldownDur).Unix())
			f.extFailCount.Store(0)
			if f.debug {
				f.log("[debug] extended metadata: %d consecutive failures, cooling down for %v", n, metadataExtCooldownDur)
			}
		}
		if f.debug {
			f.log("[debug] extended metadata failed (%v), falling back to legacy", err)
		}
		// Fall through to legacy path below — we already have block 0.
	}
	if hdrErr != nil && f.debug {
		f.log("[debug] extended header peek: %v (using legacy fetch)", hdrErr)
	}
	return f.fetchMetadataLegacy(ctx, block0)
}

// fetchMetadataExtended handles the fast path: block 0 already in hand
// with a valid extended header. Fetches blocks 1..N-1 in parallel,
// concatenates, verifies hash, parses. Retries up to metadataExtMaxRetries
// times on hash mismatch (snapshot drift between block 0 and the others).
func (f *Fetcher) fetchMetadataExtended(parent context.Context, block0 []byte, count uint8, hash [protocol.EMHHashLen]byte) (*protocol.Metadata, error) {
	var lastErr error
	for attempt := 0; attempt < metadataExtMaxRetries; attempt++ {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		// Re-fetch block 0 on retry — the server may have refreshed and
		// the previous block 0's hash no longer matches its peers.
		current := block0
		currentCount := count
		currentHash := hash
		if attempt > 0 {
			b0, err := f.FetchBlock(parent, protocol.MetadataChannel, 0)
			if err != nil {
				return nil, fmt.Errorf("retry block 0: %w", err)
			}
			ext, c, h, herr := protocol.PeekExtendedHeader(b0)
			if !ext || herr != nil {
				return nil, fmt.Errorf("retry block 0 lost extended header: %v", herr)
			}
			current, currentCount, currentHash = b0, c, h
		}

		blocks := make([][]byte, currentCount)
		blocks[0] = current
		// Fetch the remaining metadata blocks AND (when a key is pinned) the
		// signature ExtraBlock at index == currentCount in one parallel batch,
		// so verification adds no extra round-trip. Content-block failure is
		// fatal; a missing ExtraBlock (old server) is tolerated.
		var extraRaw []byte
		needExtra := f.serverPubKey != nil
		if currentCount > 1 || needExtra {
			ctx, cancel := context.WithCancel(parent)
			var (
				wg     sync.WaitGroup
				errMu  sync.Mutex
				blkErr error
			)
			for i := uint16(1); i < uint16(currentCount); i++ {
				wg.Add(1)
				go func(idx uint16) {
					defer wg.Done()
					b, err := f.FetchBlock(ctx, protocol.MetadataChannel, idx)
					if err != nil {
						errMu.Lock()
						firstFail := blkErr == nil
						if firstFail {
							blkErr = fmt.Errorf("block %d: %w", idx, err)
						}
						errMu.Unlock()
						if firstFail {
							cancel()
						}
						return
					}
					blocks[idx] = b
				}(i)
			}
			if needExtra {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if b, err := f.FetchBlock(ctx, protocol.MetadataChannel, uint16(currentCount)); err == nil {
						extraRaw = b
					}
				}()
			}
			wg.Wait()
			cancel()
			if blkErr != nil {
				return nil, blkErr
			}
		}

		assembled := make([]byte, 0, int(currentCount)*metadataBlockPreallocHint)
		for _, b := range blocks {
			assembled = append(assembled, b...)
		}
		if len(assembled) < protocol.EMHHeaderLen {
			return nil, fmt.Errorf("assembled payload too short: %d", len(assembled))
		}
		if err := protocol.VerifyExtendedHash(currentHash, assembled[protocol.EMHHeaderLen:]); err != nil {
			lastErr = err
			if f.debug {
				f.log("[debug] extended hash mismatch attempt %d/%d: %v", attempt+1, metadataExtMaxRetries, err)
			}
			continue
		}
		meta, err := protocol.ParseMetadata(assembled)
		if err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		// Verify the server signature over the metadata block concatenation
		// using the ExtraBlock fetched above. A present-but-invalid signature
		// is a hard failure; a missing one (old server) is tolerated.
		verr := f.verifyExtraBytes(protocol.MetadataChannel, assembled, extraRaw)
		if verr == ErrExtraBlockInvalid {
			return nil, fmt.Errorf("metadata: %w", verr)
		}
		// Report the chat advertisement bit: verified only when a key is pinned
		// AND the signature checked out (verr == nil), so the messenger can trust
		// a "no chat" verdict drawn from the feed's regular metadata fetches.
		if f.onMetaChatAvail != nil {
			f.onMetaChatAvail(meta.ChatAvailable, f.serverPubKey != nil && verr == nil)
		}
		if f.onMetaMTProtoAvail != nil {
			f.onMetaMTProtoAvail(meta.MTProtoAvailable, f.serverPubKey != nil && verr == nil)
		}
		return meta, nil
	}
	return nil, fmt.Errorf("extended metadata: %d consecutive hash mismatches: %w", metadataExtMaxRetries, lastErr)
}

// fetchMetadataLegacy is the legacy multi-block-probe path against channel
// 0. Used when the server doesn't emit the extended header, or when the
// extended fast path has been failing and we're in cooldown. Slated for
// removal in 1.0.0 once every supported server emits the extended header.
func (f *Fetcher) fetchMetadataLegacy(ctx context.Context, block0 []byte) (*protocol.Metadata, error) {
	meta, err := protocol.ParseMetadata(block0)
	if err == nil {
		f.cacheEpoch.Store(meta.NextFetch)
		// Legacy path never fetches the signature → unverified (verified=false).
		if f.onMetaChatAvail != nil {
			f.onMetaChatAvail(meta.ChatAvailable, false)
		}
		if f.onMetaMTProtoAvail != nil {
			f.onMetaMTProtoAvail(meta.MTProtoAvailable, false)
		}
		return meta, nil
	}
	allData := make([]byte, len(block0))
	copy(allData, block0)
	for blk := uint16(1); blk < metadataLegacyCap; blk++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		block, fetchErr := f.FetchBlock(ctx, protocol.MetadataChannel, blk)
		if fetchErr != nil {
			break
		}
		allData = append(allData, block...)
		meta, parseErr := protocol.ParseMetadata(allData)
		if parseErr == nil {
			f.cacheEpoch.Store(meta.NextFetch)
			if f.onMetaChatAvail != nil {
				f.onMetaChatAvail(meta.ChatAvailable, false)
			}
			if f.onMetaMTProtoAvail != nil {
				f.onMetaMTProtoAvail(meta.MTProtoAvailable, false)
			}
			return meta, nil
		}
	}
	return nil, fmt.Errorf("could not parse metadata: %w", err)
}

// extInCooldown returns true while we should skip the extended fast path
// after a recent run of failures. State is in-RAM only; restart clears it.
func (f *Fetcher) extInCooldown() bool {
	until := f.extCooldownUntil.Load()
	return until > 0 && time.Now().Unix() < until
}

// FetchLatestVersion fetches the latest release version from the dedicated
// version channel. The block is padded to a random size matching regular content
// blocks (DPI-resistant). Empty string means unknown/unavailable.
func (f *Fetcher) FetchLatestVersion(ctx context.Context) (string, error) {
	// Fetch block 0 and (when a key is pinned) the signature ExtraBlock in
	// parallel, verifying the version block before trusting it.
	raw, err := f.fetchRawAndVerify(ctx, protocol.VersionChannel, 1, nil)
	if err != nil {
		return "", fmt.Errorf("fetch version block: %w", err)
	}
	return protocol.DecodeVersionData(raw)
}

// RelayInfo carries the relay-discovery data the server publishes on
// RelayInfoChannel. Empty fields mean "not configured".
type RelayInfo struct {
	GitHubRepo string // "owner/repo"
}

// FetchRelayInfo pulls the relay-info payload from RelayInfoChannel.
// Block 0 carries a uint16 total-block count prefix; if more than one
// block is needed the rest are fetched in parallel and concatenated. An
// empty payload yields a zero-value RelayInfo.
func (f *Fetcher) FetchRelayInfo(ctx context.Context) (RelayInfo, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	block0, err := f.FetchBlock(fetchCtx, protocol.RelayInfoChannel, 0)
	if err != nil {
		return RelayInfo{}, fmt.Errorf("fetch relay-info: %w", err)
	}
	if len(block0) < 2 {
		return RelayInfo{}, nil
	}
	totalBlocks := int(binary.BigEndian.Uint16(block0))
	if totalBlocks < 1 {
		totalBlocks = 1
	}
	// Fetch any remaining blocks + the signature ExtraBlock in parallel and
	// verify over the raw concatenation (which includes block 0's count
	// prefix — matching exactly what the server signed).
	raw, err := f.fetchRawAndVerify(fetchCtx, protocol.RelayInfoChannel, totalBlocks, block0)
	if err != nil {
		if errors.Is(err, ErrExtraBlockInvalid) {
			return RelayInfo{}, nil // tamper → ignore optional relay discovery
		}
		return RelayInfo{}, fmt.Errorf("fetch relay-info block: %w", err)
	}
	return ParseRelayInfo(raw[2:]), nil
}

// ParseRelayInfo decodes the relay-info payload (one "key=value" pair per
// line). Unknown keys are ignored so future relays can be added without
// breaking older clients.
func ParseRelayInfo(data []byte) RelayInfo {
	var info RelayInfo
	for _, line := range strings.Split(string(data), "\n") {
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		switch k {
		case "gh":
			info.GitHubRepo = v
		}
	}
	return info
}

// FetchTitles fetches and decodes the channel display name map from TitlesChannel.
// Returns an empty map (not an error) when the server does not support TitlesChannel.
// Block 0 carries a uint16 total-block count prefix; remaining blocks are fetched in
// parallel so the overall fetch is bounded by the slowest single block, not the sum.
func (f *Fetcher) FetchTitles(ctx context.Context) (map[string]string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	block0, err := f.FetchBlock(fetchCtx, protocol.TitlesChannel, 0)
	if err != nil || len(block0) < 2 {
		return map[string]string{}, nil
	}
	totalBlocks := int(binary.BigEndian.Uint16(block0))
	if totalBlocks < 1 {
		totalBlocks = 1
	}
	// Fetch any remaining blocks + the signature ExtraBlock in parallel and
	// verify over the raw concatenation (block 0 includes the count prefix,
	// matching the signed bytes). Any failure or tamper → empty map; titles
	// are optional and the UI falls back to channel handles.
	raw, err := f.fetchRawAndVerify(fetchCtx, protocol.TitlesChannel, totalBlocks, block0)
	if err != nil {
		return map[string]string{}, nil
	}
	titles, _ := protocol.DecodeTitlesData(raw[2:])
	if titles == nil {
		titles = map[string]string{}
	}
	return titles, nil
}

// ErrContentHashMismatch is returned when the fetched messages do not match
// the expected content hash from metadata.  This typically means the server
// regenerated its blocks between the metadata fetch and the block fetch
// (block-version race).  The caller should re-fetch metadata and retry.
var ErrContentHashMismatch = fmt.Errorf("content hash mismatch")

// FetchChannel fetches all blocks for a channel and returns the parsed messages.
// Cancelling ctx immediately aborts any queued or in-flight block fetches.
// Each block is retried individually via FetchBlock before the channel fetch fails.
func (f *Fetcher) FetchChannel(ctx context.Context, channelNum int, blockCount int) ([]protocol.Message, error) {
	return f.fetchChannelBlocks(ctx, channelNum, blockCount, f.FetchBlock)
}

// FetchChannelVerified works like FetchChannel but additionally verifies that
// the parsed messages match the expected content hash from metadata.
// Returns ErrContentHashMismatch when the hash does not match (block-version race).
func (f *Fetcher) FetchChannelVerified(ctx context.Context, channelNum int, blockCount int, expectedHash uint32) ([]protocol.Message, error) {
	msgs, err := f.fetchChannelBlocks(ctx, channelNum, blockCount, f.FetchBlock)
	if err != nil {
		return nil, err
	}
	if got := protocol.ContentHashOf(msgs); got != expectedHash {
		f.log("Channel %d content hash mismatch: got %08x, want %08x (block-version race?)", channelNum, got, expectedHash)
		return nil, ErrContentHashMismatch
	}
	return msgs, nil
}

func (f *Fetcher) fetchChannelBlocks(ctx context.Context, channelNum int, blockCount int, fetchFn func(context.Context, uint16, uint16) ([]byte, error)) ([]protocol.Message, error) {
	if blockCount <= 0 {
		return nil, nil
	}

	type blockResult struct {
		idx  int
		data []byte
		err  error
	}

	results := make(chan blockResult, blockCount)
	// Cap concurrent DNS queries at 5; the token-bucket rate limiter provides
	// the actual throughput control regardless of this concurrency cap.
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup

	for i := 0; i < blockCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Acquire semaphore or bail on cancellation.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- blockResult{idx: idx, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			data, err := fetchFn(ctx, uint16(channelNum), uint16(idx))
			results <- blockResult{idx: idx, data: data, err: err}
		}(i)
	}

	// When a key is pinned, fetch the signature ExtraBlock (index ==
	// blockCount) in the same parallel batch so verification adds no extra
	// round-trip. It does not feed `results` (the count stays blockCount); a
	// missing block (old server) is tolerated.
	var extraRaw []byte
	needExtra := f.serverPubKey != nil
	if needExtra {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if b, err := f.FetchBlock(ctx, uint16(channelNum), uint16(blockCount)); err == nil {
				extraRaw = b
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	ordered := make([][]byte, blockCount)
	// When a key is pinned the signature ExtraBlock is one more fetch, so show
	// the progress bar with one extra slot that fills once verification passes.
	progressTotal := blockCount
	if needExtra {
		progressTotal++
	}
	completed := 0
	for r := range results {
		if r.err != nil {
			if r.err == ctx.Err() {
				// Context cancelled — abort immediately.
				return nil, r.err
			}
			// FetchBlock already retried internally; log and treat as fatal for this channel.
			f.log("Channel %d block %d permanently failed: %v", channelNum, r.idx, r.err)
			return nil, fmt.Errorf("channel %d block %d: %w", channelNum, r.idx, r.err)
		}
		ordered[r.idx] = r.data
		completed++
		f.logProgress(fmt.Sprintf("Channel %d (%d/%d)", channelNum, completed, progressTotal), float64(completed), float64(progressTotal))
	}

	var allData []byte
	for _, b := range ordered {
		allData = append(allData, b...)
	}

	// Verify the server signature over the raw (pre-decompression) block
	// concatenation using the ExtraBlock fetched above. A present-but-invalid
	// signature is a hard failure (active tamper); a missing one (old server)
	// is tolerated so the feed still works, just unverified. Log the outcome
	// per channel so the verification state is visible.
	if f.serverPubKey != nil {
		switch verr := f.verifyExtraBytes(uint16(channelNum), allData, extraRaw); verr {
		case nil:
			f.log("[verify] channel %d: signature OK", channelNum)
		case ErrExtraBlockInvalid:
			f.log("[verify] channel %d: SIGNATURE INVALID — rejecting", channelNum)
			return nil, fmt.Errorf("channel %d: %w", channelNum, verr)
		default: // ErrUnverified
			f.log("[verify] channel %d: no signature block (UNVERIFIED)", channelNum)
		}
		// Fill the extra progress slot once the verification step is done.
		f.logProgress(fmt.Sprintf("Channel %d (%d/%d)", channelNum, progressTotal, progressTotal), float64(progressTotal), float64(progressTotal))
	}

	// Decompress if data has compression header
	decompressed, err := protocol.DecompressMessages(allData)
	if err != nil {
		// If the data starts with a known compression header but decompression
		// failed, the data is corrupt — do NOT raw-parse compressed bytes as
		// messages (that produces binary garbage as message text).
		if len(allData) > 0 && (allData[0] == 0x00 || allData[0] == 0x01) {
			return nil, fmt.Errorf("decompress channel %d: %w", channelNum, err)
		}
		// Unknown header → pre-compression era data; try raw parse.
		return protocol.ParseMessages(allData)
	}

	return protocol.ParseMessages(decompressed)
}

func (f *Fetcher) queryResolver(ctx context.Context, resolver, qname string) ([]byte, error) {
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}

	start := time.Now()
	resp, err := f.exchangeResolver(ctx, resolver, qname)
	latency := time.Since(start)
	if err != nil {
		f.RecordFailure(resolver)
		return nil, err
	}

	if resp.Rcode != dns.RcodeSuccess {
		f.RecordFailure(resolver)
		return nil, fmt.Errorf("dns error from %s: %s", resolver, dns.RcodeToString[resp.Rcode])
	}

	for _, ans := range resp.Answer {
		if txt, ok := ans.(*dns.TXT); ok {
			encoded := strings.Join(txt.Txt, "")
			data, err := protocol.DecodeResponse(f.responseKey, encoded)
			if err != nil {
				f.RecordFailure(resolver)
				return nil, err
			}
			f.RecordSuccess(resolver, latency)
			return data, nil
		}
	}

	f.RecordFailure(resolver)
	return nil, fmt.Errorf("no TXT record in response from %s", resolver)
}

func (f *Fetcher) exchangeResolver(ctx context.Context, resolver, qname string) (*dns.Msg, error) {
	resolverCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeTXT)
	m.RecursionDesired = true
	m.SetEdns0(4096, false)

	resp, _, err := f.exchangeFn(resolverCtx, m, resolver)
	if err != nil {
		return nil, fmt.Errorf("dns exchange with %s: %w", resolver, err)
	}
	return resp, nil
}

func (f *Fetcher) queryUpload(ctx context.Context, qname string) ([]byte, error) {
	if err := f.rateWait(ctx); err != nil {
		return nil, err
	}

	resolvers := f.Resolvers()
	if len(resolvers) == 0 {
		return nil, fmt.Errorf("no active resolvers")
	}

	shuffled := make([]string, len(resolvers))
	copy(shuffled, resolvers)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	var lastErr error
	for _, resolver := range shuffled {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		data, err := f.queryResolver(ctx, resolver, qname)
		if err != nil {
			lastErr = err
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

func splitUploadPayload(data []byte) [][]byte {
	chunks := make([][]byte, 0, (len(data)+protocol.MaxUpstreamBlockPayload-1)/protocol.MaxUpstreamBlockPayload)
	for len(data) > 0 {
		n := protocol.MaxUpstreamBlockPayload
		if n > len(data) {
			n = len(data)
		}
		chunk := make([]byte, n)
		copy(chunk, data[:n])
		chunks = append(chunks, chunk)
		data = data[n:]
	}
	return chunks
}

func randomSessionID() (uint16, error) {
	var buf [2]byte
	for {
		if _, err := cryptoRand.Read(buf[:]); err != nil {
			return 0, err
		}
		sessionID := binary.BigEndian.Uint16(buf[:])
		if sessionID != 0 {
			return sessionID, nil
		}
	}
}

func (f *Fetcher) sendUpstream(ctx context.Context, kind protocol.UpstreamKind, targetChannel uint16, payload []byte) ([]byte, error) {
	chunks := splitUploadPayload(payload)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	if len(chunks) > protocol.MaxUpstreamBlocks {
		return nil, fmt.Errorf("payload requires too many DNS blocks: %d > %d", len(chunks), protocol.MaxUpstreamBlocks)
	}

	sessionID, err := randomSessionID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}

	initQname, err := protocol.EncodeUpstreamInitQuery(f.queryKey, protocol.UpstreamInit{
		SessionID:     sessionID,
		TotalBlocks:   uint8(len(chunks)),
		Kind:          kind,
		TargetChannel: uint8(targetChannel),
	}, f.domain, f.queryMode)
	if err != nil {
		return nil, fmt.Errorf("encode upstream init: %w", err)
	}
	if f.debug {
		f.log("[debug] upstream init kind=%d blocks=%d qname=%s", kind, len(chunks), initQname)
	}

	data, err := f.queryUpload(ctx, initQname)
	if err != nil {
		return nil, fmt.Errorf("start upstream session: %w", err)
	}
	if string(data) != "READY" {
		return nil, fmt.Errorf("unexpected upstream init response: %s", string(data))
	}

	for idx, chunk := range chunks {
		blockQname, err := protocol.EncodeUpstreamBlockQuery(f.queryKey, sessionID, uint8(idx), chunk, f.domain, f.queryMode)
		if err != nil {
			return nil, fmt.Errorf("encode upstream block %d: %w", idx, err)
		}
		if f.debug {
			f.log("[debug] upstream block kind=%d idx=%d len=%d qname=%s", kind, idx, len(chunk), blockQname)
		}

		data, err = f.queryUpload(ctx, blockQname)
		if err != nil {
			return nil, fmt.Errorf("upload block %d: %w", idx, err)
		}

		if idx+1 < len(chunks) && string(data) != "CONTINUE" {
			return nil, fmt.Errorf("unexpected upstream block response: %s", string(data))
		}
	}

	return data, nil
}

// SendMessage sends a text message to the given channel via chunked upstream DNS queries.
// Returns an error if the message is too long or sending fails.
func (f *Fetcher) SendMessage(ctx context.Context, channelNum int, text string) error {
	data, err := f.sendUpstream(ctx, protocol.UpstreamKindSend, uint16(channelNum), []byte(text))
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	if string(data) != "OK" {
		return fmt.Errorf("unexpected response: %s", string(data))
	}
	return nil
}

// SendAdminCommand sends an admin command to the server via chunked upstream DNS queries.
// The payload is a single AdminCmd byte followed by the argument string.
func (f *Fetcher) SendAdminCommand(ctx context.Context, cmd protocol.AdminCmd, arg string) (string, error) {
	payload := append([]byte{byte(cmd)}, []byte(arg)...)
	data, err := f.sendUpstream(ctx, protocol.UpstreamKindAdmin, 0, payload)
	if err != nil {
		return "", fmt.Errorf("admin command failed: %w", err)
	}
	return string(data), nil
}
