package client

import (
	"hash/crc32"
	"testing"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

func TestProfilePicMimeAndExtension(t *testing.T) {
	cases := []struct {
		mime    uint8
		wantStr string
		wantExt string
	}{
		{protocol.ProfilePicMimeJPEG, "image/jpeg", ".jpg"},
		{protocol.ProfilePicMimePNG, "image/png", ".png"},
		{protocol.ProfilePicMimeWebP, "image/webp", ".webp"},
		{255, "image/jpeg", ".jpg"}, // unknown → JPEG fallback
	}
	for _, c := range cases {
		p := ProfilePicEntry{MIME: c.mime}
		if got := p.MimeString(); got != c.wantStr {
			t.Errorf("MimeString(%d) = %q, want %q", c.mime, got, c.wantStr)
		}
		if got := p.Extension(); got != c.wantExt {
			t.Errorf("Extension(%d) = %q, want %q", c.mime, got, c.wantExt)
		}
	}
}

func TestDecodeProfilePicsBundleRoundTrip(t *testing.T) {
	// Build a real bundle so the Verify check the caller will run later
	// would still succeed.
	a := []byte("hello-alice")
	b := []byte("hello-bob-bob-bob")
	bundle := append(append([]byte{}, a...), b...)
	wire := protocol.EncodeProfilePicsBundle(protocol.ProfilePicsBundle{
		Header: protocol.ProfilePicsBundleHeader{
			BundleSize: uint32(len(bundle)),
			BundleCRC:  crc32.ChecksumIEEE(bundle),
			Relays:     []bool{false, true},
		},
		Entries: []protocol.ProfilePicEntry{
			{Username: "alice", Offset: 0, Size: uint32(len(a)), CRC: crc32.ChecksumIEEE(a), MIME: protocol.ProfilePicMimeJPEG, DNSChannel: 10001, DNSBlocks: 1},
			{Username: "bob", Offset: uint32(len(a)), Size: uint32(len(b)), CRC: crc32.ChecksumIEEE(b), MIME: protocol.ProfilePicMimePNG, DNSChannel: 10002, DNSBlocks: 2},
		},
	})
	got, err := decodeProfilePicsBundle(wire)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BundleSize != uint32(len(bundle)) {
		t.Errorf("bundle metadata wrong: %+v", got)
	}
	if got.Entries[0].DNSChannel != 10001 || got.Entries[1].DNSChannel != 10002 {
		t.Errorf("dns channels lost: %+v", got.Entries)
	}
	if got.HasRelay(protocol.RelayDNS) || !got.HasRelay(protocol.RelayGitHub) {
		t.Errorf("relays = %v, want GitHub-only", got.Relays)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(got.Entries))
	}
	if got.Entries[0].Username != "alice" || got.Entries[1].Username != "bob" {
		t.Errorf("entries: %+v", got.Entries)
	}
	if got.Entries[1].MIME != protocol.ProfilePicMimePNG {
		t.Errorf("bob MIME = %d, want PNG", got.Entries[1].MIME)
	}
}

func TestDecodeProfilePicsBundleEmpty(t *testing.T) {
	got, err := decodeProfilePicsBundle(nil)
	if err != nil || len(got.Entries) != 0 {
		t.Errorf("decode(nil) = %v, %v", got, err)
	}
}
