package telcreds

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
)

const (
	testID   = 1234567
	testHash = "0123456789abcdef0123456789abcdef"
)

// sealed seals the test pair and links its key in, as a release build would.
func sealed(t *testing.T) string {
	t.Helper()
	blob, key, err := Seal(testID, testHash)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetBuildKeyForTest(key))
	return blob
}

func TestSealOpenRoundTrip(t *testing.T) {
	blob := sealed(t)
	id, hash, ok := Open(blob)
	if !ok || id != testID || hash != testHash {
		t.Fatalf("Open = %d %q %v", id, hash, ok)
	}
}

// The point of sealing: the blob a build embeds must not carry the pair as
// text, in any encoding a casual search would try.
func TestBlobDoesNotRevealThePair(t *testing.T) {
	blob, key, err := Seal(testID, testHash)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(key, testHash) || strings.Contains(key, strconv.Itoa(testID)) {
		t.Fatal("the build key reveals the pair")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(blob)
	for _, needle := range []string{testHash, strconv.Itoa(testID), testHash[:12]} {
		if strings.Contains(blob, needle) || bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("blob reveals %q", needle)
		}
		if strings.Contains(blob, base64.StdEncoding.EncodeToString([]byte(needle))) {
			t.Fatalf("blob carries %q as base64", needle)
		}
	}
}

// A fresh nonce per seal: two builds of the same pair do not share a blob.
func TestEachSealDiffers(t *testing.T) {
	a, ka, _ := Seal(testID, testHash)
	b, kb, _ := Seal(testID, testHash)
	if a == b || ka == kb {
		t.Fatal("two seals of the same pair share a blob or a key")
	}
}

// The point of the per-build key: the blob and this source are not enough.
// Without the key linked in -- or with another build's -- nothing opens.
func TestBlobNeedsItsOwnBuildKey(t *testing.T) {
	blob, key, err := Seal(testID, testHash)
	if err != nil {
		t.Fatal(err)
	}
	_, other, _ := Seal(testID, testHash)
	typo := "A"
	if strings.HasSuffix(key, typo) {
		typo = "B"
	}
	for name, k := range map[string]string{"no key": "", "another build's key": other, "key with a typo": key[:len(key)-1] + typo} {
		restore := SetBuildKeyForTest(k)
		if id, hash, ok := Open(blob); ok || id != 0 || hash != "" {
			t.Errorf("%s: opened to %d %q", name, id, hash)
		}
		restore()
	}
	restore := SetBuildKeyForTest(key)
	defer restore()
	if _, _, ok := Open(blob); !ok {
		t.Fatal("its own key does not open it")
	}
}

// A build without the secret, or with a damaged blob, has no default -- never
// a half-decoded one.
func TestOpenRejectsBadBlobs(t *testing.T) {
	good := sealed(t)
	raw, _ := base64.RawURLEncoding.DecodeString(good)
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)-1] ^= 1
	for name, blob := range map[string]string{
		"empty":     "",
		"spaces":    "   ",
		"not b64":   "%%%",
		"too short": base64.RawURLEncoding.EncodeToString(raw[:nonceSize]),
		"tampered":  base64.RawURLEncoding.EncodeToString(flipped),
		"plaintext": strconv.Itoa(testID) + ":" + testHash,
	} {
		if id, hash, ok := Open(blob); ok || id != 0 || hash != "" {
			t.Errorf("%s: Open = %d %q %v", name, id, hash, ok)
		}
	}
}

func TestSealRefusesAnIncompletePair(t *testing.T) {
	for _, c := range []struct {
		id   int
		hash string
	}{{0, testHash}, {-1, testHash}, {testID, ""}, {testID, "  "}} {
		if _, _, err := Seal(c.id, c.hash); err == nil {
			t.Errorf("Seal(%d, %q) accepted", c.id, c.hash)
		}
	}
}
