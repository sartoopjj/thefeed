package client

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
)

// echoDC stands in for a Telegram data centre: it accepts one TCP connection
// and echoes the byte stream back. The tunnel is transport-agnostic, so an
// echo is enough to prove the whole path carries a stream intact.
type echoDC struct {
	ln net.Listener
}

func newEchoDC(t *testing.T) *echoDC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &echoDC{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return d
}

func (d *echoDC) addr() string { return d.ln.Addr().String() }

// End-to-end: a real Fetcher driving real sealed cells over a real loopback
// DNS server, into the relay, out to a stand-in data centre, and back. This is
// the piece neither the protocol tests nor the relay tests cover -- they each
// stub the other side.
func TestMTProtoTunnelEndToEndOverDNS(t *testing.T) {
	const passphrase = "mtp-e2e-pass"
	const feedDomain = "t.example.com"
	const mtpDomain = "m.example.com"
	const dcID = 2

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

	dc := newEchoDC(t)
	relay := server.NewMTProtoRelay(server.MTProtoConfig{
		Enabled:     true,
		Domains:     []string{mtpDomain},
		Ek:          ek,
		QueryKey:    qk,
		IdleTTL:     time.Minute,
		DialTimeout: 5 * time.Second,
		DCAddrs:     map[uint8]string{dcID: dc.addr()},
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
	addr := pc.LocalAddr().String()
	pc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dnsSrv := server.NewDNSServer(addr, feedDomain, feed, qk, rk, protocol.DefaultMaxPadding, nil, false, "", nil, false)
	if err := dnsSrv.SetMTProtoRelay(relay); err != nil {
		t.Fatal(err)
	}
	go dnsSrv.ListenAndServe(ctx)

	f, err := NewFetcher(feedDomain, passphrase, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(signPub)); err != nil {
		t.Fatal(err)
	}
	f.SetActiveResolvers([]string{addr})
	f.SetScatter(1)
	f.SetTimeout(3 * time.Second)
	f.SetNoiseDisabled(true)
	f.Start(ctx)

	// Discover the tunnel the way a real client does: fetch the signed
	// capability payload over DNS and verify it against the pinned key.
	var info *protocol.MTPInfo
	for i := 0; i < 40; i++ {
		dctx, c := context.WithTimeout(ctx, 3*time.Second)
		info, err = FetchMTProtoInfo(dctx, f)
		c()
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("tunnel discovery over DNS failed: %v", err)
	}
	if !info.Enabled || len(info.Domains) == 0 {
		t.Fatalf("discovered an unusable tunnel: %+v", info)
	}
	if string(info.EkPub) != string(ek.PublicKey().Bytes()) {
		t.Fatal("discovered handshake key does not match the server's")
	}
	if info.Domains[0] != mtpDomain {
		t.Fatalf("discovered domain %q, want %q", info.Domains[0], mtpDomain)
	}

	sess, err := NewMTProtoSessionOn(f, info, protocol.ChatCellPlainSize)
	if err != nil {
		t.Fatal(err)
	}

	// Handshake on the discovered domain.
	var hsErr error
	for i := 0; i < 40; i++ {
		hctx, c := context.WithTimeout(ctx, 3*time.Second)
		hsErr = sess.Handshake(hctx)
		c()
		if hsErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if hsErr != nil {
		t.Fatalf("handshake over DNS failed: %v", hsErr)
	}
	if !sess.Live() {
		t.Fatal("session not live after handshake")
	}

	conn, err := DialTunnel(ctx, sess.Exchange, dcID, TunnelOptions{
		Chunk:       sess.Chunk(),
		IdlePoll:    50 * time.Millisecond,
		MaxIdlePoll: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()

	// A message longer than one cell, so fragmentation and the ARQ both run.
	// Carried in the framing the tunnel is built for: message boundaries are
	// what tell the relay when to deliver.
	body := make([]byte, 4*(3*sess.Chunk()/4+2))
	if _, err := cryptoRand.Read(body); err != nil {
		t.Fatal(err)
	}
	payload := abridgedFrame(body)
	conn.SetDeadline(time.Now().Add(90 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back through the tunnel: %v", err)
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("stream corrupted at byte %d of %d", i, len(payload))
		}
	}

	if active, _ := relay.Stats(); active != 1 {
		t.Fatalf("relay reports %d active tunnels, want 1", active)
	}
}
