package server

import (
	"crypto/rand"
	"hash/crc32"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// fakeJPEG returns n bytes that start with the JPEG SOI marker so
// sniffProfilePicMime tags them as JPEG.
func fakeJPEG(n int) []byte {
	out := make([]byte, n)
	rand.Read(out)
	if n >= 3 {
		out[0] = 0xFF
		out[1] = 0xD8
		out[2] = 0xFF
	}
	return out
}

func fakePNG(n int) []byte {
	out := make([]byte, n)
	rand.Read(out)
	if n >= 4 {
		out[0] = 0x89
		out[1] = 'P'
		out[2] = 'N'
		out[3] = 'G'
	}
	return out
}

func newFeedWithMedia(t *testing.T) *Feed {
	t.Helper()
	f := NewFeed([]string{"a", "b"})
	mc := NewMediaCache(MediaCacheConfig{
		MaxFileBytes:    64 * 1024,
		TTL:             time.Hour,
		DNSRelayEnabled: true,
	})
	f.SetMediaCache(mc)
	return f
}

func TestSetProfilePicsBundlesAvatarsAndExposesDirectory(t *testing.T) {
	f := newFeedWithMedia(t)
	alice := fakeJPEG(1024)
	bob := fakePNG(2048)

	stored := f.SetProfilePics(map[string][]byte{"alice": alice, "bob": bob})
	if stored != 2 {
		t.Fatalf("stored = %d, want 2", stored)
	}

	got := f.ProfilePicsBundle()
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(got.Entries))
	}
	// Sorted by username.
	if got.Entries[0].Username != "alice" || got.Entries[1].Username != "bob" {
		t.Errorf("entries = %s,%s want alice,bob",
			got.Entries[0].Username, got.Entries[1].Username)
	}
	// MIME survived the sniff.
	if got.Entries[0].MIME != protocol.ProfilePicMimeJPEG {
		t.Errorf("alice MIME = %d want JPEG", got.Entries[0].MIME)
	}
	if got.Entries[1].MIME != protocol.ProfilePicMimePNG {
		t.Errorf("bob MIME = %d want PNG", got.Entries[1].MIME)
	}
	// Each per-entry CRC matches the original bytes.
	if got.Entries[0].CRC != crc32.ChecksumIEEE(alice) {
		t.Errorf("alice crc mismatch")
	}
	if got.Entries[1].CRC != crc32.ChecksumIEEE(bob) {
		t.Errorf("bob crc mismatch")
	}
	// Offsets are tightly packed.
	if got.Entries[0].Offset != 0 {
		t.Errorf("alice offset = %d, want 0", got.Entries[0].Offset)
	}
	if got.Entries[1].Offset != got.Entries[0].Size {
		t.Errorf("bob offset = %d, want %d (right after alice)",
			got.Entries[1].Offset, got.Entries[0].Size)
	}

	// Bundle metadata: total bytes = sum of avatars.
	if got.Header.BundleSize != got.Entries[0].Size+got.Entries[1].Size {
		t.Errorf("BundleSize = %d, want sum of entries (%d)",
			got.Header.BundleSize, got.Entries[0].Size+got.Entries[1].Size)
	}
	// Each entry has its own DNS channel inside the media range.
	for i, e := range got.Entries {
		if e.DNSChannel < protocol.MediaChannelStart || e.DNSChannel > protocol.MediaChannelEnd {
			t.Errorf("entries[%d].DNSChannel = %d, outside media range",
				i, e.DNSChannel)
		}
		if e.DNSBlocks == 0 {
			t.Errorf("entries[%d].DNSBlocks = 0", i)
		}
	}
	// And those per-entry DNS channels are fetchable as ordinary media.
	for i, e := range got.Entries {
		blk0, err := f.GetBlock(int(e.DNSChannel), 0)
		if err != nil || len(blk0) == 0 {
			t.Errorf("entries[%d] DNS block 0: %v / %d bytes", i, err, len(blk0))
		}
	}

	// And we can pull the directory back over the wire and re-derive
	// each avatar's slice with VerifyEntry.
	dirBlk, err := f.GetBlock(int(protocol.ProfilePicsChannel), 0)
	if err != nil {
		t.Fatalf("get profile-pics block 0: %v", err)
	}
	if len(dirBlk) < 4 {
		t.Fatalf("dir block too small: %d", len(dirBlk))
	}
	totalBlocks := int(dirBlk[0])<<8 | int(dirBlk[1])
	all := dirBlk[2:]
	for i := 1; i < totalBlocks; i++ {
		next, err := f.GetBlock(int(protocol.ProfilePicsChannel), i)
		if err != nil {
			t.Fatalf("get profile-pics block %d: %v", i, err)
		}
		all = append(all, next...)
	}
	decoded, err := protocol.DecodeProfilePicsBundle(all)
	if err != nil {
		t.Fatalf("decode dir: %v", err)
	}
	if len(decoded.Entries) != 2 {
		t.Fatalf("decoded entries = %d, want 2", len(decoded.Entries))
	}

	// Each entry's DNS channel is independently fetchable. This is the
	// "if even one DNS channel works, that one avatar still shows" path.
	// Full byte-level round-trip is exercised in the client tests.
	for _, e := range decoded.Entries {
		blk0, err := f.GetBlock(int(e.DNSChannel), 0)
		if err != nil || len(blk0) == 0 {
			t.Errorf("entry %s: channel %d block 0: %v",
				e.Username, e.DNSChannel, err)
		}
	}
}

func TestSetProfilePicsExposesRelayBits(t *testing.T) {
	f := newFeedWithMedia(t) // DNSRelayEnabled: true, no GitHub relay attached
	f.SetProfilePics(map[string][]byte{"alice": fakeJPEG(1024)})
	got := f.ProfilePicsBundle()
	if !got.Header.HasRelay(protocol.RelayDNS) {
		t.Errorf("RelayDNS bit should be set when DNS is enabled, got Relays=%v", got.Header.Relays)
	}
	if got.Header.HasRelay(protocol.RelayGitHub) {
		t.Errorf("RelayGitHub bit should not be set without a GitHub relay, got Relays=%v", got.Header.Relays)
	}
}

func TestSetProfilePicsSkipsEmpty(t *testing.T) {
	f := newFeedWithMedia(t)
	stored := f.SetProfilePics(map[string][]byte{
		"":      fakeJPEG(100), // empty username
		"empty": nil,           // empty bytes
		"good":  fakeJPEG(100),
	})
	if stored != 1 {
		t.Errorf("stored = %d, want 1", stored)
	}
}

func TestSetProfilePicsReplaceUpdatesBundle(t *testing.T) {
	f := newFeedWithMedia(t)
	first := fakeJPEG(1024)
	f.SetProfilePics(map[string][]byte{"alice": first})
	b1 := f.ProfilePicsBundle()

	second := fakeJPEG(2048)
	f.SetProfilePics(map[string][]byte{"alice": second})
	b2 := f.ProfilePicsBundle()

	if len(b1.Entries) != 1 || len(b2.Entries) != 1 {
		t.Fatalf("expected one entry each, got %d / %d", len(b1.Entries), len(b2.Entries))
	}
	if b1.Entries[0].CRC == b2.Entries[0].CRC {
		t.Errorf("entry CRC didn't change after replacing bytes")
	}
	if b1.Header.BundleCRC == b2.Header.BundleCRC {
		t.Errorf("bundle CRC didn't change after replacing bytes")
	}
}

func TestSetProfilePicsClearsOnEmpty(t *testing.T) {
	f := newFeedWithMedia(t)
	f.SetProfilePics(map[string][]byte{"alice": fakeJPEG(1024)})
	if got := f.ProfilePicsBundle(); len(got.Entries) != 1 {
		t.Fatalf("setup: got %d entries", len(got.Entries))
	}
	stored := f.SetProfilePics(nil)
	if stored != 0 {
		t.Errorf("stored = %d, want 0", stored)
	}
	got := f.ProfilePicsBundle()
	if len(got.Entries) != 0 {
		t.Errorf("entries = %d, want 0 after empty refresh", len(got.Entries))
	}
}

func TestMergeProfilePicsKeepsOtherEntries(t *testing.T) {
	f := newFeedWithMedia(t)
	// First reader contributes Telegram avatars.
	tg := map[string][]byte{
		"alice": fakeJPEG(800),
		"bob":   fakeJPEG(900),
	}
	if got := f.SetProfilePics(tg); got != 2 {
		t.Fatalf("seed Set: got %d want 2", got)
	}

	// Second reader contributes X avatars under the "x:" namespace —
	// the Telegram entries should survive.
	xs := map[string][]byte{
		"x:elonmusk": fakePNG(700),
	}
	if got := f.MergeProfilePics(xs); got != 3 {
		t.Fatalf("merge: got %d want 3", got)
	}

	got := f.ProfilePicsBundle()
	if len(got.Entries) != 3 {
		t.Fatalf("entries = %d want 3", len(got.Entries))
	}
	have := map[string]bool{}
	for _, e := range got.Entries {
		have[e.Username] = true
	}
	for _, want := range []string{"alice", "bob", "x:elonmusk"} {
		if !have[want] {
			t.Errorf("missing entry %q in merged bundle", want)
		}
	}
}

func TestMergeProfilePicsDropEntry(t *testing.T) {
	f := newFeedWithMedia(t)
	f.SetProfilePics(map[string][]byte{
		"alice": fakeJPEG(800),
		"bob":   fakeJPEG(900),
	})
	// nil bytes for an existing key drops it.
	if got := f.MergeProfilePics(map[string][]byte{"alice": nil}); got != 1 {
		t.Fatalf("merge: got %d want 1", got)
	}
	got := f.ProfilePicsBundle()
	if len(got.Entries) != 1 || got.Entries[0].Username != "bob" {
		t.Errorf("entries after drop = %+v", got.Entries)
	}
}

func TestSetProfilePicsNoMediaCache(t *testing.T) {
	f := NewFeed([]string{"a"})
	stored := f.SetProfilePics(map[string][]byte{"alice": fakeJPEG(100)})
	if stored != 0 {
		t.Errorf("stored = %d, want 0 (media cache not configured)", stored)
	}
	if got := f.ProfilePicsBundle(); len(got.Entries) != 0 {
		t.Errorf("entries = %v, want empty", got.Entries)
	}
}

