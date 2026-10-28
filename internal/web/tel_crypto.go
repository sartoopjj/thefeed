package web

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
)

// Telegram at-rest encryption.
//
// The MTProto session and, later, the message cache are sealed with
// AES-256-GCM under their own data key, reusing the Saved primitives
// (sealBytes / openBytes / deriveKEK). The DEK is wrapped exactly the way
// Saved's is:
//
//   - device mode (default): wrapped under a random key at <dataDir>/tel/devicekey.
//   - passphrase mode: wrapped under an Argon2id key from the user's passcode,
//     with the device-key file removed, so the data is useless without it.
//
// A SEPARATE keyring from Saved on purpose. One passphrase can drive both, but
// resetting or regenerating one store's key must never orphan the other -- and
// session.json is the one file here that cannot be rebuilt: losing it is a
// silent logout.
type telCrypto struct {
	dek    [32]byte
	dir    string
	mode   string // "device" | "passphrase"
	locked bool
}

// telCryptoDir holds the keyring, inside the Telegram data dir.
func telCryptoDir(dir string) string { return filepath.Join(dir, "keys") }

func (tc *telCrypto) keyringPath() string   { return filepath.Join(tc.dir, "keyring.json") }
func (tc *telCrypto) deviceKeyPath() string { return filepath.Join(tc.dir, "devicekey") }

// errTelLocked means the key is unavailable, so sealed bytes must be left
// alone rather than overwritten. Only ever used in passcode mode, where the
// user holds the means to unlock.
var errTelLocked = errors.New("tel: store is locked")

// resetSealed deletes data nobody can ever read again.
//
// Locking only helps when SOMEONE can still unlock. In device mode the key is
// a file beside the data and the user has nothing to type: if it is gone, the
// session and cache are unrecoverable by anyone, and keeping them would brick
// the section for ever instead of costing one sign-in. Passcode mode is the
// opposite -- the user does hold the key -- so that path locks instead.
func resetSealed(dir string) {
	for _, p := range []string{
		filepath.Join(dir, "session.json"),
		telCachePath(dir),
	} {
		if err := os.Remove(p); err == nil {
			log.Printf("tel: %s was sealed under a key that is gone — removed so the app can start fresh", filepath.Base(p))
		}
	}
}

// loadTelCrypto loads or initialises the Telegram keyring. A first run makes a
// DEK and device key; anything unreadable locks instead of regenerating.
func loadTelCrypto(dir string) (*telCrypto, error) {
	tc := &telCrypto{dir: telCryptoDir(dir), mode: "device"}
	if err := os.MkdirAll(tc.dir, 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(tc.keyringPath())
	if err != nil {
		if !os.IsNotExist(err) {
			// Present but unreadable: a transient I/O or permission problem,
			// where the bytes may well come back on the next read. This one
			// DOES lock -- re-initialising on a passing error would throw away
			// a session that was never really lost.
			log.Printf("tel: keyring unreadable (%v) — leaving the session alone for now", err)
			tc.locked = true
			return tc, nil
		}
		// No keyring at all, so anything already here is sealed under a key
		// that no longer exists. Nothing can decrypt it, now or later.
		resetSealed(dir)
		return tc, tc.initDevice()
	}

	var kr savedKeyring
	if err := json.Unmarshal(raw, &kr); err != nil {
		return nil, err
	}
	tc.mode = kr.Mode
	if kr.Mode == "passphrase" {
		tc.locked = true // filled in by unlockTelCrypto
		return tc, nil
	}
	dk, err := os.ReadFile(tc.deviceKeyPath())
	if err != nil || len(dk) != 32 {
		// Device mode with no device key. Unrecoverable: start over rather
		// than leave the user with a section that can never open again.
		log.Printf("tel: device key missing — starting fresh")
		resetSealed(dir)
		return tc, tc.initDevice()
	}
	var kek [32]byte
	copy(kek[:], dk)
	dek, err := openBytes(kek, kr.WrappedDEK)
	if err != nil {
		log.Printf("tel: device key does not open the keyring — starting fresh")
		resetSealed(dir)
		return tc, tc.initDevice()
	}
	copy(tc.dek[:], dek)
	return tc, nil
}

func (tc *telCrypto) initDevice() error {
	if _, err := rand.Read(tc.dek[:]); err != nil {
		return err
	}
	var kek [32]byte
	if _, err := rand.Read(kek[:]); err != nil {
		return err
	}
	if err := writeFileAtomic(tc.deviceKeyPath(), kek[:], 0o600); err != nil {
		return err
	}
	tc.mode = "device"
	tc.locked = false
	return tc.writeKeyring(savedKeyring{Mode: "device", WrappedDEK: sealBytes(kek, tc.dek[:])})
}

func (tc *telCrypto) writeKeyring(kr savedKeyring) error {
	data, err := json.MarshalIndent(kr, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(tc.keyringPath(), data, 0o600)
}

// setPassphrase rewraps the SAME DEK under a passcode-derived key and removes
// the device key. The DEK is never regenerated, so the sealed session survives.
func (tc *telCrypto) setPassphrase(passphrase string) error {
	if tc.locked {
		return errTelLocked
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	kek := deriveKEK(passphrase, salt)
	if err := tc.writeKeyring(savedKeyring{
		Mode: "passphrase", WrappedDEK: sealBytes(kek, tc.dek[:]), Salt: salt,
	}); err != nil {
		return err
	}
	// Only after the keyring is safely on disk: until then the device key is
	// the only way back to the data.
	_ = os.Remove(tc.deviceKeyPath())
	tc.mode = "passphrase"
	return nil
}

// unlockTelCrypto opens a passphrase-mode keyring for this run.
func unlockTelCrypto(dir, passphrase string) (*telCrypto, error) {
	tc := &telCrypto{dir: telCryptoDir(dir), mode: "passphrase", locked: true}
	raw, err := os.ReadFile(tc.keyringPath())
	if err != nil {
		return nil, err
	}
	var kr savedKeyring
	if err := json.Unmarshal(raw, &kr); err != nil {
		return nil, err
	}
	if kr.Mode != "passphrase" {
		return nil, errors.New("tel: no passphrase set")
	}
	dek, err := openBytes(deriveKEK(passphrase, kr.Salt), kr.WrappedDEK)
	if err != nil {
		return nil, errBadPassphrase
	}
	copy(tc.dek[:], dek)
	tc.locked = false
	return tc, nil
}

// removePassphrase goes back to device mode, keeping the same DEK.
func (tc *telCrypto) removePassphrase() error {
	if tc.locked {
		return errTelLocked
	}
	var kek [32]byte
	if _, err := rand.Read(kek[:]); err != nil {
		return err
	}
	if err := writeFileAtomic(tc.deviceKeyPath(), kek[:], 0o600); err != nil {
		return err
	}
	if err := tc.writeKeyring(savedKeyring{Mode: "device", WrappedDEK: sealBytes(kek, tc.dek[:])}); err != nil {
		return err
	}
	tc.mode = "device"
	return nil
}

// seal/open are what the session file and the cache use. A locked store seals
// nothing, so callers must check: writing plaintext over sealed bytes would
// undo the encryption, and writing under a wrong key would orphan them.
func (tc *telCrypto) seal(plain []byte) ([]byte, error) {
	if tc == nil || tc.locked {
		return nil, errTelLocked
	}
	return sealBytes(tc.dek, plain), nil
}

func (tc *telCrypto) open(sealed []byte) ([]byte, error) {
	if tc == nil || tc.locked {
		return nil, errTelLocked
	}
	return openBytes(tc.dek, sealed)
}
