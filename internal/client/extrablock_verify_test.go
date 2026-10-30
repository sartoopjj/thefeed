package client

import (
	"context"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// buildSignedChannel serializes msgs the way the server does and returns the
// content blocks plus a signed ExtraBlock over their concatenation.
func buildSignedChannel(t *testing.T, priv ed25519.PrivateKey, ch uint16, msgs []protocol.Message) ([][]byte, []byte) {
	t.Helper()
	raw := protocol.CompressMessages(protocol.SerializeMessages(msgs))
	blocks := protocol.SplitIntoBlocks(raw)
	var concat []byte
	for _, b := range blocks {
		concat = append(concat, b...)
	}
	eb, err := protocol.EncodeExtraBlock(priv, ch, protocol.ContentDigest(concat), time.Now().Unix())
	if err != nil {
		t.Fatalf("encode extra block: %v", err)
	}
	return blocks, eb
}

// routeExchange decodes each query and serves the matching content block, the
// ExtraBlock at index == len(blocks), or an error beyond that.
func routeExchange(f *Fetcher, ch uint16, blocks [][]byte, extra []byte) func(context.Context, *dns.Msg, string) (*dns.Msg, time.Duration, error) {
	return func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		qname := m.Question[0].Name
		c, blk, err := protocol.DecodeQuery(f.queryKey, qname, f.domain)
		if err != nil {
			return nil, 0, err
		}
		if c != ch {
			return nil, 0, fmt.Errorf("unexpected channel %d", c)
		}
		var payload []byte
		switch {
		case int(blk) < len(blocks):
			payload = blocks[blk]
		case int(blk) == len(blocks):
			payload = extra
		default:
			return nil, 0, fmt.Errorf("no block %d", blk)
		}
		encoded, err := protocol.EncodeResponse(f.responseKey, payload, 0)
		if err != nil {
			return nil, 0, err
		}
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Rcode = dns.RcodeSuccess
		resp.Answer = []dns.RR{&dns.TXT{
			Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
			Txt: []string{encoded},
		}}
		return resp, time.Millisecond, nil
	}
}

func TestFetchChannelAcceptsValidSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(cryptoRand.Reader)
	const ch uint16 = 1
	blocks, extra := buildSignedChannel(t, priv, ch, []protocol.Message{{ID: 1, Timestamp: 1, Text: "hello"}})

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	f.exchangeFn = routeExchange(f, ch, blocks, extra)

	got, err := f.FetchChannel(context.Background(), int(ch), len(blocks))
	if err != nil {
		t.Fatalf("FetchChannel: %v", err)
	}
	if len(got) != 1 || got[0].Text != "hello" {
		t.Errorf("messages = %+v, want one 'hello'", got)
	}
}

func TestFetchChannelRejectsTamperedContent(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(cryptoRand.Reader)
	const ch uint16 = 1
	// Sign the genuine content, but serve different ("evil") content with the
	// still-valid signature — a malicious resolver swapping the payload.
	_, extra := buildSignedChannel(t, priv, ch, []protocol.Message{{ID: 1, Timestamp: 1, Text: "hello"}})
	tampered := protocol.SplitIntoBlocks(protocol.CompressMessages(protocol.SerializeMessages(
		[]protocol.Message{{ID: 1, Timestamp: 1, Text: "EVIL"}})))

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	f.exchangeFn = routeExchange(f, ch, tampered, extra)

	if _, err := f.FetchChannel(context.Background(), int(ch), len(tampered)); err == nil {
		t.Fatal("expected FetchChannel to reject tampered content")
	}
}

// A pinned key must not be bypassable by simply WITHHOLDING the signature.
//
// Verification tolerated an absent ExtraBlock as "an old server", and every
// caller only logged it. So a resolver -- which needs nothing but the
// passphrase, and the passphrase is public by design -- served forged content
// with no signature block and the client took it, pinned key and all. The pin
// was decorative.
//
// Availability is not the cost it looks like: FetchBlock retries twenty times
// with back-off, so reaching this point means twenty consecutive failures on
// a link that just delivered the content blocks.
func TestPinnedKeyRejectsAWithheldSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(cryptoRand.Reader)
	const ch uint16 = 1
	_, _ = buildSignedChannel(t, priv, ch, []protocol.Message{{ID: 1, Timestamp: 1, Text: "hello"}})

	forged := protocol.SplitIntoBlocks(protocol.CompressMessages(protocol.SerializeMessages(
		[]protocol.Message{{ID: 1, Timestamp: 1, Text: "FORGED by resolver"}})))

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	// Content blocks answer; the signature block never does.
	f.exchangeFn = routeExchange(f, ch, forged, nil)

	msgs, err := f.FetchChannel(context.Background(), int(ch), len(forged))
	if err == nil {
		t.Fatalf("forged content with no signature block was accepted: %+v", msgs)
	}
}

// buildMetadata returns extended-format metadata blocks and, when priv is
// given, a signed ExtraBlock over their concatenation.
func buildMetadata(t *testing.T, priv ed25519.PrivateKey, name string) ([][]byte, []byte) {
	t.Helper()
	blocks, err := protocol.EncodeMetadataExtended(&protocol.Metadata{
		Channels: []protocol.ChannelInfo{{Name: name, Blocks: 3, LastMsgID: 9}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if priv == nil {
		return blocks, nil
	}
	var concat []byte
	for _, b := range blocks {
		concat = append(concat, b...)
	}
	eb, err := protocol.EncodeExtraBlock(priv, protocol.MetadataChannel, protocol.ContentDigest(concat), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	return blocks, eb
}

// The channel list is metadata, and a forged one is as useful to an attacker
// as forged content: it decides which channels exist and how many blocks each
// has. Withholding the signature block must not get it accepted either.
func TestPinnedKeyRejectsMetadataWithNoSignature(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(cryptoRand.Reader)
	blocks, _ := buildMetadata(t, nil, "news")

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	f.exchangeFn = routeExchange(f, protocol.MetadataChannel, blocks, nil)

	if meta, err := f.FetchMetadata(context.Background()); err == nil {
		t.Fatalf("unsigned metadata was accepted: %+v", meta)
	}
}

// The format is chosen by a magic number in block 0 -- which the answering
// resolver writes. The legacy path fetches no signature and verifies nothing,
// so clearing two bytes would be a downgrade to an unverified feed.
func TestPinnedKeyRefusesTheLegacyMetadataDowngrade(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(cryptoRand.Reader)
	blocks, extra := buildMetadata(t, priv, "news")

	// Everything is genuinely signed; the resolver changes only the magic, so
	// the client takes the path that never looks at the signature.
	stripped := make([][]byte, len(blocks))
	for i, b := range blocks {
		stripped[i] = append([]byte(nil), b...)
	}
	stripped[0][0], stripped[0][1] = 0, 0

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	f.exchangeFn = routeExchange(f, protocol.MetadataChannel, stripped, extra)

	if meta, err := f.FetchMetadata(context.Background()); err == nil {
		t.Fatalf("a two-byte edit downgraded a pinned config to an unverified feed: %+v", meta)
	}
}

// And with the signature present the same fetch must succeed, or the three
// tests above would be passing because metadata is broken outright.
func TestPinnedMetadataWithItsSignatureIsAccepted(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(cryptoRand.Reader)
	blocks, extra := buildMetadata(t, priv, "news")

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	f.exchangeFn = routeExchange(f, protocol.MetadataChannel, blocks, extra)

	meta, err := f.FetchMetadata(context.Background())
	if err != nil {
		t.Fatalf("signed metadata was refused: %v", err)
	}
	if len(meta.Channels) != 1 || meta.Channels[0].Name != "news" {
		t.Errorf("channels = %+v, want one named \"news\"", meta.Channels)
	}
}

// Without a pinned key nothing changes: an old config still reads an old
// server. The guard must key off the pin, not off the signature.
func TestUnpinnedConfigStillReadsAnUnsignedServer(t *testing.T) {
	const ch uint16 = 1
	blocks := protocol.SplitIntoBlocks(protocol.CompressMessages(protocol.SerializeMessages(
		[]protocol.Message{{ID: 1, Timestamp: 1, Text: "hello"}})))

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	f.exchangeFn = routeExchange(f, ch, blocks, nil)

	got, err := f.FetchChannel(context.Background(), int(ch), len(blocks))
	if err != nil {
		t.Fatalf("an unpinned config stopped working: %v", err)
	}
	if len(got) != 1 || got[0].Text != "hello" {
		t.Errorf("messages = %+v, want one 'hello'", got)
	}
}
