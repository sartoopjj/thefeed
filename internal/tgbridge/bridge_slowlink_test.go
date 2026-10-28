package tgbridge

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"errors"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tgtest/cluster"
	"github.com/gotd/td/transport"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
)

// queryMeter counts cells by op, so a slow link's budget can be split into
// "bytes moved" and "asked whether anything arrived".
type queryMeter struct {
	mu    sync.Mutex
	data  int // DATA / DATAEND: carries upstream bytes
	poll  int // POLL: carries nothing, asks for downstream
	other int // OPEN / CLOSE / handshake
	up    int // upstream bytes delivered
	down  int // downstream bytes received
}

func (m *queryMeter) record(plain []byte, reply []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case len(plain) == 0:
		m.other++
	case plain[0] == protocol.MTPOpData || plain[0] == protocol.MTPOpDataEnd:
		m.data++
		if len(plain) > protocol.MTPDataHeader {
			m.up += len(plain) - protocol.MTPDataHeader
		}
	case plain[0] == protocol.MTPOpPoll:
		m.poll++
	default:
		m.other++
	}
	if r, err := protocol.ParseMTPReply(reply); err == nil {
		m.down += len(r.Down)
	}
}

func (m *queryMeter) totals() (data, poll, other, up, down int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data, m.poll, m.other, m.up, m.down
}

// slowLink is the client's own rate limiter: one token per interval into a
// channel of capacity 1, so tokens never accumulate into a burst and a caller
// that gives up takes nothing with it.
type slowLink struct {
	tokens chan struct{}
}

func newSlowLink(ctx context.Context, gap time.Duration) *slowLink {
	s := &slowLink{tokens: make(chan struct{}, 1)}
	if gap <= 0 {
		return s
	}
	go func() {
		t := time.NewTicker(gap)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case s.tokens <- struct{}{}:
				default:
				}
			}
		}
	}()
	return s
}

func (s *slowLink) wait(ctx context.Context) error {
	if s.tokens == nil {
		return nil
	}
	select {
	case <-s.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// queryTimeoutForTest matches the client's configured DNS timeout.
const queryTimeoutForTest = 10 * time.Second

var errSimulatedDrop = errors.New("simulated: no answer")

func envDuration(key string) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 0
}

func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// slowLinkRig is the whole path — DNS server, relay, stand-in Telegram — with
// a rate limit and a meter on the tunnel.
type slowLinkRig struct {
	exchange func(context.Context, []byte) ([]byte, error)
	chunk    int
	window   int
	cluster  *cluster.Cluster
	session  string
	meter    *queryMeter
}

func newSlowLinkRig(t *testing.T, ctx context.Context, gap time.Duration, cellBudget int) *slowLinkRig {
	t.Helper()
	const passphrase = "slowlink"
	const feedDomain = "t.example.com"
	const mtpDomain = "m.example.com"
	const dcID = 2

	tg := cluster.NewCluster(cluster.Options{Protocol: transport.Abridged})
	tg.DC(dcID, "dc2")
	go func() { _ = tg.Up(ctx) }()
	select {
	case <-tg.Ready():
	case <-ctx.Done():
		t.Fatal("stand-in Telegram never became ready")
	}
	dcAddr := ""
	for _, o := range tg.List().Options {
		if o.ID == dcID {
			dcAddr = net.JoinHostPort(o.IPAddress, itoa(o.Port))
		}
	}

	signPub, signKey, err := ed25519.GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdh.X25519().GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	qk, rk, err := protocol.DeriveKeys(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	relay := server.NewMTProtoRelay(server.MTProtoConfig{
		Enabled: true, Domains: []string{mtpDomain}, Ek: ek, QueryKey: qk,
		IdleTTL: 5 * time.Minute, DialTimeout: 10 * time.Second,
		DCAddrs: map[uint8]string{dcID: dcAddr},
		// "session lost" has three different causes and the client cannot tell
		// them apart; the relay's own log can.
		Debug: true,
	})
	feed := server.NewFeed(nil)
	feed.SetSigningKey(signKey)
	feed.SetMTProtoAvailable(true)
	feed.SetMTProtoInfoPayload(protocol.EncodeMTPInfo(relay.Info()))

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsAddr := pc.LocalAddr().String()
	pc.Close()
	dnsSrv := server.NewDNSServer(dnsAddr, feedDomain, feed, qk, rk,
		protocol.DefaultMaxPadding, nil, false, "", nil, false)
	if err := dnsSrv.SetMTProtoRelay(relay); err != nil {
		t.Fatal(err)
	}
	go dnsSrv.ListenAndServe(ctx)

	f, err := client.NewFetcher(feedDomain, passphrase, []string{dnsAddr})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(signPub)); err != nil {
		t.Fatal(err)
	}
	f.SetActiveResolvers([]string{dnsAddr})
	f.SetScatter(1)
	f.SetTimeout(5 * time.Second)
	f.SetNoiseDisabled(true)
	f.Start(ctx)

	var info *protocol.MTPInfo
	for i := 0; i < 40; i++ {
		dctx, c := context.WithTimeout(ctx, 3*time.Second)
		info, err = client.FetchMTProtoInfo(dctx, f)
		c()
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("tunnel discovery failed: %v", err)
	}

	sess, err := client.NewMTProtoSessionOn(f, info, cellBudget)
	if err != nil {
		t.Fatal(err)
	}
	hctx, hc := context.WithTimeout(ctx, 30*time.Second)
	err = sess.Handshake(hctx)
	hc()
	if err != nil {
		t.Fatalf("tunnel handshake: %v", err)
	}

	meter := &queryMeter{}
	link := newSlowLink(ctx, gap)
	// A real degraded link is latency AND loss, not just a rate: SLOWLINK_LAT
	// sets a per-query delay range and SLOWLINK_DROP a percentage that never
	// answers at all, which is what the client's own deadline must survive.
	latMin, latMax := envDuration("SLOWLINK_LAT_MIN"), envDuration("SLOWLINK_LAT_MAX")
	dropPct := envInt("SLOWLINK_DROP")
	exchange := func(ctx context.Context, plain []byte) ([]byte, error) {
		if gap > 0 {
			if err := link.wait(ctx); err != nil {
				return nil, err
			}
		}
		if dropPct > 0 && rand.Intn(100) < dropPct {
			// Swallowed: no answer ever comes. A real client gives up on its
			// own per-query timeout, so the wait is bounded by that and not by
			// the caller's context, which has no deadline of its own.
			select {
			case <-time.After(queryTimeoutForTest):
				return nil, errSimulatedDrop
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if latMax > 0 {
			d := latMin
			if latMax > latMin {
				d += time.Duration(rand.Int63n(int64(latMax - latMin)))
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		reply, err := sess.Exchange(ctx, plain)
		if err == nil {
			meter.record(plain, reply)
		}
		return reply, err
	}
	return &slowLinkRig{
		exchange: exchange, chunk: sess.Chunk(), window: sess.InFlight(), cluster: tg,
		session: filepath.Join(t.TempDir(), "session.json"), meter: meter,
	}
}

// How many DNS queries one Telegram login costs, and whether it completes at
// all when the link is slow. Set SLOWLINK_GAP_MS=1000 to simulate the one
// query per second a degraded network gets; SLOWLINK_CELL sets the per-cell
// budget.
//
// gotd's own defaults cannot survive that link: the key exchange alone
// measured 1m3s, past its 1-minute ExchangeTimeout, after which it re-dialled
// every few seconds for ever without finishing. The timeouts in client.go are
// what this guards.
func TestSlowLinkHandshakeQueryCost(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: drives a full MTProto key exchange")
	}
	gap := time.Duration(0)
	if v := os.Getenv("SLOWLINK_GAP_MS"); v != "" {
		n, _ := strconv.Atoi(v)
		gap = time.Duration(n) * time.Millisecond
	}
	budget := protocol.ChatCellPlainMax
	if v := os.Getenv("SLOWLINK_CELL"); v != "" {
		budget, _ = strconv.Atoi(v)
	}

	// A login is ~100 queries, so the budget has to scale with the link.
	budgetFor := 4 * time.Minute
	if gap > 0 {
		if need := 300 * gap; need > budgetFor {
			budgetFor = need
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), budgetFor)
	defer cancel()
	start := time.Now()

	rig := newSlowLinkRig(t, ctx, gap, budget)
	// Mirrors production's dialer: gotd re-dials on every reconnect, and the
	// previous tunnel has to go first or its pump keeps spending queries the
	// new one needs.
	var dials int32
	var dialMu sync.Mutex
	var prev *client.TunnelConn
	dial := func(ctx context.Context, dc int) (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		dialMu.Lock()
		old := prev
		prev = nil
		dialMu.Unlock()
		if old != nil {
			go old.Close()
		}
		// Why a dial failed is the whole question when the query count
		// stands still while the dial count climbs.
		deadline := "none"
		if d, ok := ctx.Deadline(); ok {
			deadline = time.Until(d).Round(time.Millisecond).String()
		}
		c, err := client.DialTunnel(ctx, rig.exchange, dc, client.TunnelOptions{
			Chunk:       rig.chunk,
			IdlePoll:    telIdlePollForTest,
			MaxIdlePoll: telMaxIdlePollForTest,
			// As production does: the window comes from the configured rate.
			Window: rig.window,
		})
		if err != nil || atomic.LoadInt32(&dials) <= 3 {
			t.Logf("dial #%d budget=%s err=%v", atomic.LoadInt32(&dials), deadline, err)
		}
		if err != nil {
			return nil, err
		}
		dialMu.Lock()
		prev = c
		dialMu.Unlock()
		return c, nil
	}
	var logged int32
	tgc, err := newClientForTestLogged(dial, rig.cluster, rig.session, func(msg string) {
		// gotd's own reasoning, capped so a loop cannot drown the output.
		if atomic.AddInt32(&logged, 1) <= 40 {
			t.Logf("gotd: %s", msg)
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- tgc.Run(ctx, func(ctx context.Context) error {
			_, err := tgc.API().HelpGetConfig(ctx)
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("login over the tunnel failed: %v", err)
		}
	case <-ctx.Done():
		d, p, o, up, _ := rig.meter.totals()
		t.Fatalf("login never completed: %d queries (%d data, %d poll, %d other), "+
			"%d B up, %d dials — a dial count climbing while the query count "+
			"stands still is gotd abandoning the key exchange and starting over",
			d+p+o, d, p, o, up, atomic.LoadInt32(&dials))
	}

	data, poll, other, up, down := rig.meter.totals()
	total := data + poll + other
	t.Logf("%d B/cell: %d queries (%d data, %d poll, %d other), %d B up / %d B down, "+
		"%s wall — at 1 query/s that is %s",
		rig.chunk, total, data, poll, other, up, down,
		time.Since(start).Round(time.Millisecond),
		(time.Duration(total) * time.Second).Round(time.Second))

	// One dial. More means the key exchange was abandoned and restarted, which
	// on a slow link never converges.
	if n := atomic.LoadInt32(&dials); n != 1 {
		t.Errorf("login took %d dials, want 1 — the key exchange is being restarted", n)
	}
}

// The timeouts gotd is given have to clear what the link actually costs: a
// key exchange measured 1m3s at one query a second, and a re-dial throws all
// of it away.
func TestGotdTimeoutsClearAMeasuredSlowLogin(t *testing.T) {
	const measuredKeyExchange = 63 * time.Second
	if exchangeTimeout <= measuredKeyExchange {
		t.Errorf("exchangeTimeout %s does not clear the measured %s key exchange",
			exchangeTimeout, measuredKeyExchange)
	}
	// gotd's own defaults, for comparison: 1m exchange, 35s dial, 5s retry.
	if exchangeTimeout <= time.Minute {
		t.Error("exchangeTimeout is no better than gotd's default")
	}
	if dialTimeout <= 35*time.Second {
		t.Error("dialTimeout is no better than gotd's default")
	}
	// A resend must not start while the request is still being transmitted:
	// at one query a second a 400-byte request is ~25 seconds of cells, and
	// each duplicate competes with the original for the same budget.
	const slowestRequest = 60 * time.Second
	if retryInterval < slowestRequest {
		t.Errorf("retryInterval %s re-sends a request still in flight on a slow link",
			retryInterval)
	}
}

// Deliberately matching production so the measurement means something.
const (
	telIdlePollForTest    = 700 * time.Millisecond
	telMaxIdlePollForTest = 5 * time.Second
)
