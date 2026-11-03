package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestPasscodeHashAndCheck(t *testing.T) {
	stored := HashPasscode("482913")
	if got := CheckPasscode("482913", stored); got != stored {
		t.Fatalf("right passcode: %q", got)
	}
	if got := CheckPasscode("482914", stored); got != "" {
		t.Fatal("wrong passcode accepted")
	}
	if HashPasscode("482913") == stored {
		t.Fatal("two hashes of one passcode are equal: no salt")
	}
}

// Checking a guess against the stored form costs a full deriveKEK, the same as
// opening the key the passcode wraps.
func TestPasscodeStoredFormIsTheKEK(t *testing.T) {
	stored := HashPasscode("2580")
	old := sha256.Sum256([]byte("2580"))
	if strings.Contains(stored, hex.EncodeToString(old[:])) {
		t.Fatal("the fast hash is still stored")
	}
	parts := strings.Split(strings.TrimPrefix(stored, passcodePrefix), "$")
	if !strings.HasPrefix(stored, passcodePrefix) || len(parts) != 2 {
		t.Fatalf("stored form %q", stored)
	}
	salt, _ := base64.RawStdEncoding.DecodeString(parts[0])
	sum, _ := base64.RawStdEncoding.DecodeString(parts[1])
	k := deriveKEK("2580", salt)
	if !bytes.Equal(sum, k[:]) {
		t.Fatal("stored hash is not deriveKEK")
	}
}

// A passcode set by an older version is still accepted, and comes back in the
// current form; a wrong one is not upgraded.
func TestPasscodeOldFormIsUpgraded(t *testing.T) {
	h := sha256.Sum256([]byte("1379"))
	old := hex.EncodeToString(h[:])
	if CheckPasscode("1378", old) != "" {
		t.Fatal("wrong passcode accepted against the old form")
	}
	next := CheckPasscode("1379", old)
	if !strings.HasPrefix(next, passcodePrefix) {
		t.Fatalf("not upgraded: %q", next)
	}
	if CheckPasscode("1379", next) != next {
		t.Fatal("the upgraded form does not check")
	}
}

func TestPasscodeBrokenStoredFormMatchesNothing(t *testing.T) {
	for _, s := range []string{"", passcodePrefix, passcodePrefix + "$", passcodePrefix + "AAAA$AAAA", passcodePrefix + "!!$!!", "argon2id"} {
		if CheckPasscode("", s) != "" || CheckPasscode("x", s) != "" {
			t.Errorf("%q matched", s)
		}
	}
}

// Values the previous Android version stored, computed by the JVM itself
// (UTF-8 bytes, SHA-256, "%02x" per byte): those users must still get in.
func TestPasscodeOldAndroidValuesStillOpen(t *testing.T) {
	for pw, old := range map[string]string{
		"password123": "ef92b778bafe771e89245b89ecbc08a44a4e166c06659911881f383d4473e94f",
		"رمز عبور خیلی طولانی من 🔐 با فاصله": "89840db9f154e5ba736dac0b6538327ccf3f9711a030e08d5d3c599c84975e91",
	} {
		next := CheckPasscode(pw, old)
		if !strings.HasPrefix(next, passcodePrefix) || CheckPasscode(pw, next) != next {
			t.Errorf("%q: not let in / not upgraded (%q)", pw, next)
		}
		if CheckPasscode(pw+" ", old) != "" {
			t.Errorf("%q: a different password opened it", pw)
		}
	}
}
