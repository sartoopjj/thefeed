package client

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// ResolverChecker periodically probes the fetcher's configured resolvers and
// updates the active (healthy) resolver pool. It replaces the old file/CIDR
// scanner — no file I/O; just a plain DNS probe on channel 0.
type ResolverChecker struct {
	fetcher *Fetcher
	timeout time.Duration
	logFunc LogFunc
}

// NewResolverChecker creates a health checker for the resolvers in fetcher.
// timeout is the per-probe deadline; 0 uses a 5-second default.
func NewResolverChecker(fetcher *Fetcher, timeout time.Duration) *ResolverChecker {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &ResolverChecker{
		fetcher: fetcher,
		timeout: timeout,
	}
}

// SetLogFunc sets the callback used to emit health-check results to the log panel.
func (rc *ResolverChecker) SetLogFunc(fn LogFunc) {
	rc.logFunc = fn
}

// Start begins the periodic health-check loop in the background.
// An initial check runs immediately; subsequent checks happen every 10 minutes.
// ctx controls the lifetime — cancel it to stop the checker.
func (rc *ResolverChecker) Start(ctx context.Context) {
	go func() {
		rc.CheckNow()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rc.CheckNow()
			}
		}
	}()
}

// CheckNow runs a single resolver health-check pass immediately.
func (rc *ResolverChecker) CheckNow() {
	resolvers := rc.fetcher.AllResolvers()
	if len(resolvers) == 0 {
		return
	}

	rc.log("Checking %d resolver(s)...", len(resolvers))

	var healthy []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // probe up to 10 resolvers concurrently

	for _, r := range resolvers {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if rc.checkOne(r) {
				mu.Lock()
				healthy = append(healthy, r)
				mu.Unlock()
				rc.log("Resolver OK: %s", r)
			} else {
				rc.log("Resolver failed: %s", r)
			}
		}(r)
	}
	wg.Wait()

	rc.fetcher.SetActiveResolvers(healthy)
	if len(healthy) == 0 {
		rc.log("Resolver check done: 0/%d healthy", len(resolvers))
		return
	}
	rc.log("Resolver check done: %d/%d healthy", len(healthy), len(resolvers))
}

// checkOne probes a single resolver by sending a metadata channel query
// (channel 0, block 0). A successful DNS response (any rcode that isn't a
// network/timeout error) means the resolver is reachable and understands the domain.
func (rc *ResolverChecker) checkOne(resolver string) bool {
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}

	qname, err := protocol.EncodeQuery(
		rc.fetcher.queryKey,
		protocol.MetadataChannel, 0,
		rc.fetcher.domain,
		rc.fetcher.queryMode,
	)
	if err != nil {
		return false
	}

	c := &dns.Client{Timeout: rc.timeout}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeTXT)
	m.RecursionDesired = true
	m.SetEdns0(4096, false)

	resp, _, err := c.Exchange(m, resolver)
	// We consider the resolver healthy if we get any DNS response back
	// (even NXDOMAIN means the resolver forwarded the query to our server).
	return err == nil && resp != nil
}

func (rc *ResolverChecker) log(format string, args ...any) {
	if rc.logFunc != nil {
		rc.logFunc(fmt.Sprintf(format, args...))
	}
}
