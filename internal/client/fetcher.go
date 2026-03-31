package client

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"sync"
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
}

// NewFetcher creates a new DNS block fetcher.
func NewFetcher(domain, passphrase string, resolvers []string) (*Fetcher, error) {
	qk, rk, err := protocol.DeriveKeys(passphrase)
	if err != nil {
		return nil, fmt.Errorf("derive keys: %w", err)
	}

	r := make([]string, len(resolvers))
	copy(r, resolvers)

	return &Fetcher{
		domain:          strings.TrimSuffix(domain, "."),
		queryKey:        qk,
		responseKey:     rk,
		queryMode:       protocol.QuerySingleLabel,
		allResolvers:    r,
		activeResolvers: r,
		timeout:         30 * time.Second,
	}, nil
}

// SetRateLimit sets the maximum queries per second (0 = unlimited). Must be called before Start.
func (f *Fetcher) SetRateLimit(qps float64) {
	f.rateQPS = qps
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

// Start launches background goroutines (rate limiter and noise generator).
// ctx controls their lifetime — cancel it to cleanly stop them.
// Call once per fetcher configuration; creating a new fetcher replaces the old one.
func (f *Fetcher) Start(ctx context.Context) {
	if f.rateQPS > 0 {
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
	const maxAttempts = 10
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
		if f.debug {
			f.log("[debug] query ch=%d blk=%d attempt=%d qname=%s", channel, block, attempt+1, qname)
		}

		resolvers := f.Resolvers()
		if len(resolvers) == 0 {
			return nil, fmt.Errorf("no active resolvers")
		}

		// Shuffle to spread load across resolvers.
		shuffled := make([]string, len(resolvers))
		copy(shuffled, resolvers)
		rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		for _, resolver := range shuffled {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			data, err := f.queryResolver(ctx, resolver, qname)
			if err != nil {
				lastErr = err
				continue
			}
			if f.debug {
				f.log("[debug] response ch=%d blk=%d len=%d", channel, block, len(data))
			}
			return data, nil
		}
		lastErr = fmt.Errorf("all resolvers failed: %w", lastErr)
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

// FetchChannel fetches all blocks for a channel and returns the parsed messages.
// Cancelling ctx immediately aborts any queued or in-flight block fetches.
// Each block is retried individually via FetchBlock before the channel fetch fails.
func (f *Fetcher) FetchChannel(ctx context.Context, channelNum int, blockCount int) ([]protocol.Message, error) {
	return f.fetchChannelBlocks(ctx, channelNum, blockCount, f.FetchBlock)
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
		// Fall back to raw parse for backward compatibility with uncompressed data
		return protocol.ParseMessages(allData)
	}

	return protocol.ParseMessages(decompressed)
}

func (f *Fetcher) queryResolver(ctx context.Context, resolver, qname string) ([]byte, error) {
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}

	resp, err := f.exchangeResolver(ctx, resolver, qname)
	if err != nil {
		return nil, err
	}

	if resp.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("dns error from %s: %s", resolver, dns.RcodeToString[resp.Rcode])
	}

	for _, ans := range resp.Answer {
		if txt, ok := ans.(*dns.TXT); ok {
			encoded := strings.Join(txt.Txt, "")
			return protocol.DecodeResponse(f.responseKey, encoded)
		}
	}

	return nil, fmt.Errorf("no TXT record in response from %s", resolver)
}

func (f *Fetcher) exchangeResolver(ctx context.Context, resolver, qname string) (*dns.Msg, error) {
	resolverCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	c := &dns.Client{Timeout: f.timeout, Net: "udp"}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeTXT)
	m.RecursionDesired = true
	m.SetEdns0(4096, false)

	resp, _, err := c.ExchangeContext(resolverCtx, m, resolver)
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
