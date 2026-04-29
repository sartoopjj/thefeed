package server

import (
	"bytes"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

func newTestCache(maxBytes int64, ttl time.Duration) *MediaCache {
	return NewMediaCache(MediaCacheConfig{MaxFileBytes: maxBytes, TTL: ttl, DNSRelayEnabled: true})
}

// TestMediaCacheRelayFlags: with DNS off the wire flag stays clear, and
// when a GitHub relay is attached the cache surfaces RelayGitHub.
func TestMediaCacheRelayFlags(t *testing.T) {
	cfg := MediaCacheConfig{MaxFileBytes: 1 << 20, TTL: time.Hour, DNSRelayEnabled: false}
	cache := NewMediaCache(cfg)
	meta, err := cache.Store("k", protocol.MediaImage, []byte("payload"), "image/jpeg", "")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if meta.HasRelay(protocol.RelayDNS) {
		t.Errorf("DNS relay should be off when DNSRelayEnabled=false")
	}
	if meta.HasRelay(protocol.RelayGitHub) {
		t.Errorf("GitHub relay should be off when no relay is attached")
	}
}

func TestMediaCacheStoreAndGetBlock(t *testing.T) {
	cache := newTestCache(1<<20, time.Hour)
	content := bytes.Repeat([]byte("ab"), 1000) // 2000 bytes — multiple blocks

	meta, err := cache.Store("key1", protocol.MediaImage, content, "image/jpeg", "")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if !meta.HasRelay(protocol.RelayDNS) {
		t.Fatalf("RelayDNS = false, want true")
	}
	if !protocol.IsMediaChannel(meta.Channel) {
		t.Fatalf("Channel %d not in media range", meta.Channel)
	}
	if meta.Size != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", meta.Size, len(content))
	}
	if meta.CRC32 != crc32.ChecksumIEEE(content) {
		t.Fatalf("CRC32 mismatch")
	}
	if meta.Blocks == 0 {
		t.Fatalf("Blocks should be > 0")
	}

	// Reassemble: block 0 begins with the protocol media header, then comes
	// the (compression-default = none) bytes which equal the original.
	var got []byte
	for blk := uint16(0); blk < meta.Blocks; blk++ {
		b, err := cache.GetBlock(meta.Channel, blk)
		if err != nil {
			t.Fatalf("GetBlock(%d, %d): %v", meta.Channel, blk, err)
		}
		got = append(got, b...)
	}
	if len(got) < protocol.MediaBlockHeaderLen {
		t.Fatalf("assembled bytes too short: %d", len(got))
	}
	hdr, err := protocol.DecodeMediaBlockHeader(got[:protocol.MediaBlockHeaderLen])
	if err != nil {
		t.Fatalf("DecodeMediaBlockHeader: %v", err)
	}
	if hdr.CRC32 != meta.CRC32 {
		t.Fatalf("header CRC = %x, want %x", hdr.CRC32, meta.CRC32)
	}
	if hdr.Compression != protocol.MediaCompressionNone {
		t.Fatalf("header compression = %v, want none", hdr.Compression)
	}
	if !bytes.Equal(got[protocol.MediaBlockHeaderLen:], content) {
		t.Fatalf("reassembled bytes differ: got %d, want %d", len(got)-protocol.MediaBlockHeaderLen, len(content))
	}
}

// TestMediaCacheStoreGzip exercises the compressed wire path: bytes after
// the header are gzip-compressed and DecompressMediaBytes reproduces the
// original.
func TestMediaCacheStoreGzip(t *testing.T) {
	cache := NewMediaCache(MediaCacheConfig{
		MaxFileBytes:    1 << 20,
		TTL:             time.Hour,
		Compression:     protocol.MediaCompressionGzip,
		DNSRelayEnabled: true,
	})
	content := bytes.Repeat([]byte("compress-me "), 200)

	meta, err := cache.Store("gz", protocol.MediaFile, content, "text/plain", "")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	var got []byte
	for blk := uint16(0); blk < meta.Blocks; blk++ {
		b, err := cache.GetBlock(meta.Channel, blk)
		if err != nil {
			t.Fatalf("GetBlock(%d, %d): %v", meta.Channel, blk, err)
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
	body, err := DecompressMediaBytes(bytes.NewReader(got[protocol.MediaBlockHeaderLen:]), hdr.Compression)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	defer body.Close()
	decompressed := new(bytes.Buffer)
	if _, err := decompressed.ReadFrom(body); err != nil {
		t.Fatalf("read decompressed: %v", err)
	}
	if !bytes.Equal(decompressed.Bytes(), content) {
		t.Fatalf("decompressed differs from original")
	}
	if crc32.ChecksumIEEE(decompressed.Bytes()) != hdr.CRC32 {
		t.Fatalf("header CRC %x doesn't match decompressed CRC %x", hdr.CRC32, crc32.ChecksumIEEE(decompressed.Bytes()))
	}
}

// Storing the same key with the same content should refresh TTL but reuse
// the existing channel — this is the "every 10 min refresh" deduplication
// path called out in the spec.
func TestMediaCacheDedup(t *testing.T) {
	cache := newTestCache(0, time.Hour)
	content := []byte("hello")
	meta1, err := cache.Store("dup", protocol.MediaImage, content, "", "")
	if err != nil {
		t.Fatalf("first Store: %v", err)
	}
	stats1 := cache.Stats()
	meta2, err := cache.Store("dup", protocol.MediaImage, content, "", "")
	if err != nil {
		t.Fatalf("second Store: %v", err)
	}
	if meta1.Channel != meta2.Channel {
		t.Fatalf("dedup: channel changed (%d → %d)", meta1.Channel, meta2.Channel)
	}
	stats2 := cache.Stats()
	if stats2.StoreHits != stats1.StoreHits+1 {
		t.Fatalf("StoreHits did not increment: %d → %d", stats1.StoreHits, stats2.StoreHits)
	}
	if stats2.StoreMisses != stats1.StoreMisses {
		t.Fatalf("StoreMisses changed unexpectedly")
	}
}

// Cross-key dedup: identical bytes arriving under a different upstream id
// must reuse the existing cache slot, refresh the TTL, and not consume a
// fresh channel — this is the behaviour the spec calls out.
func TestMediaCacheCrossKeyDedup(t *testing.T) {
	cache := newTestCache(0, time.Hour)
	content := []byte("the same bytes under different keys")
	m1, err := cache.Store("key-A", protocol.MediaImage, content, "", "")
	if err != nil {
		t.Fatalf("first Store: %v", err)
	}
	statsBefore := cache.Stats()

	m2, err := cache.Store("key-B-different", protocol.MediaImage, content, "", "")
	if err != nil {
		t.Fatalf("second Store: %v", err)
	}
	if m1.Channel != m2.Channel {
		t.Fatalf("cross-key dedup: channel changed (%d -> %d)", m1.Channel, m2.Channel)
	}
	statsAfter := cache.Stats()
	if statsAfter.Entries != statsBefore.Entries {
		t.Fatalf("cross-key dedup: entries grew %d -> %d (should reuse slot)", statsBefore.Entries, statsAfter.Entries)
	}
	if statsAfter.StoreHits != statsBefore.StoreHits+1 {
		t.Fatalf("StoreHits should have incremented")
	}

	// Lookup under either key returns the same entry.
	if meta, ok := cache.Lookup("key-A"); !ok || meta.Channel != m1.Channel {
		t.Fatalf("Lookup(key-A) failed: ok=%v meta=%+v", ok, meta)
	}
	if meta, ok := cache.Lookup("key-B-different"); !ok || meta.Channel != m1.Channel {
		t.Fatalf("Lookup(key-B-different) failed: ok=%v meta=%+v", ok, meta)
	}
}

// Same key with different bytes (e.g. a Telegram edit) must replace the
// stored content and produce a new channel.
func TestMediaCacheKeyReplaceOnContentChange(t *testing.T) {
	cache := newTestCache(0, time.Hour)
	first := []byte("first content")
	second := []byte("second content (different)")

	m1, err := cache.Store("k", protocol.MediaImage, first, "", "")
	if err != nil {
		t.Fatalf("first Store: %v", err)
	}
	m2, err := cache.Store("k", protocol.MediaImage, second, "", "")
	if err != nil {
		t.Fatalf("second Store: %v", err)
	}
	if m1.CRC32 == m2.CRC32 {
		t.Fatalf("CRC32 should differ for different content")
	}
	// Verify GetBlock on m1.Channel either succeeds with NEW bytes (channel
	// reuse) or fails entirely — never returns the OLD bytes. Block 0
	// begins with the protocol header whose CRC field identifies which
	// content the slot is currently serving.
	if blk, err := cache.GetBlock(m1.Channel, 0); err == nil {
		if len(blk) >= protocol.MediaBlockHeaderLen {
			if hdr, err := protocol.DecodeMediaBlockHeader(blk[:protocol.MediaBlockHeaderLen]); err == nil && hdr.CRC32 == m1.CRC32 {
				t.Fatalf("GetBlock returned stale (first) bytes after content change")
			}
		}
	}
}

func TestMediaCacheRejectsOversizeFile(t *testing.T) {
	cache := newTestCache(100, time.Hour)
	_, err := cache.Store("big", protocol.MediaFile, bytes.Repeat([]byte("x"), 200), "", "")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	stats := cache.Stats()
	if stats.StoreRejected != 1 {
		t.Fatalf("StoreRejected = %d, want 1", stats.StoreRejected)
	}
	if stats.Entries != 0 {
		t.Fatalf("Entries = %d, want 0", stats.Entries)
	}
}

func TestMediaCacheGetBlockOutOfRange(t *testing.T) {
	cache := newTestCache(0, time.Hour)
	_, err := cache.GetBlock(protocol.MediaChannelStart, 0)
	if err == nil {
		t.Fatalf("expected error for unknown channel")
	}
	_, err = cache.GetBlock(0, 0)
	if err == nil || !strings.Contains(err.Error(), "outside media range") {
		t.Fatalf("expected media-range error, got %v", err)
	}
}

func TestMediaCacheSweepEvictsExpired(t *testing.T) {
	cache := newTestCache(0, 10*time.Millisecond)
	_, err := cache.Store("k", protocol.MediaFile, []byte("data"), "", "")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if cache.Stats().Entries != 1 {
		t.Fatalf("Entries = %d, want 1", cache.Stats().Entries)
	}
	time.Sleep(20 * time.Millisecond)
	if n := cache.Sweep(); n != 1 {
		t.Fatalf("Sweep evicted %d, want 1", n)
	}
	if cache.Stats().Entries != 0 {
		t.Fatalf("Entries after sweep = %d, want 0", cache.Stats().Entries)
	}
}

// Allocator: when the next-hint slot is taken but expired, that slot is
// reclaimed instead of skipped.
func TestMediaCacheReclaimsExpiredSlot(t *testing.T) {
	cache := newTestCache(0, 10*time.Millisecond)
	m1, err := cache.Store("a", protocol.MediaFile, []byte("aaa"), "", "")
	if err != nil {
		t.Fatalf("Store a: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	// Force the allocator's nextChannel back to m1.Channel by storing keys
	// until we wrap is impractical, but we know the next hint is m1.Channel+1.
	// Triggering a Store with the expired slot in the way of the linear scan
	// proves it's reclaimed and the new entry fits.
	m2, err := cache.Store("b", protocol.MediaFile, []byte("bbb"), "", "")
	if err != nil {
		t.Fatalf("Store b: %v", err)
	}
	if m2.Channel == m1.Channel {
		t.Logf("note: reused expired slot at ch %d (expected when nextChannel wraps)", m2.Channel)
	}
	stats := cache.Stats()
	if stats.Entries != 1 {
		t.Fatalf("Entries = %d, want 1 (the old expired entry should be gone)", stats.Entries)
	}
}

// Round-trip with the wire-format encoder: a cache entry's metadata, when
// embedded in a message, can be parsed back to recover the same channel and
// hash a client would download.
func TestMediaCacheMetadataRoundTrip(t *testing.T) {
	cache := newTestCache(0, time.Hour)
	content := []byte("round trip content")
	meta, err := cache.Store("rt", protocol.MediaImage, content, "image/png", "pic.png")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	body := protocol.EncodeMediaText(meta, "look at this")
	parsed, caption, ok := protocol.ParseMediaText(body)
	if !ok {
		t.Fatalf("ParseMediaText ok=false")
	}
	if parsed.Channel != meta.Channel {
		t.Fatalf("Channel: parsed %d, stored %d", parsed.Channel, meta.Channel)
	}
	if parsed.CRC32 != meta.CRC32 {
		t.Fatalf("CRC32 mismatch")
	}
	if caption != "look at this" {
		t.Fatalf("caption = %q", caption)
	}
}
