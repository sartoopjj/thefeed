// Package telcreds carries the bundled Telegram app credentials into release
// builds without them appearing in the source tree or as readable text in the
// binary.
//
// The pair is injected at build time from CI secrets and sealed under a key
// made fresh for each build. The key is linked in beside the blob, in another
// package, and is in neither the source nor the logs: recovering the pair
// takes both pieces out of a binary, not a script run against this code.
// That is still obfuscation, not secrecy -- an app has to hand the pair to
// Telegram, so whoever runs it can find it -- but it keeps the pair out of the
// repository, out of `strings` output, and out of reach of a casual search;
// Telegram restricts an api_id it finds published.
package telcreds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// buildKey is the per-build half of the key, set by the linker alongside the
// blob (see cmd/telseal). A blob opens only with the key it was sealed under.
var buildKey string

// pepper mixes into every key. Changing it invalidates every sealed blob.
var pepper = [32]byte{
	0x3e, 0x42, 0x43, 0x2f, 0xfa, 0xf4, 0x3a, 0xa3, 0x1c, 0xa0, 0x15, 0x17, 0xab, 0x1d, 0xf8, 0x89,
	0xbf, 0x90, 0xa0, 0x58, 0x0e, 0xc6, 0x69, 0x21, 0xc7, 0x06, 0xe5, 0x50, 0xeb, 0xe5, 0x99, 0x7c,
}

const nonceSize = 16

func aeadFor(key string, nonce []byte) (cipher.AEAD, error) {
	h := sha256.New()
	h.Write(pepper[:])
	h.Write([]byte(key))
	h.Write(nonce)
	block, err := aes.NewCipher(h.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal turns an api id and hash into the blob a build embeds, and the fresh
// build key it opens with. Both go to the linker; neither is useful alone.
func Seal(id int, hash string) (blob, key string, err error) {
	hash = strings.TrimSpace(hash)
	if id <= 0 || hash == "" {
		return "", "", errors.New("telcreds: api id and hash required")
	}
	rnd := make([]byte, nonceSize+32)
	if _, err := rand.Read(rnd); err != nil {
		return "", "", err
	}
	nonce := rnd[:nonceSize]
	key = base64.RawURLEncoding.EncodeToString(rnd[nonceSize:])
	aead, err := aeadFor(key, nonce)
	if err != nil {
		return "", "", err
	}
	plain := []byte(strconv.Itoa(id) + ":" + hash)
	out := aead.Seal(nil, make([]byte, aead.NonceSize()), plain, nil)
	return base64.RawURLEncoding.EncodeToString(append(nonce, out...)), key, nil
}

// SetBuildKeyForTest stands in for the linker in tests.
func SetBuildKeyForTest(key string) (restore func()) {
	prev := buildKey
	buildKey = key
	return func() { buildKey = prev }
}

// Open returns the pair a blob carries, using the build key linked into this
// binary. ok is false for an empty or damaged blob, or one sealed under
// another key: a build without the secret simply has no default.
func Open(blob string) (id int, hash string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(blob))
	if err != nil || len(raw) <= nonceSize || buildKey == "" {
		return 0, "", false
	}
	aead, err := aeadFor(buildKey, raw[:nonceSize])
	if err != nil {
		return 0, "", false
	}
	plain, err := aead.Open(nil, make([]byte, aead.NonceSize()), raw[nonceSize:], nil)
	if err != nil {
		return 0, "", false
	}
	idStr, hash, found := strings.Cut(string(plain), ":")
	id, err = strconv.Atoi(idStr)
	if !found || err != nil || id <= 0 || hash == "" {
		return 0, "", false
	}
	return id, hash, true
}
