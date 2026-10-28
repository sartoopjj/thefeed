package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Per-query session seal. A per-connection eph↔ek ECDH (mixed with the public
// query key, so the passphrase stays a required gate) yields Ksession; every
// in-context query is AES-CTR encrypted and HMAC-tagged under keys derived from
// it, with a nonce derived from the visible (selector,counter) — nothing extra
// on the wire. The 4-byte tag is enough: forging it needs ~2^32 online,
// rate-limited DNS round-trips and only yields E2E-encrypted payloads.

const (
	chatSessionInfo = "thefeed-chat-session-v1"
	chatSealEncInfo = "thefeed-chat-seal-enc-v1"
	chatSealMacInfo = "thefeed-chat-seal-mac-v1"
	// ChatSealTagSize is the truncated per-query MAC length.
	ChatSealTagSize = 4
)

// ChatSessionKey derives the per-connection session key from an eph↔ek ECDH,
// mixing the query key into the HKDF info so the public passphrase is required,
// and the negotiated protocol version so a tampered/cleartext version byte in
// the handshake can't downgrade: a different version yields a different key, so
// the sealed bootstrap fails to open (fail-closed) rather than being accepted
// under another version.
func ChatSessionKey(own *ecdh.PrivateKey, peerPub []byte, protoVer byte, queryKey [KeySize]byte) ([KeySize]byte, error) {
	return sessionKey(own, peerPub, chatSessionInfo, protoVer, queryKey)
}

// sessionKey is the shared eph↔ek derivation. The context string gives each
// user of the cell layer its own key space: a chat key can never open a tunnel
// cell, or the reverse, even on the same sub-domain.
func sessionKey(own *ecdh.PrivateKey, peerPub []byte, context string, protoVer byte, queryKey [KeySize]byte) ([KeySize]byte, error) {
	var k [KeySize]byte
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return k, fmt.Errorf("%s: bad session peer key: %w", context, err)
	}
	secret, err := own.ECDH(pub)
	if err != nil {
		return k, fmt.Errorf("%s: session ecdh: %w", context, err)
	}
	info := append([]byte(context), protoVer)
	info = append(info, queryKey[:]...)
	_, err = io.ReadFull(hkdf.New(sha256.New, secret, nil, info), k[:])
	return k, err
}

func chatSealKeys(ksession [KeySize]byte) (enc, mac [KeySize]byte) {
	_, _ = io.ReadFull(hkdf.New(sha256.New, ksession[:], nil, []byte(chatSealEncInfo)), enc[:])
	_, _ = io.ReadFull(hkdf.New(sha256.New, ksession[:], nil, []byte(chatSealMacInfo)), mac[:])
	return
}

// chatSealNonce packs the visible selector and counter into one AES block. The
// selector is ≤4 bytes and the counter is 4, so they fit with room to spare;
// (selector,counter) is unique per sealed query, so the CTR keystream never
// repeats under one key.
func chatSealNonce(selector []byte, counter uint32) [aes.BlockSize]byte {
	var n [aes.BlockSize]byte
	copy(n[:4], selector)
	binary.BigEndian.PutUint32(n[4:8], counter)
	return n
}

// SealChat returns ct‖tag(4): AES-CTR of plaintext, then a truncated HMAC over
// nonce‖ct. Deterministic for the same (key,selector,counter,plaintext).
func SealChat(ksession [KeySize]byte, selector []byte, counter uint32, plaintext []byte) []byte {
	enc, mac := chatSealKeys(ksession)
	nonce := chatSealNonce(selector, counter)
	block, err := aes.NewCipher(enc[:])
	if err != nil {
		panic(fmt.Sprintf("chat seal aes: %v", err))
	}
	out := make([]byte, len(plaintext)+ChatSealTagSize)
	cipher.NewCTR(block, nonce[:]).XORKeyStream(out, plaintext)
	h := hmac.New(sha256.New, mac[:])
	h.Write(nonce[:])
	h.Write(out[:len(plaintext)])
	copy(out[len(plaintext):], h.Sum(nil)[:ChatSealTagSize])
	return out
}

// OpenChat verifies the tag and decrypts. Fail-closed on any mismatch.
func OpenChat(ksession [KeySize]byte, selector []byte, counter uint32, sealed []byte) ([]byte, error) {
	if len(sealed) < ChatSealTagSize {
		return nil, fmt.Errorf("chat: sealed too short")
	}
	enc, mac := chatSealKeys(ksession)
	nonce := chatSealNonce(selector, counter)
	ctLen := len(sealed) - ChatSealTagSize
	h := hmac.New(sha256.New, mac[:])
	h.Write(nonce[:])
	h.Write(sealed[:ctLen])
	if !hmac.Equal(h.Sum(nil)[:ChatSealTagSize], sealed[ctLen:]) {
		return nil, fmt.Errorf("chat: seal auth failed")
	}
	block, err := aes.NewCipher(enc[:])
	if err != nil {
		return nil, err
	}
	pt := make([]byte, ctLen)
	cipher.NewCTR(block, nonce[:]).XORKeyStream(pt, sealed[:ctLen])
	return pt, nil
}

// mtpSessionInfo is the tunnel's key-derivation context. Distinct from
// chatSessionInfo: the two share the cell encoding, never a key space.
const mtpSessionInfo = "thefeed-mtproto-session-v1"

// MTPSessionKey derives the tunnel's per-connection session key from the same
// eph↔ek exchange the messenger uses.
func MTPSessionKey(own *ecdh.PrivateKey, peerPub []byte, protoVer byte, queryKey [KeySize]byte) ([KeySize]byte, error) {
	return sessionKey(own, peerPub, mtpSessionInfo, protoVer, queryKey)
}
