package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// HKDF context labels — kept distinct from "thefeed-query"/"thefeed-response"
// so the relay key is cryptographically independent of the DNS keys.
const (
	relayKeyContext    = "thefeed-relay"
	relayDomainContext = "thefeed-relay-domain"
	relayObjectContext = "thefeed-relay-object"
)

// Truncated HMAC widths (in hex chars). 64 bits for the domain folder is
// plenty — repos typically host a handful of deployments. 96 bits for object
// names guards against an attacker confirming "is file X in the repo?".
const (
	relayDomainHexLen = 16
	relayObjectHexLen = 24
)

// DeriveRelayKey derives the AES-256 key used to encrypt blobs uploaded to a
// shared relay (e.g. a public GitHub repo) and to HMAC the path segments.
// Returns an error only if HKDF fails; the result is deterministic for a
// given passphrase.
func DeriveRelayKey(passphrase string) ([KeySize]byte, error) {
	var key [KeySize]byte
	master := sha256.Sum256([]byte(passphrase))
	rdr := hkdf.New(sha256.New, master[:], nil, []byte(relayKeyContext))
	if _, err := io.ReadFull(rdr, key[:]); err != nil {
		return key, err
	}
	return key, nil
}

// RelayDomainSegment returns the path segment that scopes a deployment's
// files inside a shared relay repo. Computed as HMAC-SHA256 over the domain,
// keyed by a passphrase-derived secret, then truncated. Without the
// passphrase an observer cannot tell which deployment a folder belongs to.
func RelayDomainSegment(domain, passphrase string) string {
	return relayHMAC(passphrase, relayDomainContext, domain)[:relayDomainHexLen]
}

// RelayObjectName returns the per-file path segment under the domain folder.
// Computed from (size, crc) so the same content always lives at the same
// path (dedup), but HMAC'd with the passphrase so an observer can't probe
// "is a known file present in this repo?".
func RelayObjectName(size int64, crc uint32, passphrase string) string {
	label := fmt.Sprintf("%d_%08x", size, crc)
	return relayHMAC(passphrase, relayObjectContext, label)[:relayObjectHexLen]
}

// EncryptRelayBlob seals plaintext with the relay key. Output is
// nonce||ciphertext||tag, identical framing to the DNS response cipher so
// clients can reuse Decrypt for both paths.
func EncryptRelayBlob(key [KeySize]byte, plaintext []byte) ([]byte, error) {
	return Encrypt(key, plaintext)
}

// DecryptRelayBlob is the inverse of EncryptRelayBlob.
func DecryptRelayBlob(key [KeySize]byte, blob []byte) ([]byte, error) {
	return Decrypt(key, blob)
}

func relayHMAC(passphrase, ctx, msg string) string {
	master := sha256.Sum256([]byte(passphrase))
	h := hmac.New(sha256.New, master[:])
	h.Write([]byte(ctx))
	h.Write([]byte{0}) // separator: prevents ctx||msg collisions
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}
