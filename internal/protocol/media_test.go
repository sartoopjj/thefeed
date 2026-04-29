package protocol

import (
	"strings"
	"testing"
)

func TestEncodeMediaTextRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		meta    MediaMeta
		caption string
	}{
		{
			name: "dns only",
			meta: MediaMeta{
				Tag:     MediaImage,
				Size:    123456,
				Relays:  []bool{true, false},
				Channel: 12345,
				Blocks:  42,
				CRC32:   0xabcdef01,
			},
			caption: "hello world\nmulti-line",
		},
		{
			name: "dns + github",
			meta: MediaMeta{
				Tag:      MediaFile,
				Size:     800,
				Relays:   []bool{true, true},
				Channel:  MediaChannelStart,
				Blocks:   2,
				CRC32:    0,
				Filename: "report.zip",
			},
			caption: "",
		},
		{
			name: "github only",
			meta: MediaMeta{
				Tag:    MediaImage,
				Size:   12_000_000,
				Relays: []bool{false, true},
				CRC32:  0xdeadbeef,
			},
			caption: "fast path",
		},
		{
			name: "no relays available",
			meta: MediaMeta{
				Tag:    MediaImage,
				Size:   50_000_000,
				Relays: []bool{false, false},
				CRC32:  0xdeadbeef,
			},
			caption: "too big",
		},
		{
			name: "future third relay flag survives roundtrip",
			meta: MediaMeta{
				Tag:     MediaFile,
				Size:    1024,
				Relays:  []bool{true, false, true},
				Channel: MediaChannelStart + 5,
				Blocks:  3,
				CRC32:   0xcafebabe,
			},
			caption: "future relay",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := EncodeMediaText(tc.meta, tc.caption)
			meta, caption, ok := ParseMediaText(body)
			if !ok {
				t.Fatalf("ParseMediaText returned ok=false for body %q", body)
			}
			if caption != tc.caption {
				t.Fatalf("caption = %q, want %q", caption, tc.caption)
			}
			if meta.Tag != tc.meta.Tag {
				t.Fatalf("Tag = %q, want %q", meta.Tag, tc.meta.Tag)
			}
			if meta.Size != tc.meta.Size {
				t.Fatalf("Size = %d, want %d", meta.Size, tc.meta.Size)
			}
			// Relays roundtrip: every input slot must reflect on the wire.
			for i, want := range tc.meta.Relays {
				if got := meta.HasRelay(i); got != want {
					t.Errorf("Relay %d = %v, want %v (body=%q)", i, got, want, body)
				}
			}
			if meta.Channel != tc.meta.Channel {
				t.Fatalf("Channel = %d, want %d", meta.Channel, tc.meta.Channel)
			}
			if meta.Blocks != tc.meta.Blocks {
				t.Fatalf("Blocks = %d, want %d", meta.Blocks, tc.meta.Blocks)
			}
			if meta.CRC32 != tc.meta.CRC32 {
				t.Fatalf("CRC32 = %x, want %x", meta.CRC32, tc.meta.CRC32)
			}
			wantFilename := SanitiseMediaFilename(tc.meta.Filename)
			if meta.Filename != wantFilename {
				t.Fatalf("Filename = %q, want %q", meta.Filename, wantFilename)
			}
		})
	}
}

// TestParseMediaTextUnknownRelaysIgnored is the forward-compat guarantee:
// older clients reading a wire form with extra relay flags must not fail.
func TestParseMediaTextUnknownRelaysIgnored(t *testing.T) {
	body := "[FILE]200:0,1,1,0:0:0:deadbeef:f.bin\ncap"
	meta, _, ok := ParseMediaText(body)
	if !ok {
		t.Fatalf("ok=false on multi-flag body")
	}
	if meta.HasRelay(RelayDNS) {
		t.Fatalf("RelayDNS should be false")
	}
	if !meta.HasRelay(RelayGitHub) {
		t.Fatalf("RelayGitHub should be true")
	}
	if !meta.HasRelay(2) {
		t.Fatalf("relay 2 should be true")
	}
	if meta.HasRelay(99) {
		t.Fatalf("unknown relay 99 must read as false, not panic")
	}
}

func TestSanitiseMediaFilename(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"report.zip":           "report.zip",
		"path/to/report.zip":   "report.zip",
		"..":                   "",
		"a:b\nc.txt":           "abc.txt",
		"hello":                "hello",
		"WeIrD-Name_v2.tar.gz": "WeIrD-Name_v2.tar.gz",
		"\xff\xfe.txt":         "media.txt",
		"\u062d\u0645\u0644\u0647.zip":             "media.zip",
	}
	for in, want := range cases {
		if got := SanitiseMediaFilename(in); got != want {
			t.Errorf("SanitiseMediaFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitiseMediaFilenameLongName(t *testing.T) {
	long := strings.Repeat("abc", 50) + ".zip"
	got := SanitiseMediaFilename(long)
	if !strings.HasPrefix(got, "media-") || !strings.HasSuffix(got, ".zip") {
		t.Fatalf("long filename = %q, want media-<hash>.zip", got)
	}
	if len(got) > 6+8+1+3 {
		t.Fatalf("long filename too long: %q", got)
	}
	if again := SanitiseMediaFilename(long); again != got {
		t.Fatalf("non-deterministic: %q vs %q", got, again)
	}
}

func TestParseMediaTextLegacy(t *testing.T) {
	body := "[IMAGE]\nlook at this"
	meta, caption, ok := ParseMediaText(body)
	if !ok {
		t.Fatalf("ParseMediaText ok=false on legacy body")
	}
	if meta.Tag != MediaImage {
		t.Fatalf("Tag = %q, want %q", meta.Tag, MediaImage)
	}
	if meta.HasAnyRelay() {
		t.Fatalf("legacy body should have no available relays")
	}
	if caption != "look at this" {
		t.Fatalf("caption = %q, want %q", caption, "look at this")
	}
}

func TestParseMediaTextLegacyNoCaption(t *testing.T) {
	for _, body := range []string{"[IMAGE]", "[IMAGE]\n"} {
		meta, caption, ok := ParseMediaText(body)
		if !ok {
			t.Fatalf("ok=false on %q", body)
		}
		if meta.Tag != MediaImage {
			t.Fatalf("Tag = %q, want [IMAGE]", meta.Tag)
		}
		if meta.HasAnyRelay() {
			t.Fatalf("legacy body should have no available relays")
		}
		if caption != "" {
			t.Fatalf("caption = %q, want empty", caption)
		}
	}
}

func TestParseMediaTextHumanCaption(t *testing.T) {
	body := "[IMAGE]nice picture\nrest of post"
	meta, caption, ok := ParseMediaText(body)
	if !ok {
		t.Fatalf("ok=false on caption-leading body")
	}
	if meta.HasAnyRelay() {
		t.Fatalf("human caption must not be flagged as downloadable")
	}
	if meta.Channel != 0 {
		t.Fatalf("channel should be 0 for non-metadata body, got %d", meta.Channel)
	}
	want := "nice picture\nrest of post"
	if caption != want {
		t.Fatalf("caption = %q, want %q", caption, want)
	}
}

// Unknown tag → ok=false.
func TestParseMediaTextUnknownTag(t *testing.T) {
	_, _, ok := ParseMediaText("not a tag")
	if ok {
		t.Fatalf("ok=true for non-tag body")
	}
}

// A metadata line that names a channel outside the media range must NOT be
// surfaced as DNS-downloadable; other relay flags stay as-claimed.
func TestParseMediaTextRejectsOutOfRangeChannel(t *testing.T) {
	body := "[IMAGE]100:1,1:5:200:00000000\ncaption"
	meta, _, ok := ParseMediaText(body)
	if !ok {
		t.Fatalf("ok=false on otherwise-valid metadata")
	}
	if meta.HasRelay(RelayDNS) {
		t.Fatalf("RelayDNS should be false for channel %d outside media range", meta.Channel)
	}
	if !meta.HasRelay(RelayGitHub) {
		t.Fatalf("RelayGitHub flag should survive even when DNS is rejected")
	}
}

func TestIsMediaChannel(t *testing.T) {
	checks := map[uint16]bool{
		0:                       false,
		1:                       false,
		MediaChannelStart - 1:   false,
		MediaChannelStart:       true,
		MediaChannelStart + 100: true,
		MediaChannelEnd:         true,
		MediaChannelEnd + 1:     false,
		65535:                   false,
	}
	for ch, want := range checks {
		if got := IsMediaChannel(ch); got != want {
			t.Errorf("IsMediaChannel(%d) = %v, want %v", ch, got, want)
		}
	}
}
