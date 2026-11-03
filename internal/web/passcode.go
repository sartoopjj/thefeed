package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// The app-lock passcode is stored as an Argon2id hash at the same cost as the
// key it also wraps (deriveKEK).
const passcodePrefix = "argon2id$"

// HashPasscode returns the stored form of a passcode, or "" if no random salt
// could be read.
func HashPasscode(passcode string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return ""
	}
	k := deriveKEK(passcode, salt)
	enc := base64.RawStdEncoding
	return passcodePrefix + enc.EncodeToString(salt) + "$" + enc.EncodeToString(k[:])
}

// CheckPasscode compares a passcode with its stored form. It returns "" when
// they do not match, otherwise what to store from now on: the same value, or a
// fresh hash when the stored one is the older SHA-256 hex form.
func CheckPasscode(passcode, stored string) string {
	if rest, ok := strings.CutPrefix(stored, passcodePrefix); ok {
		enc := base64.RawStdEncoding
		s, h, ok := strings.Cut(rest, "$")
		salt, err1 := enc.DecodeString(s)
		sum, err2 := enc.DecodeString(h)
		if !ok || err1 != nil || err2 != nil {
			return ""
		}
		k := deriveKEK(passcode, salt)
		if subtle.ConstantTimeCompare(k[:], sum) != 1 {
			return ""
		}
		return stored
	}
	old := sha256.Sum256([]byte(passcode))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(old[:])), []byte(stored)) != 1 {
		return ""
	}
	if next := HashPasscode(passcode); next != "" {
		return next
	}
	return stored
}
