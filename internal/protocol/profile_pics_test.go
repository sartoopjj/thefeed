package protocol

import (
	"hash/crc32"
	"strings"
	"testing"
)

func TestEncodeDecodeProfilePicsBundleRoundTrip(t *testing.T) {
	// Three avatars concatenated into a fake bundle, with offsets/CRCs
	// computed for real so VerifyEntry doesn't trip on them.
	a := []byte("aaaaaaaaaa")    // 10 bytes
	b := []byte("bbbbbbbbbbbbbb") // 14 bytes
	c := []byte("ccccc")          // 5 bytes
	bundle := append(append(append([]byte{}, a...), b...), c...)

	in := ProfilePicsBundle{
		Header: ProfilePicsBundleHeader{
			BundleSize: uint32(len(bundle)),
			BundleCRC:  crc32.ChecksumIEEE(bundle),
			Relays:     []bool{false, true}, // bundle on GitHub only; per-entry DNS handles DNS path
		},
		Entries: []ProfilePicEntry{
			{Username: "alice", Offset: 0, Size: uint32(len(a)), CRC: crc32.ChecksumIEEE(a), MIME: ProfilePicMimeJPEG, DNSChannel: 10001, DNSBlocks: 1},
			{Username: "bob", Offset: uint32(len(a)), Size: uint32(len(b)), CRC: crc32.ChecksumIEEE(b), MIME: ProfilePicMimePNG, DNSChannel: 10002, DNSBlocks: 2},
			{Username: "carol", Offset: uint32(len(a) + len(b)), Size: uint32(len(c)), CRC: crc32.ChecksumIEEE(c), MIME: ProfilePicMimeWebP, DNSChannel: 10003, DNSBlocks: 1},
		},
	}
	wire := EncodeProfilePicsBundle(in)
	got, err := DecodeProfilePicsBundle(wire)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Header.BundleSize != in.Header.BundleSize ||
		got.Header.BundleCRC != in.Header.BundleCRC {
		t.Errorf("header mismatch: got %+v want %+v", got.Header, in.Header)
	}
	if len(got.Header.Relays) != len(in.Header.Relays) {
		t.Fatalf("relays len = %d, want %d", len(got.Header.Relays), len(in.Header.Relays))
	}
	for i, r := range in.Header.Relays {
		if got.Header.Relays[i] != r {
			t.Errorf("relays[%d] = %v, want %v", i, got.Header.Relays[i], r)
		}
	}
	if len(got.Entries) != len(in.Entries) {
		t.Fatalf("entries len = %d, want %d", len(got.Entries), len(in.Entries))
	}
	for i, want := range in.Entries {
		if got.Entries[i] != want {
			t.Errorf("entry %d = %+v want %+v", i, got.Entries[i], want)
		}
	}

	// VerifyEntry should accept the real bytes…
	for _, e := range got.Entries {
		if _, err := VerifyEntry(bundle, e); err != nil {
			t.Errorf("VerifyEntry(%s) = %v, want ok", e.Username, err)
		}
	}
	// …and reject a tampered bundle.
	tampered := append([]byte{}, bundle...)
	tampered[0] ^= 0xFF
	if _, err := VerifyEntry(tampered, got.Entries[0]); err == nil {
		t.Errorf("VerifyEntry should fail on tampered bundle")
	}
}

func TestVerifyEntryOutOfRange(t *testing.T) {
	bundle := []byte("hello")
	e := ProfilePicEntry{Username: "x", Offset: 0, Size: 100, CRC: 0}
	if _, err := VerifyEntry(bundle, e); err == nil {
		t.Errorf("VerifyEntry should fail when entry runs past bundle end")
	}
}

func TestVerifyEntrySizeMismatch(t *testing.T) {
	bundle := []byte("0123456789")
	// CRC is right for the real slice, but Size is wrong → mismatch.
	right := bundle[2:6]
	e := ProfilePicEntry{
		Username: "y",
		Offset:   2,
		Size:     5, // claim 5 but slice is 4
		CRC:      crc32.ChecksumIEEE(right),
	}
	// In this case Size=5 + Offset=2 → end=7, in range. CRC will be checked
	// over bundle[2:7] which differs from right → mismatch.
	if _, err := VerifyEntry(bundle, e); err == nil {
		t.Errorf("VerifyEntry should fail when claimed size doesn't match recorded crc")
	}
}

func TestProfilePicsBundleEmpty(t *testing.T) {
	in := ProfilePicsBundle{
		Header: ProfilePicsBundleHeader{Relays: []bool{false, false}},
	}
	wire := EncodeProfilePicsBundle(in)
	got, err := DecodeProfilePicsBundle(wire)
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(got.Entries) != 0 {
		t.Errorf("entries = %d want 0", len(got.Entries))
	}
}

func TestProfilePicsTruncatesLongUsername(t *testing.T) {
	long := strings.Repeat("x", 300)
	in := ProfilePicsBundle{
		Header: ProfilePicsBundleHeader{Relays: []bool{true}},
		Entries: []ProfilePicEntry{
			{Username: long, Offset: 0, Size: 100, CRC: 1, MIME: 0},
		},
	}
	wire := EncodeProfilePicsBundle(in)
	got, err := DecodeProfilePicsBundle(wire)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entries) != 1 || len(got.Entries[0].Username) != 255 {
		t.Errorf("expected 1 entry with 255-char username, got %+v", got.Entries)
	}
}

func TestProfilePicsTruncatedDataReturnsError(t *testing.T) {
	in := ProfilePicsBundle{
		Header: ProfilePicsBundleHeader{Relays: []bool{true}},
		Entries: []ProfilePicEntry{
			{Username: "a", Offset: 0, Size: 1, CRC: 1, MIME: 0},
		},
	}
	wire := EncodeProfilePicsBundle(in)
	for _, cut := range []int{0, 1, 2, 3, len(wire) - 1} {
		_, err := DecodeProfilePicsBundle(wire[:cut])
		if err == nil {
			t.Errorf("expected error on cut=%d", cut)
		}
	}
}

func TestProfilePicsChannelConstant(t *testing.T) {
	if ProfilePicsChannel != 0xFFF7 {
		t.Errorf("ProfilePicsChannel = 0x%X, want 0xFFF7", ProfilePicsChannel)
	}
	others := []uint16{
		SendChannel, AdminChannel, UpstreamInitChannel, UpstreamDataChannel,
		VersionChannel, TitlesChannel, RelayInfoChannel,
	}
	for _, o := range others {
		if o == ProfilePicsChannel {
			t.Fatalf("ProfilePicsChannel collides with another control channel: 0x%X", o)
		}
	}
}

func TestBundleHasRelay(t *testing.T) {
	h := ProfilePicsBundleHeader{Relays: []bool{false, true}}
	if h.HasRelay(RelayDNS) {
		t.Errorf("RelayDNS should be false")
	}
	if !h.HasRelay(RelayGitHub) {
		t.Errorf("RelayGitHub should be true")
	}
	if h.HasRelay(99) || h.HasRelay(-1) {
		t.Errorf("out-of-range should return false")
	}
}
