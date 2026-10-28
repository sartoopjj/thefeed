package client

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
)

// infoFixture serves a feed over loopback DNS so discovery can be exercised
// against a real server. sign=false strips the signing key, which is the
// signature-strip attack: the payload still arrives, just unverifiable.
func infoFixture(t *testing.T, sign bool, publish func(*server.Feed, *ecdh.PrivateKey)) (*Fetcher, string) {
	t.Helper()
	const passphrase = "mtp-info-pass"
	const feedDomain = "t.example.com"

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

	feed := server.NewFeed(nil)
	if sign {
		feed.SetSigningKey(signKey)
	}
	publish(feed, ek)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dnsSrv := server.NewDNSServer(addr, feedDomain, feed, qk, rk, protocol.DefaultMaxPadding, nil, false, "", nil, false)
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
	f.SetTimeout(2 * time.Second)
	f.SetNoiseDisabled(true)
	f.Start(ctx)
	return f, addr
}

func fetchInfoEventually(t *testing.T, f *Fetcher) (*protocol.MTPInfo, error) {
	t.Helper()
	var info *protocol.MTPInfo
	var err error
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err = FetchMTProtoInfo(ctx, f)
		cancel()
		// Stop as soon as the server is clearly answering, success or verdict.
		if err == nil || errors.Is(err, ErrMTProtoServerDisabled) || errors.Is(err, ErrMTProtoUnverified) {
			return info, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return info, err
}

func enabledInfo(ek *ecdh.PrivateKey) protocol.MTPInfo {
	return protocol.MTPInfo{
		MinVersion: protocol.MTProtoVersion,
		MaxVersion: protocol.MTProtoVersion,
		Enabled:    true,
		Domains:    []string{"m.example.com"},
		EkPub:      ek.PublicKey().Bytes(),
		Chunk:      uint8(protocol.MTPChunkSize(protocol.ChatCellPlainMax)),
	}
}

// The payload carries the server's handshake key, so an unsigned copy must
// never be used: anyone able to answer DNS could otherwise substitute their
// own key and sit in the middle of the tunnel.
func TestFetchMTProtoInfoRejectsUnsignedPayload(t *testing.T) {
	f, _ := infoFixture(t, false, func(feed *server.Feed, ek *ecdh.PrivateKey) {
		feed.SetMTProtoAvailable(true)
		feed.SetMTProtoInfoPayload(protocol.EncodeMTPInfo(enabledInfo(ek)))
	})
	_, err := fetchInfoEventually(t, f)
	if !errors.Is(err, ErrMTProtoUnverified) {
		t.Fatalf("err = %v, want ErrMTProtoUnverified", err)
	}
}

// A config with no pinned key cannot verify anything, so discovery must not
// even go to the network.
func TestFetchMTProtoInfoRequiresPinnedKey(t *testing.T) {
	f, err := NewFetcher("t.example.com", "pass", []string{"127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchMTProtoInfo(context.Background(), f); !errors.Is(err, ErrMTProtoNoServerKey) {
		t.Fatalf("err = %v, want ErrMTProtoNoServerKey", err)
	}
}

// "Configured but switched off" is a distinct, useful answer: the UI says so
// instead of retrying as if the server were unreachable.
func TestFetchMTProtoInfoReportsServerDisabled(t *testing.T) {
	f, _ := infoFixture(t, true, func(feed *server.Feed, _ *ecdh.PrivateKey) {
		feed.SetMTProtoAvailable(true)
		feed.SetMTProtoInfoPayload(protocol.EncodeMTPInfo(protocol.MTPInfo{
			MinVersion: protocol.MTProtoVersion,
			MaxVersion: protocol.MTProtoVersion,
			Enabled:    false,
			Domains:    []string{"m.example.com"},
		}))
	})
	if _, err := fetchInfoEventually(t, f); !errors.Is(err, ErrMTProtoServerDisabled) {
		t.Fatalf("err = %v, want ErrMTProtoServerDisabled", err)
	}
}

// A server with no tunnel serves nothing on the channel. One call only: the
// verdict is stable, and retrying just burns the fetch budget (looping here
// cost a minute of suite time).
func TestFetchMTProtoInfoNotOffered(t *testing.T) {
	f, _ := infoFixture(t, true, func(feed *server.Feed, _ *ecdh.PrivateKey) {})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := FetchMTProtoInfo(ctx, f); !errors.Is(err, ErrMTProtoNotOffered) {
		t.Fatalf("err = %v, want ErrMTProtoNotOffered", err)
	}
}

// And the happy path returns exactly what the server published.
func TestFetchMTProtoInfoSignedRoundTrip(t *testing.T) {
	var want protocol.MTPInfo
	f, _ := infoFixture(t, true, func(feed *server.Feed, ek *ecdh.PrivateKey) {
		want = enabledInfo(ek)
		feed.SetMTProtoAvailable(true)
		feed.SetMTProtoInfoPayload(protocol.EncodeMTPInfo(want))
	})
	got, err := fetchInfoEventually(t, f)
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if !got.Enabled || got.Chunk != want.Chunk || len(got.Domains) != 1 || got.Domains[0] != want.Domains[0] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if string(got.EkPub) != string(want.EkPub) {
		t.Fatal("handshake key did not round-trip")
	}
}
