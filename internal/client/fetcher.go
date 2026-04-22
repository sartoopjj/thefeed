package client

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
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
	domain      string
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

	debug   bool
	logFunc LogFunc

	// Resolver scoring: per-resolver success/failure counters and latency.
	stats sync.Map // string (resolver:port) -> *resolverStat

	// scatter is how many resolvers to query simultaneously per DNS block request.
	// 1 = sequential (no scatter), 2+ = fan-out (use fastest response).
	scatter int

	// exchangeFn is the function used to send a DNS message to a resolver.
	// It defaults to a real dns.Client exchange and can be replaced in tests.
	exchangeFn func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error)
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
	f.exchangeFn = func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		c := &dns.Client{Timeout: f.timeout, Net: "udp"}
		return c.ExchangeContext(ctx, m, addr)
	}
	return f, nil
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

// SetTimeout sets the per-query DNS timeout.
func (f *Fetcher) SetTimeout(d time.Duration) {
	f.timeout = d
}

// SetLogFunc sets the debug log callback.
func (f *Fetcher) SetLogFunc(fn LogFunc) {
	f.logFunc = fn
}

// SetDebug enables or disables debug logging of generated query names.
func (f *Fetcher) SetDebug(debug bool) {
	f.debug = debug
}

// SetQueryMode sets the DNS query encoding mode.
func (f *Fetcher) SetQueryMode(mode protocol.QueryEncoding) {
	f.queryMode = mode
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

// RecordSuccess records a successful DNS query for the given resolver.
func (f *Fetcher) RecordSuccess(resolver string, latency time.Duration) {
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}
	v, _ := f.stats.LoadOrStore(resolver, &resolverStat{})
	s := v.(*resolverStat)
	atomic.AddInt64(&s.success, 1)
	atomic.AddInt64(&s.totalMs, latency.Milliseconds())
}

// RecordFailure records a failed DNS query for the given resolver.
func (f *Fetcher) RecordFailure(resolver string) {
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}
	v, _ := f.stats.LoadOrStore(resolver, &resolverStat{})
	s := v.(*resolverStat)
	atomic.AddInt64(&s.failure, 1)
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

// Start launches background goroutines (rate limiter and noise generator).
// ctx controls their lifetime — cancel it to cleanly stop them.
// Call once per fetcher configuration; creating a new fetcher replaces the old one.
func (f *Fetcher) Start(ctx context.Context) {
	if f.rateQPS > 0 {
		f.log("fetcher started: %d configured resolvers, rate=%.1f q/s, scatter=%d", len(f.allResolvers), f.rateQPS, f.scatter)
		f.rateCh = make(chan struct{}, 1)
		go f.runRateLimiter(ctx)
		go f.runNoise(ctx)
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
// On transient failure it retries up to 2 additional times with a short back-off.
func (f *Fetcher) FetchBlock(ctx context.Context, channel, block uint16) ([]byte, error) {
	const maxAttempts = 20
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

		qname, err := protocol.EncodeQuery(f.queryKey, channel, block, f.domain, f.queryMode)
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

// FetchMetadata fetches and parses the metadata block (channel 0).
func (f *Fetcher) FetchMetadata(ctx context.Context) (*protocol.Metadata, error) {
	data, err := f.FetchBlock(ctx, protocol.MetadataChannel, 0)
	if err != nil {
		return nil, fmt.Errorf("fetch metadata block 0: %w", err)
	}

	meta, err := protocol.ParseMetadata(data)
	if err == nil {
		return meta, nil
	}

	// Metadata may span multiple blocks.
	allData := make([]byte, len(data))
	copy(allData, data)

	for blk := uint16(1); blk < 10; blk++ {
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
			return meta, nil
		}
	}

	return nil, fmt.Errorf("could not parse metadata: %w", err)
}

// FetchLatestVersion fetches the latest release version from the dedicated
// version channel. The block is padded to a random size matching regular content
// blocks (DPI-resistant). Empty string means unknown/unavailable.
func (f *Fetcher) FetchLatestVersion(ctx context.Context) (string, error) {
	data, err := f.FetchBlock(ctx, protocol.VersionChannel, 0)
	if err != nil {
		return "", fmt.Errorf("fetch version block: %w", err)
	}
	return protocol.DecodeVersionData(data)
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
	payload0 := block0[2:]

	if totalBlocks <= 1 {
		titles, _ := protocol.DecodeTitlesData(payload0)
		if titles == nil {
			titles = map[string]string{}
		}
		return titles, nil
	}

	// Fetch remaining blocks in parallel.
	type blockResult struct {
		data []byte
		err  error
	}
	results := make([]blockResult, totalBlocks)
	results[0] = blockResult{data: payload0}

	var wg sync.WaitGroup
	for blk := 1; blk < totalBlocks; blk++ {
		wg.Add(1)
		go func(blk int) {
			defer wg.Done()
			data, fetchErr := f.FetchBlock(fetchCtx, protocol.TitlesChannel, uint16(blk))
			results[blk] = blockResult{data: data, err: fetchErr}
		}(blk)
	}
	wg.Wait()

	var allData []byte
	for _, r := range results {
		if r.err != nil {
			return map[string]string{}, nil
		}
		allData = append(allData, r.data...)
	}
	titles, _ := protocol.DecodeTitlesData(allData)
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

	go func() {
		wg.Wait()
		close(results)
	}()

	ordered := make([][]byte, blockCount)
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
		f.logProgress(fmt.Sprintf("Channel %d (%d/%d)", channelNum, completed, blockCount), float64(completed), float64(blockCount))
	}

	var allData []byte
	for _, b := range ordered {
		allData = append(allData, b...)
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
