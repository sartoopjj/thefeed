package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// ScannerState represents the current state of the scanner.
type ScannerState string

const (
	ScannerIdle    ScannerState = "idle"
	ScannerRunning ScannerState = "running"
	ScannerPaused  ScannerState = "paused"
	ScannerDone    ScannerState = "done"
)

// ScannerConfig holds the configuration for a resolver scan.
type ScannerConfig struct {
	// Targets is a list of IPs, domains, or CIDRs to scan.
	Targets []string `json:"targets"`
	// MaxIPs limits how many IPs to scan from the expanded list (0 = all).
	MaxIPs int `json:"maxIPs"`
	// RateLimit is the concurrent probe limit (default 50).
	RateLimit int `json:"rateLimit"`
	// Timeout is the per-probe timeout in seconds (default 10).
	Timeout float64 `json:"timeout"`
	// ExpandSubnet: if true, when a working resolver is found, also scan its /24.
	ExpandSubnet bool `json:"expandSubnet"`
	// QueryMode is "single" or "double".
	QueryMode string `json:"queryMode"`
	// Domain is the thefeed server domain.
	Domain string `json:"domain"`
	// Passphrase is the encryption key.
	Passphrase string `json:"passphrase"`
}

// ScannerResult represents a single working resolver found by the scanner.
type ScannerResult struct {
	IP        string  `json:"ip"`
	LatencyMs float64 `json:"latencyMs"` // milliseconds
	FoundAt   int64   `json:"foundAt"`   // unix timestamp
}

// ScannerProgress holds the current progress of the scanner.
type ScannerProgress struct {
	State     ScannerState    `json:"state"`
	Total     int             `json:"total"`
	Scanned   int             `json:"scanned"`
	Found     int             `json:"found"`
	Results   []ScannerResult `json:"results"`
	Error     string          `json:"error,omitempty"`
	StartedAt int64           `json:"startedAt,omitempty"`
}

// ResolverScanner scans IP ranges to find working DNS resolvers for thefeed.
type ResolverScanner struct {
	mu       sync.Mutex
	state    ScannerState
	ctx      context.Context    // main scan context
	cancel   context.CancelFunc // cancels the main scan context
	pauseCh  chan struct{}
	resumeCh chan struct{}
	logFunc  LogFunc

	// probeCtx is a child of ctx that is cancelled on pause/stop to abort
	// in-flight DNS exchanges immediately. Recreated on resume.
	probeCtx    context.Context
	probeCancel context.CancelFunc

	// Progress tracking (atomic for concurrent reads).
	total   atomic.Int64
	scanned atomic.Int64

	// Results are protected by resultMu.
	resultMu sync.Mutex
	results  []ScannerResult

	// Error from the scan.
	scanErr atomic.Value // stores string

	startedAt int64

	debug bool // when true, log individual probe queries/responses

	// expandedIPs tracks /24 subnets already expanded so we don't re-expand.
	expandMu     sync.Mutex
	expandedNets map[string]bool
	expandQueue  chan string // IPs whose /24 needs scanning
}

// SetDebug enables or disables verbose scanner logging.
func (rs *ResolverScanner) SetDebug(v bool) {
	rs.debug = v
}

// NewResolverScanner creates a new scanner instance.
func NewResolverScanner() *ResolverScanner {
	return &ResolverScanner{
		state:        ScannerIdle,
		expandedNets: make(map[string]bool),
	}
}

// SetLogFunc sets the log callback.
func (rs *ResolverScanner) SetLogFunc(fn LogFunc) {
	rs.logFunc = fn
}

// State returns the current scanner state.
func (rs *ResolverScanner) State() ScannerState {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.state
}

// Progress returns the current scan progress.
func (rs *ResolverScanner) Progress() ScannerProgress {
	rs.mu.Lock()
	state := rs.state
	rs.mu.Unlock()

	rs.resultMu.Lock()
	results := make([]ScannerResult, len(rs.results))
	copy(results, rs.results)
	rs.resultMu.Unlock()

	total := int(rs.total.Load())
	scanned := int(rs.scanned.Load())

	// If all IPs have been scanned but goroutine hasn't exited yet, report done.
	if state == ScannerRunning && total > 0 && scanned >= total {
		state = ScannerDone
	}

	errVal := rs.scanErr.Load()
	var errStr string
	if errVal != nil {
		errStr = errVal.(string)
	}

	return ScannerProgress{
		State:     state,
		Total:     total,
		Scanned:   scanned,
		Found:     len(results),
		Results:   results,
		Error:     errStr,
		StartedAt: rs.startedAt,
	}
}

// Start begins scanning with the given config. Returns error if already running.
func (rs *ResolverScanner) Start(cfg ScannerConfig) error {
	rs.mu.Lock()
	if rs.state == ScannerRunning || rs.state == ScannerPaused {
		// Allow restart if the scan is effectively complete (all IPs
		// scanned) but the goroutine hasn't returned yet.
		total := rs.total.Load()
		scanned := rs.scanned.Load()
		if total <= 0 || scanned < total {
			rs.mu.Unlock()
			return fmt.Errorf("scanner already running")
		}
		// Effectively done — cancel the lingering goroutine and proceed.
		if rs.cancel != nil {
			rs.cancel()
		}
		rs.state = ScannerIdle
	}

	// Derive keys.
	qk, rk, err := protocol.DeriveKeys(cfg.Passphrase)
	if err != nil {
		rs.mu.Unlock()
		return fmt.Errorf("invalid passphrase: %w", err)
	}

	// Parse query mode.
	queryMode := protocol.QuerySingleLabel
	if cfg.QueryMode == "double" {
		queryMode = protocol.QueryMultiLabel
	}

	// Expand targets to IPs.
	ips, err := expandTargets(cfg.Targets)
	if err != nil {
		rs.mu.Unlock()
		return fmt.Errorf("expand targets: %w", err)
	}
	if len(ips) == 0 {
		rs.mu.Unlock()
		return fmt.Errorf("no IPs to scan")
	}

	// Shuffle IPs.
	rand.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })

	// Apply maxIPs limit.
	if cfg.MaxIPs > 0 && cfg.MaxIPs < len(ips) {
		ips = ips[:cfg.MaxIPs]
	}

	// Set defaults.
	rateLimit := cfg.RateLimit
	if rateLimit <= 0 {
		rateLimit = 50
	}
	timeout := time.Duration(cfg.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	domain := strings.TrimSuffix(cfg.Domain, ".")

	// Reset state.
	ctx, cancel := context.WithCancel(context.Background())
	rs.ctx = ctx
	rs.cancel = cancel
	rs.probeCtx, rs.probeCancel = context.WithCancel(ctx)
	rs.pauseCh = make(chan struct{})
	rs.resumeCh = make(chan struct{})
	rs.state = ScannerRunning
	rs.total.Store(int64(len(ips)))
	rs.scanned.Store(0)
	rs.resultMu.Lock()
	rs.results = nil
	rs.resultMu.Unlock()
	rs.scanErr.Store("")
	rs.startedAt = time.Now().Unix()
	rs.expandMu.Lock()
	rs.expandedNets = make(map[string]bool)
	rs.expandMu.Unlock()

	if cfg.ExpandSubnet {
		rs.expandQueue = make(chan string, 1000)
	} else {
		rs.expandQueue = nil
	}

	rs.mu.Unlock()

	rs.log("SCANNER_START %d", len(ips))
	rs.log("Scanner started: probing %d IPs (concurrency=%d, timeout=%.0fs)", len(ips), rateLimit, timeout.Seconds())

	go rs.runScan(ctx, ips, qk, rk, domain, queryMode, rateLimit, timeout, cfg.ExpandSubnet)
	return nil
}

// Stop stops the scanner.
func (rs *ResolverScanner) Stop() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.probeCancel != nil {
		rs.probeCancel() // cancel in-flight probes immediately
	}
	if rs.cancel != nil {
		rs.cancel()
	}
	rs.state = ScannerDone
}

// Pause pauses the scanner.
func (rs *ResolverScanner) Pause() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.state == ScannerRunning {
		rs.state = ScannerPaused
		if rs.probeCancel != nil {
			rs.probeCancel() // cancel in-flight probes immediately
		}
		rs.pauseCh = make(chan struct{})
		close(rs.pauseCh) // signal pause
		rs.resumeCh = make(chan struct{})
		rs.log("Scanner paused")
	}
}

// Resume resumes the scanner from pause.
func (rs *ResolverScanner) Resume() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.state == ScannerPaused {
		rs.state = ScannerRunning
		// Recreate probe context so new probes can proceed.
		rs.probeCtx, rs.probeCancel = context.WithCancel(rs.ctx)
		close(rs.resumeCh) // signal resume
		rs.log("Scanner resumed")
	}
}

func (rs *ResolverScanner) runScan(ctx context.Context, ips []string, qk, rk [protocol.KeySize]byte, domain string, queryMode protocol.QueryEncoding, rateLimit int, timeout time.Duration, expandSubnet bool) {
	defer func() {
		rs.mu.Lock()
		if rs.state != ScannerDone {
			rs.state = ScannerDone
		}
		rs.mu.Unlock()
		total := int(rs.total.Load())
		scanned := int(rs.scanned.Load())
		rs.resultMu.Lock()
		found := len(rs.results)
		rs.resultMu.Unlock()
		rs.log("SCANNER_DONE %d/%d found=%d", scanned, total, found)
		rs.log("Scanner finished: %d/%d scanned, %d working resolvers found", scanned, total, found)
	}()

	// Feed IPs through a channel so dispatch can be paused.
	ipCh := make(chan string, rateLimit)
	var wg sync.WaitGroup

	// Worker pool: rateLimit workers.
	for w := 0; w < rateLimit; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range ipCh {
				if ctx.Err() != nil {
					rs.scanned.Add(1)
					continue
				}

				// Check for pause before probing.
				rs.mu.Lock()
				paused := rs.state == ScannerPaused
				var resumeCh chan struct{}
				if paused {
					resumeCh = rs.resumeCh
				}
				rs.mu.Unlock()

				if paused && resumeCh != nil {
					select {
					case <-resumeCh:
					case <-ctx.Done():
						rs.scanned.Add(1)
						continue
					}
				}

				// Get the current probe context (cancelled on pause/stop).
				rs.mu.Lock()
				pCtx := rs.probeCtx
				rs.mu.Unlock()

				latency, ok := rs.probeResolver(pCtx, ip, qk, rk, domain, queryMode, timeout)
				rs.scanned.Add(1)

				scanned := int(rs.scanned.Load())
				total := int(rs.total.Load())
				if scanned%100 == 0 || scanned == total {
					rs.resultMu.Lock()
					found := len(rs.results)
					rs.resultMu.Unlock()
					rs.log("SCANNER_PROGRESS %d/%d found=%d", scanned, total, found)
				}

				if ok {
					result := ScannerResult{
						IP:        ip,
						LatencyMs: float64(latency.Milliseconds()),
						FoundAt:   time.Now().Unix(),
					}
					rs.resultMu.Lock()
					rs.results = append(rs.results, result)
					rs.resultMu.Unlock()
					rs.log("Scanner: resolver OK %s (%.0fms)", ip, result.LatencyMs)

					if expandSubnet {
						rs.expandMu.Lock()
						eq := rs.expandQueue
						if eq != nil {
							select {
							case eq <- ip:
							default:
							}
						}
						rs.expandMu.Unlock()
					}
				}
			}
		}()
	}

	// If expandSubnet is enabled, start the expand goroutine now so it
	// processes found IPs concurrently with primary dispatch.
	var expandDone chan struct{}
	if expandSubnet && rs.expandQueue != nil {
		expandDone = make(chan struct{})
		go func() {
			defer close(expandDone)
			for {
				select {
				case <-ctx.Done():
					return
				case foundIP, ok := <-rs.expandQueue:
					if !ok {
						return
					}
					rs.expandSubnetOf(ctx, foundIP, ips, qk, rk, domain, queryMode, timeout, ipCh)
				}
			}
		}()
	}

	// Dispatch IPs, respecting pause.
	for _, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		// Check for pause before dispatching.
		for {
			rs.mu.Lock()
			paused := rs.state == ScannerPaused
			var resumeCh chan struct{}
			if paused {
				resumeCh = rs.resumeCh
			}
			rs.mu.Unlock()
			if !paused {
				break
			}
			select {
			case <-resumeCh:
			case <-ctx.Done():
				goto doneDispatch
			}
		}
		select {
		case ipCh <- ip:
		case <-ctx.Done():
			goto doneDispatch
		}
	}
doneDispatch:

	// Safely close expandQueue so no worker can send to a closed channel.
	if expandSubnet {
		rs.expandMu.Lock()
		eq := rs.expandQueue
		rs.expandQueue = nil
		rs.expandMu.Unlock()
		if eq != nil {
			close(eq)
		}
		if expandDone != nil {
			<-expandDone
		}
	}

	close(ipCh)
	wg.Wait()

	// Sort results by latency.
	rs.resultMu.Lock()
	sort.Slice(rs.results, func(i, j int) bool {
		return rs.results[i].LatencyMs < rs.results[j].LatencyMs
	})
	rs.resultMu.Unlock()
}

func (rs *ResolverScanner) expandSubnetOf(ctx context.Context, foundIP string, alreadyScanned []string, qk, rk [protocol.KeySize]byte, domain string, queryMode protocol.QueryEncoding, timeout time.Duration, ipCh chan<- string) {
	ip := net.ParseIP(foundIP)
	if ip == nil {
		return
	}
	ip = ip.To4()
	if ip == nil {
		return // skip IPv6
	}

	// Get the /24 prefix.
	prefix := fmt.Sprintf("%d.%d.%d", ip[0], ip[1], ip[2])

	rs.expandMu.Lock()
	if rs.expandedNets[prefix] {
		rs.expandMu.Unlock()
		return
	}
	rs.expandedNets[prefix] = true
	rs.expandMu.Unlock()

	// Build a set of already-known IPs for quick lookup.
	known := make(map[string]bool, len(alreadyScanned))
	for _, s := range alreadyScanned {
		known[s] = true
	}
	rs.resultMu.Lock()
	for _, r := range rs.results {
		known[r.IP] = true
	}
	rs.resultMu.Unlock()

	// Generate all /24 IPs that aren't in the known set.
	var newIPs []string
	for i := 1; i < 255; i++ {
		candidate := fmt.Sprintf("%s.%d", prefix, i)
		addr := candidate + ":53"
		if !known[candidate] && !known[addr] {
			newIPs = append(newIPs, candidate)
		}
	}

	if len(newIPs) == 0 {
		return
	}

	rs.log("Scanner: expanding /24 of %s — scanning %d additional IPs", foundIP, len(newIPs))
	rs.total.Add(int64(len(newIPs)))

	for _, newIP := range newIPs {
		if ctx.Err() != nil {
			break
		}
		select {
		case ipCh <- newIP:
		case <-ctx.Done():
			return
		}
	}
}

func (rs *ResolverScanner) probeResolver(ctx context.Context, ip string, qk, rk [protocol.KeySize]byte, domain string, queryMode protocol.QueryEncoding, timeout time.Duration) (time.Duration, bool) {
	resolver := ip
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}

	qname, err := protocol.EncodeQuery(qk, protocol.MetadataChannel, 0, domain, queryMode)
	if err != nil {
		if rs.debug {
			rs.log("[debug] scanner probe %s: encode error: %v", ip, err)
		}
		return 0, false
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := &dns.Client{Timeout: timeout, Net: "udp"}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeTXT)
	m.RecursionDesired = true
	m.SetEdns0(4096, false)

	if rs.debug {
		rs.log("[debug] scanner query %s qname=%s", ip, qname)
	}

	start := time.Now()
	resp, _, err := c.ExchangeContext(probeCtx, m, resolver)
	latency := time.Since(start)

	if err != nil || resp == nil {
		if rs.debug {
			rs.log("[debug] scanner probe %s: err=%v latency=%s", ip, err, latency)
		}
		return 0, false
	}

	for _, ans := range resp.Answer {
		if txt, ok := ans.(*dns.TXT); ok {
			encoded := strings.Join(txt.Txt, "")
			_, decErr := protocol.DecodeResponse(rk, encoded)
			if decErr == nil {
				if rs.debug {
					rs.log("[debug] scanner probe %s: OK latency=%s", ip, latency)
				}
				return latency, true
			}
			if rs.debug {
				rs.log("[debug] scanner probe %s: decode failed: %v", ip, decErr)
			}
		}
	}
	if rs.debug {
		rs.log("[debug] scanner probe %s: no valid TXT record (answers=%d)", ip, len(resp.Answer))
	}
	return 0, false
}

func (rs *ResolverScanner) log(format string, args ...any) {
	if rs.logFunc != nil {
		rs.logFunc(fmt.Sprintf(format, args...))
	}
}

// expandTargets expands a list of IPs, domains, and CIDRs into individual IP strings.
func expandTargets(targets []string) ([]string, error) {
	var result []string
	seen := make(map[string]bool)

	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}

		// Try as CIDR.
		if strings.Contains(target, "/") {
			ips, err := expandCIDR(target)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", target, err)
			}
			for _, ip := range ips {
				if !seen[ip] {
					seen[ip] = true
					result = append(result, ip)
				}
			}
			continue
		}

		// Try as IP.
		if ip := net.ParseIP(target); ip != nil {
			s := ip.String()
			if !seen[s] {
				seen[s] = true
				result = append(result, s)
			}
			continue
		}

		// Try as IP:port (e.g. "1.2.3.4:53").
		if host, _, splitErr := net.SplitHostPort(target); splitErr == nil {
			if ip := net.ParseIP(host); ip != nil {
				s := ip.String()
				if !seen[s] {
					seen[s] = true
					result = append(result, s)
				}
				continue
			}
		}

		// Try as domain — resolve to IPs.
		addrs, err := net.LookupHost(target)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve %q: %w", target, err)
		}
		for _, addr := range addrs {
			if !seen[addr] {
				seen[addr] = true
				result = append(result, addr)
			}
		}
	}

	return result, nil
}

// expandCIDR expands a CIDR to individual IP addresses, skipping network and broadcast
// addresses for IPv4 networks larger than /31.
func expandCIDR(cidr string) ([]string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	mask := network.Mask
	ones, bits := mask.Size()

	// For IPv4, limit to /16 to avoid memory issues.
	if bits == 32 && ones < 16 {
		return nil, fmt.Errorf("CIDR %s is too large (minimum /16)", cidr)
	}

	var ips []string
	ip := make(net.IP, len(network.IP))
	copy(ip, network.IP)

	for {
		if !network.Contains(ip) {
			break
		}
		// Skip network and broadcast addresses for networks > /31.
		if bits == 32 && ones < 31 {
			// Check if it's the network address (all host bits zero)
			// or broadcast address (all host bits one).
			hostBits := bits - ones
			if hostBits > 1 {
				ipInt := ipToUint32(ip.To4())
				netInt := ipToUint32(network.IP.To4())
				hostMask := uint32((1 << hostBits) - 1)
				hostPart := ipInt - netInt
				if hostPart == 0 || hostPart == hostMask {
					incrementIP(ip)
					continue
				}
			}
		}
		ips = append(ips, ip.String())
		incrementIP(ip)
	}

	return ips, nil
}

func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func ipToUint32(ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip[:4])
}
