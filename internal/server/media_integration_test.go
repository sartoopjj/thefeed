package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// TestApplyHTTPMediaSourcesEndToEnd wires a fake upstream HTTP image server,
// runs applyHTTPMediaSources against it, and verifies the message body now
// carries downloadable metadata that ParseMediaText can read back. Then it
// fetches a block out of the resulting MediaCache to confirm the bytes were
// stored correctly.
func TestApplyHTTPMediaSourcesEndToEnd(t *testing.T) {
	imageBytes := []byte("fake-image-bytes-payload-1234567890")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		w.Write(imageBytes)
	}))
	defer srv.Close()

	cache := NewMediaCache(MediaCacheConfig{MaxFileBytes: 1 << 20, TTL: time.Hour, DNSRelayEnabled: true})

	msgs := []protocol.Message{
		{ID: 100, Timestamp: 1, Text: protocol.MediaImage + "\nhello"},
	}
	sources := []mediaSource{{tag: protocol.MediaImage, url: srv.URL + "/photo.png"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applyHTTPMediaSources(ctx, cache, msgs, sources)

	meta, caption, ok := protocol.ParseMediaText(msgs[0].Text)
	if !ok {
		t.Fatalf("ParseMediaText ok=false on rewritten message: %q", msgs[0].Text)
	}
	if !meta.HasRelay(protocol.RelayDNS) {
		t.Fatalf("expected downloadable meta, got %+v (text=%q)", meta, msgs[0].Text)
	}
	if meta.Tag != protocol.MediaImage {
		t.Fatalf("Tag = %q, want %q", meta.Tag, protocol.MediaImage)
	}
	if meta.Size != int64(len(imageBytes)) {
		t.Fatalf("Size = %d, want %d", meta.Size, len(imageBytes))
	}
	if caption != "hello" {
		t.Fatalf("caption = %q, want %q", caption, "hello")
	}

	// Block 0 starts with the 4-byte CRC32 prefix; subsequent blocks are
	// raw content.
	var got []byte
	for blk := uint16(0); blk < meta.Blocks; blk++ {
		b, err := cache.GetBlock(meta.Channel, blk)
		if err != nil {
			t.Fatalf("GetBlock(%d, %d): %v", meta.Channel, blk, err)
		}
		got = append(got, b...)
	}
	if len(got) < protocol.MediaBlockHeaderLen {
		t.Fatalf("block 0 too short: %d", len(got))
	}
	hdr, err := protocol.DecodeMediaBlockHeader(got[:protocol.MediaBlockHeaderLen])
	if err != nil {
		t.Fatalf("DecodeMediaBlockHeader: %v", err)
	}
	if hdr.CRC32 != meta.CRC32 {
		t.Fatalf("header CRC = %x, want %x", hdr.CRC32, meta.CRC32)
	}
	if string(got[protocol.MediaBlockHeaderLen:]) != string(imageBytes) {
		t.Fatalf("reassembled bytes differ:\n  got:  %q\n  want: %q", got[protocol.MediaBlockHeaderLen:], imageBytes)
	}
}

// TestApplyHTTPMediaSourcesGzipRoundTrip: with --dns-media-compression=gzip,
// a successful upstream fetch lands compressed blocks in the cache. A
// client decompressing the assembled blocks recovers the original bytes
// verbatim and the embedded CRC32 matches.
func TestApplyHTTPMediaSourcesGzipRoundTrip(t *testing.T) {
	imageBytes := bytes.Repeat([]byte("compressible-stripe "), 300)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		w.Write(imageBytes)
	}))
	defer srv.Close()

	cache := NewMediaCache(MediaCacheConfig{
		MaxFileBytes:    1 << 20,
		TTL:             time.Hour,
		Compression:     protocol.MediaCompressionGzip,
		DNSRelayEnabled: true,
	})
	msgs := []protocol.Message{{ID: 100, Timestamp: 1, Text: protocol.MediaImage + "\n"}}
	sources := []mediaSource{{tag: protocol.MediaImage, url: srv.URL + "/big.png"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applyHTTPMediaSources(ctx, cache, msgs, sources)

	meta, _, ok := protocol.ParseMediaText(msgs[0].Text)
	if !ok || !meta.HasRelay(protocol.RelayDNS) {
		t.Fatalf("expected downloadable meta, got %+v", meta)
	}

	var got []byte
	for blk := uint16(0); blk < meta.Blocks; blk++ {
		b, err := cache.GetBlock(meta.Channel, blk)
		if err != nil {
			t.Fatalf("GetBlock: %v", err)
		}
		got = append(got, b...)
	}
	hdr, err := protocol.DecodeMediaBlockHeader(got[:protocol.MediaBlockHeaderLen])
	if err != nil {
		t.Fatalf("DecodeMediaBlockHeader: %v", err)
	}
	if hdr.Compression != protocol.MediaCompressionGzip {
		t.Fatalf("compression = %v, want gzip", hdr.Compression)
	}
	if hdr.CRC32 != meta.CRC32 {
		t.Fatalf("header CRC = %x, want %x", hdr.CRC32, meta.CRC32)
	}
	rc, err := DecompressMediaBytes(bytes.NewReader(got[protocol.MediaBlockHeaderLen:]), hdr.Compression)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if !bytes.Equal(out, imageBytes) {
		t.Fatalf("decompressed differs from upstream")
	}
}

// TestApplyHTTPMediaSourcesAlbum: when src.extraURLs is populated (public-mode
// album), every URL is fetched and the canonical body is rebuilt with N
// stacked downloadable headers + the original caption. The frontend then
// renders an N-card album.
func TestApplyHTTPMediaSourcesAlbum(t *testing.T) {
	images := [][]byte{
		[]byte("first-image-bytes-XXXXXX"),
		[]byte("second-image-bytes-YYYYY"),
		[]byte("third-image-bytes-ZZZZZZ"),
	}
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		// Path looks like /img-N.jpg → pick the matching slice.
		switch r.URL.Path {
		case "/img1.jpg":
			w.Write(images[0])
		case "/img2.jpg":
			w.Write(images[1])
		case "/img3.jpg":
			w.Write(images[2])
		}
		served++
	}))
	defer srv.Close()

	cache := NewMediaCache(MediaCacheConfig{MaxFileBytes: 1 << 20, TTL: time.Hour, DNSRelayEnabled: true})

	// Mirror what parsePublicMessagesWithMedia produces for a 3-image album:
	// stacked [IMAGE] headers + caption, plus an extraURLs slice on the source.
	body := protocol.MediaImage + "\n" + protocol.MediaImage + "\n" + protocol.MediaImage + "\nalbum caption"
	msgs := []protocol.Message{{ID: 5, Timestamp: 1, Text: body}}
	sources := []mediaSource{{
		tag:       protocol.MediaImage,
		url:       srv.URL + "/img1.jpg",
		extraURLs: []string{srv.URL + "/img2.jpg", srv.URL + "/img3.jpg"},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applyHTTPMediaSources(ctx, cache, msgs, sources)

	if served != 3 {
		t.Errorf("served = %d, want 3 upstream fetches", served)
	}

	// Rewritten body must have exactly 3 [IMAGE]<size>:1:... headers and
	// the original caption preserved on the trailing line.
	got := msgs[0].Text
	headerCount := strings.Count(got, protocol.MediaImage)
	if headerCount != 3 {
		t.Fatalf("header count = %d, want 3 (text=%q)", headerCount, got)
	}
	if !strings.HasSuffix(got, "\nalbum caption") {
		t.Errorf("caption not preserved: %q", got)
	}

	// Each header must round-trip through ParseMediaText with downloadable=true.
	rest := got
	for i := 0; i < 3; i++ {
		meta, c, ok := protocol.ParseMediaText(rest)
		if !ok {
			t.Fatalf("ParseMediaText #%d ok=false on %q", i, rest)
		}
		if !meta.HasRelay(protocol.RelayDNS) {
			t.Errorf("header #%d not downloadable: %+v", i, meta)
		}
		if int(meta.Size) != len(images[i]) {
			t.Errorf("header #%d size = %d, want %d", i, meta.Size, len(images[i]))
		}
		rest = c
	}
	if rest != "album caption" {
		t.Errorf("trailing caption = %q, want %q", rest, "album caption")
	}
}

// TestApplyHTTPMediaSourcesAlbumPartialFailure: when one upstream fetch
// fails we still emit a placeholder [TAG] for that slot so the album's
// ID-span (= number of leading headers) is preserved. The remaining items
// stay downloadable.
func TestApplyHTTPMediaSourcesAlbumPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken.jpg" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok-image"))
	}))
	defer srv.Close()

	cache := NewMediaCache(MediaCacheConfig{MaxFileBytes: 1 << 20, TTL: time.Hour, DNSRelayEnabled: true})

	body := protocol.MediaImage + "\n" + protocol.MediaImage + "\ncap"
	msgs := []protocol.Message{{ID: 5, Timestamp: 1, Text: body}}
	sources := []mediaSource{{
		tag:       protocol.MediaImage,
		url:       srv.URL + "/ok.jpg",
		extraURLs: []string{srv.URL + "/broken.jpg"},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applyHTTPMediaSources(ctx, cache, msgs, sources)

	got := msgs[0].Text
	if c := strings.Count(got, protocol.MediaImage); c != 2 {
		t.Errorf("header count = %d, want 2 (text=%q)", c, got)
	}
	// First should be downloadable; last line is the broken-fallback bare tag
	// followed by the caption.
	if !strings.HasSuffix(got, "\n"+protocol.MediaImage+"\ncap") {
		t.Errorf("expected placeholder + caption tail, got %q", got)
	}
}

// TestApplyHTTPMediaSourcesRejectsOversize: a too-large file leaves the
// message text untouched but still records the entry as "metadata only" with
// downloadable=false so the UI can show the size without offering the button.
func TestApplyHTTPMediaSourcesRejectsOversize(t *testing.T) {
	bigBody := strings.Repeat("X", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(bigBody))
	}))
	defer srv.Close()

	cache := NewMediaCache(MediaCacheConfig{MaxFileBytes: 100, TTL: time.Hour, DNSRelayEnabled: true})
	msgs := []protocol.Message{{ID: 1, Timestamp: 1, Text: protocol.MediaImage + "\ncap"}}
	sources := []mediaSource{{tag: protocol.MediaImage, url: srv.URL + "/big.jpg"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applyHTTPMediaSources(ctx, cache, msgs, sources)

	meta, _, ok := protocol.ParseMediaText(msgs[0].Text)
	if !ok {
		t.Fatalf("ParseMediaText ok=false")
	}
	if meta.HasRelay(protocol.RelayDNS) {
		t.Fatalf("oversized file should not be downloadable; got meta=%+v", meta)
	}
	if meta.Size != int64(len(bigBody)) {
		t.Fatalf("Size = %d, want %d (server should still surface the size)", meta.Size, len(bigBody))
	}
	stats := cache.Stats()
	if stats.Entries != 0 {
		t.Fatalf("oversized file should not occupy a cache slot, got entries=%d", stats.Entries)
	}
}
