package client

import (
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// bomb returns a deflate stream that expands to mb megabytes of zeros.
func bomb(t *testing.T, mb int, header byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if header != 0 {
		buf.WriteByte(header)
	}
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	zeros := make([]byte, 1<<20)
	for i := 0; i < mb; i++ {
		if _, err := w.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	return buf.Bytes()
}

// The FETCHER has to apply the cap, not just own a function that could.
//
// The bytes come from whichever resolver answered, and the feed passphrase is
// public by design, so this is any resolver. The protocol-level test covers
// DecompressMessagesLimited; this covers the call site actually passing a
// bound, which is the part that can be lost in an edit.
func TestFetchChannelRefusesAnInflatedChannel(t *testing.T) {
	payload := bomb(t, 48, 0) // compression header is added by the server side
	// Channel data on the wire is CompressMessages output: a 1-byte header
	// then the deflate stream. Build that shape directly.
	var wire bytes.Buffer
	wire.WriteByte(1) // compressionDeflate
	wire.Write(payload[0:])
	blocks := protocol.SplitIntoBlocks(wire.Bytes())

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	const ch uint16 = 1
	f.exchangeFn = func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		qname := m.Question[0].Name
		c, blk, err := protocol.DecodeQuery(f.queryKey, qname, f.domain)
		if err != nil {
			return nil, 0, err
		}
		if c != ch || int(blk) >= len(blocks) {
			return nil, 0, fmt.Errorf("no block %d/%d", c, blk)
		}
		encoded, err := protocol.EncodeResponse(f.responseKey, blocks[blk], 0)
		if err != nil {
			return nil, 0, err
		}
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Answer = []dns.RR{&dns.TXT{
			Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeTXT, Class: dns.ClassINET},
			Txt: splitTXTString(encoded),
		}}
		return resp, time.Millisecond, nil
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	msgs, err := f.FetchChannel(context.Background(), int(ch), len(blocks))
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatalf("%d bytes of blocks expanded unchallenged into %d messages",
			wire.Len(), len(msgs))
	}
	// Refusing it must also be cheap, or it is the same denial by another route.
	if mb := (after.TotalAlloc - before.TotalAlloc) / (1 << 20); mb > 256 {
		t.Errorf("allocated %d MiB to refuse it", mb)
	}
}

// And a real channel of ordinary size still decodes, or the test above would
// be passing because channel fetches are broken outright.
func TestFetchChannelStillReadsANormalChannel(t *testing.T) {
	msgs := []protocol.Message{{ID: 1, Timestamp: 1, Text: "hello"}}
	blocks := protocol.SplitIntoBlocks(protocol.CompressMessages(protocol.SerializeMessages(msgs)))

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	const ch uint16 = 1
	f.exchangeFn = routeExchange(f, ch, blocks, nil)

	got, err := f.FetchChannel(context.Background(), int(ch), len(blocks))
	if err != nil {
		t.Fatalf("a normal channel was refused: %v", err)
	}
	if len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("messages = %+v", got)
	}
}

// The whole-body media path (the non-streaming one) has its own cap.
func TestMediaBytesRefuseAnInflatedBody(t *testing.T) {
	b := bomb(t, 48, 0)
	out, err := decompressMediaBytes(b, protocol.MediaCompressionDeflate)
	if err == nil {
		t.Fatalf("%d bytes on the wire expanded to %d unchallenged", len(b), len(out))
	}

	// A real file of ordinary size still comes through whole.
	payload := make([]byte, 200000)
	for i := range payload {
		payload[i] = byte(i*31 + i/251)
	}
	var comp bytes.Buffer
	w, _ := flate.NewWriter(&comp, flate.BestSpeed)
	w.Write(payload)
	w.Close()
	got, err := decompressMediaBytes(comp.Bytes(), protocol.MediaCompressionDeflate)
	if err != nil {
		t.Fatalf("a real file was refused: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %d bytes, want %d", len(got), len(payload))
	}
}
