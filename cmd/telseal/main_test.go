package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sartoopjj/thefeed/internal/telcreds"
)

// parseFlags splits telseal's output back into the two values it links in.
func parseFlags(t *testing.T, flags string) (blob, key string) {
	t.Helper()
	f := strings.Fields(flags)
	if len(f) != 4 || f[0] != "-X" || f[2] != "-X" ||
		!strings.HasPrefix(f[1], blobVar+"=") || !strings.HasPrefix(f[3], keyVar+"=") {
		t.Fatalf("unexpected linker flags: %q", flags)
	}
	return strings.TrimPrefix(f[1], blobVar+"="), strings.TrimPrefix(f[3], keyVar+"=")
}

const goodHash = "0123456789abcdef0123456789abcdef"

// Every way the two CI secrets can be set. A missing secret reaches the build
// as an empty string, so "unset" and "empty" must mean the same thing.
func TestSealFromEnv(t *testing.T) {
	for _, c := range []struct {
		name, id, hash string
		wantBlob       bool
		wantErr        bool
	}{
		{"both missing", "", "", false, false},
		{"both whitespace", "  ", "\n", false, false},
		{"only the id", "1234567", "", false, true},
		{"only the hash", "", goodHash, false, true},
		{"id not a number", "abc", goodHash, false, true},
		{"id zero", "0", goodHash, false, true},
		{"id negative", "-5", goodHash, false, true},
		{"hash too short", "1234567", "feedface", false, true},
		{"hash upper case", "1234567", "0123456789ABCDEF0123456789ABCDEF", false, true},
		{"hash not hex", "1234567", "0123456789abcdef0123456789abcdeg", false, true},
		{"hash too long", "1234567", goodHash + "0", false, true},
		{"pasted with spaces and newlines", " 1234567\n", "\n" + goodHash + " ", true, false},
		{"correct", "1234567", goodHash, true, false},
	} {
		flags, err := sealFromEnv(c.id, c.hash)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
			continue
		}
		if (flags != "") != c.wantBlob {
			t.Errorf("%s: flags present = %v, want %v", c.name, flags != "", c.wantBlob)
			continue
		}
		if c.wantBlob {
			blob, key := parseFlags(t, flags)
			restore := telcreds.SetBuildKeyForTest(key)
			id, hash, ok := telcreds.Open(blob)
			restore()
			if !ok || id != 1234567 || hash != goodHash {
				t.Errorf("%s: blob opens to %d %q %v", c.name, id, hash, ok)
			}
		}
	}
}

// The values go straight onto a -ldflags command line, unquoted in places, so
// they must be made of characters no shell or linker treats specially.
func TestFlagsAreSafeOnACommandLine(t *testing.T) {
	safe := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for i := 0; i < 200; i++ {
		flags, err := sealFromEnv("1234567", goodHash)
		if err != nil {
			t.Fatal(err)
		}
		blob, key := parseFlags(t, flags)
		if !safe.MatchString(blob) || !safe.MatchString(key) {
			t.Fatalf("a value has a character the command line may mangle: %q", flags)
		}
	}
}

// Each build gets its own key: two builds of the same pair share nothing a
// reader of one could reuse on the other.
func TestEachBuildGetsItsOwnKey(t *testing.T) {
	a, _ := sealFromEnv("1234567", goodHash)
	b, _ := sealFromEnv("1234567", goodHash)
	blobA, keyA := parseFlags(t, a)
	blobB, keyB := parseFlags(t, b)
	if keyA == keyB || blobA == blobB {
		t.Fatal("two builds produced the same key or blob")
	}
	restore := telcreds.SetBuildKeyForTest(keyA)
	defer restore()
	if _, _, ok := telcreds.Open(blobB); ok {
		t.Fatal("one build's key opened another build's blob")
	}
}
