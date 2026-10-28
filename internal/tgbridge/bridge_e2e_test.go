package tgbridge

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tgtest/cluster"
	"github.com/gotd/td/transport"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
)

// The whole stack, for real: a gotd Telegram client performs the MTProto key
// exchange and an RPC against a stand-in Telegram, with every byte travelling
// as DNS queries through thefeed's relay. Nothing here is stubbed except
// Telegram itself.
func TestTelegramRPCOverDNSTunnel(t *testing.T) {
	const passphrase = "tgbridge-e2e"
	const feedDomain = "t.example.com"
	const mtpDomain = "m.example.com"
	const dcID = 2

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// --- stand-in Telegram -------------------------------------------------
	// Abridged, to match the resolver: the relay only moves bytes, so the
	// transport codec is agreed end-to-end between client and Telegram.
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
			break
		}
	}
	if dcAddr == "" {
		t.Fatal("stand-in Telegram published no address for the DC")
	}

	// --- thefeed server with the relay ------------------------------------
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
		Enabled:     true,
		Domains:     []string{mtpDomain},
		Ek:          ek,
		QueryKey:    qk,
		IdleTTL:     2 * time.Minute,
		DialTimeout: 10 * time.Second,
		BufferBytes: 256 << 10,
		DCAddrs:     map[uint8]string{dcID: dcAddr},
	})
	if relay == nil {
		t.Fatal("relay not created")
	}

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

	// --- thefeed client ----------------------------------------------------
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

	// One session carries the tunnel; the dialer hands gotd a net.Conn on it.
	sess, err := client.NewMTProtoSessionOn(f, info, protocol.ChatCellPlainMax)
	if err != nil {
		t.Fatal(err)
	}
	hctx, hc := context.WithTimeout(ctx, 20*time.Second)
	err = sess.Handshake(hctx)
	hc()
	if err != nil {
		t.Fatalf("tunnel handshake: %v", err)
	}

	dial := func(ctx context.Context, dc int) (net.Conn, error) {
		return client.DialTunnel(ctx, sess.Exchange, dc, client.TunnelOptions{
			Chunk:       sess.Chunk(),
			IdlePoll:    20 * time.Millisecond,
			MaxIdlePoll: 200 * time.Millisecond,
		})
	}

	// Same resolver and session storage as production; only the DC list and
	// server keys point at the stand-in instead of Telegram.
	tgc, err := newClientForTest(dial, tg, filepath.Join(t.TempDir(), "session.json"))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- tgc.Run(ctx, func(ctx context.Context) error {
			// help.getConfig is answered by the cluster's config service and
			// requires a completed MTProto key exchange, so reaching it proves
			// the whole path works.
			cfg, err := tgc.API().HelpGetConfig(ctx)
			if err != nil {
				return err
			}
			if cfg == nil {
				t.Error("nil config")
			}
			// Check while the connection is still open: gotd closes the
			// tunnel as soon as Run returns.
			if active, _ := relay.Stats(); active != 1 {
				t.Errorf("relay reports %d active tunnels during the call, want 1", active)
			}
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("MTProto RPC over the DNS tunnel failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out running MTProto over the DNS tunnel")
	}

	// And nothing is left behind.
	//
	// The session deliberately OUTLIVES the connection -- closing ends the
	// tunnel so the next one costs a single open instead of a fresh handshake
	// -- so the thing to check is that the sweeper still reclaims it. A
	// session nobody reclaims would hold its buffers for ever.
	if n := relay.Sweep(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatalf("swept %d sessions, want the finished one reclaimed", n)
	}
	if active, _ := relay.Stats(); active != 0 {
		t.Fatalf("%d sessions left after the sweep", active)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// newClientForTest wires gotd to the stand-in cluster: same resolver as
// production, but the cluster's own DC list and server keys.
func newClientForTest(dial Dialer, tg *cluster.Cluster, sessionPath string) (*telegram.Client, error) {
	return newClientForTestLogged(dial, tg, sessionPath, nil)
}

// newClientForTestLogged is newClientForTest with the production timeouts and
// gotd's own log, which is the only place a slow-link stall explains itself.
func newClientForTestLogged(dial Dialer, tg *cluster.Cluster, sessionPath string, logf func(string)) (*telegram.Client, error) {
	resolver, err := NewResolver(dial)
	if err != nil {
		return nil, err
	}
	opts := telegram.Options{
		Resolver:         resolver,
		DC:               2,
		DCList:           tg.List(),
		PublicKeys:       tg.Keys(),
		SessionStorage:   SessionFile{Path: sessionPath},
		NoUpdates:        true,
		MigrationTimeout: migrationTimeout,
		ExchangeTimeout:  exchangeTimeout,
		DialTimeout:      dialTimeout,
		RetryInterval:    retryInterval,
		MaxRetries:       maxRetries,
	}
	if logf != nil {
		opts.Logger = newLogger(logf)
	}
	return telegram.NewClient(1, "hash", opts), nil
}
