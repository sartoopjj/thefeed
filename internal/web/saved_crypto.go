package web

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

// Saved Messages at-rest encryption.
//
// The saved.json store and every saved-media blob are sealed with AES-256-GCM
// under a per-store data key (DEK). The DEK is wrapped at rest:
//
//   - device mode (default, transparent): wrapped under a random 32-byte device
//     key stored at <dataDir>/saved/devicekey. Zero friction; protects against
//     raw file-copy / backup exfiltration since the key sits on the device.
//   - passphrase mode (optional, Phase 5): wrapped under an Argon2id-derived key
//     from a user passphrase; the device-key file is removed and the store is
//     locked until unlocked for the session.
//
// We use AES-256-GCM (full 16-byte tag, random nonce) rather than the wire
// SealChat (4-byte tag, tuned for the rate-limited DNS channel) because an
// at-rest blob can be copied and attacked offline without rate limiting.

// sealBytes returns nonce(12) ‖ AES-256-GCM(ciphertext‖tag). A fresh random
// nonce per call, so identical plaintexts seal differently.
func sealBytes(key [32]byte, plaintext []byte) []byte {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err) // a 32-byte key is always valid for AES-256
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		panic(err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil)
}

// openBytes verifies and decrypts a sealBytes blob. Fail-closed.
func openBytes(key [32]byte, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("saved: sealed too short")
	}
	return gcm.Open(nil, sealed[:ns], sealed[ns:], nil)
}

// savedCrypto holds the in-memory DEK for a data dir's Saved store.
type savedCrypto struct {
	dek    [32]byte
	dir    string // <dataDir>/saved
	mode   string // "device" | "passphrase"
	locked bool   // true in passphrase mode until unlocked for the session
}

// savedKeyring is the on-disk wrapped-DEK record.
type savedKeyring struct {
	Mode       string `json:"mode"`           // "device" | "passphrase"
	WrappedDEK []byte `json:"wrappedDek"`     // sealBytes(KEK, dek)
	Salt       []byte `json:"salt,omitempty"` // argon2 salt (passphrase mode)
}

func savedCryptoDir(dataDir string) string { return filepath.Join(dataDir, "saved") }

// savedStoreLooksSealed reports whether saved.json exists and is NOT plaintext
// JSON — i.e. it was sealed with a DEK whose keyring is now missing. Generating
// a fresh keyring/DEK in that state would orphan the data, so the caller locks
// (preserving the bytes) instead. A valid-JSON store is legacy plaintext and is
// safe to migrate; a missing/empty store is a genuine first run.
func savedStoreLooksSealed(dataDir string) bool {
	data, err := os.ReadFile(filepath.Join(dataDir, "saved.json"))
	if err != nil || len(data) == 0 {
		return false
	}
	return !json.Valid(data)
}

func (sc *savedCrypto) keyringPath() string   { return filepath.Join(sc.dir, "keyring.json") }
func (sc *savedCrypto) deviceKeyPath() string { return filepath.Join(sc.dir, "devicekey") }

// errSavedLocked is returned when a mutation is attempted on a locked store
// (passphrase set but not yet unlocked, or device key unreadable). Writing
// would clobber the sealed bytes on disk.
var errSavedLocked = errors.New("saved: store is locked")

// loadSavedCrypto loads or initialises the Saved keyring for a data dir. First
// run generates a DEK + device key (mode "device"). Passphrase mode returns a
// locked instance whose dek is filled in by unlockSavedCrypto.
func loadSavedCrypto(dataDir string) (*savedCrypto, error) {
	sc := &savedCrypto{dir: savedCryptoDir(dataDir), mode: "device"}
	if err := os.MkdirAll(sc.dir, 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(sc.keyringPath())
	if err != nil {
		if !os.IsNotExist(err) {
			// The keyring file is there but unreadable (transient I/O, permission,
			// a partial write). DO NOT initDevice — that overwrites the keyring
			// with a fresh DEK and orphans the sealed store forever. Lock instead;
			// a later clean read recovers it.
			log.Printf("saved: keyring unreadable (%v) — locking to preserve store", err)
			sc.locked = true
			return sc, nil
		}
		// No keyring at all. Normally a genuine first run → init a device key.
		// But if a SEALED saved.json already exists (keyring lost, e.g. an
		// inconsistent OS backup-restore), generating a new DEK would orphan that
		// data. Detect a non-plaintext store and lock instead of destroying it.
		if savedStoreLooksSealed(dataDir) {
			log.Printf("saved: keyring missing but a sealed saved.json exists — locking instead of regenerating the key (data preserved)")
			sc.locked = true
			return sc, nil
		}
		return sc, sc.initDevice() // first run (or legacy plaintext to migrate)
	}
	var kr savedKeyring
	if err := json.Unmarshal(raw, &kr); err != nil {
		return nil, err
	}
	sc.mode = kr.Mode
	if kr.Mode == "passphrase" {
		sc.locked = true
		return sc, nil
	}
	dk, err := os.ReadFile(sc.deviceKeyPath())
	if err != nil || len(dk) != 32 {
		// A keyring exists but the device key is gone/corrupt. Return a LOCKED
		// instance (not an error) so the server treats the sealed store as
		// unreadable rather than clobbering it with a fresh plaintext write.
		sc.locked = true
		return sc, nil
	}
	var kek [32]byte
	copy(kek[:], dk)
	dek, err := openBytes(kek, kr.WrappedDEK)
	if err != nil {
		sc.locked = true
		return sc, nil
	}
	copy(sc.dek[:], dek)
	return sc, nil
}

// initDevice generates a fresh DEK + device key and writes the device-mode
// keyring.
func (sc *savedCrypto) initDevice() error {
	if _, err := rand.Read(sc.dek[:]); err != nil {
		return err
	}
	var kek [32]byte
	if _, err := rand.Read(kek[:]); err != nil {
		return err
	}
	if err := writeKeyFile(sc.deviceKeyPath(), kek[:]); err != nil {
		return err
	}
	sc.mode = "device"
	sc.locked = false
	return sc.writeKeyring(savedKeyring{Mode: "device", WrappedDEK: sealBytes(kek, sc.dek[:])})
}

func (sc *savedCrypto) writeKeyring(kr savedKeyring) error {
	data, err := json.MarshalIndent(kr, "", "  ")
	if err != nil {
		return err
	}
	return writeKeyFile(sc.keyringPath(), data)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeKeyFile is writeFileAtomic for key material, flushed to disk before the
// rename so a crash cannot leave the key file empty. A filesystem that cannot
// flush still gets the write.
func writeKeyFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		_ = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// errBadPassphrase is returned when an unlock attempt fails the GCM tag check.
var errBadPassphrase = errors.New("saved: wrong passphrase")

// deriveKEK derives a 32-byte key-encryption-key from a passphrase using
// Argon2id (OWASP-ish params: 64 MiB, 1 pass, 4 lanes).
func deriveKEK(passphrase string, salt []byte) [32]byte {
	var k [32]byte
	copy(k[:], argon2.IDKey([]byte(passphrase), salt, 1, 64*1024, 4, 32))
	return k
}

// setPassphrase switches the store to passphrase mode: re-wrap the (already
// unlocked) DEK under an Argon2id key and remove the device key. The DEK itself
// is unchanged, so existing sealed data stays readable.
func (sc *savedCrypto) setPassphrase(passphrase string) error {
	if sc.locked {
		return errSavedLocked
	}
	if passphrase == "" {
		return errors.New("saved: empty passphrase")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	kek := deriveKEK(passphrase, salt)
	if err := sc.writeKeyring(savedKeyring{Mode: "passphrase", WrappedDEK: sealBytes(kek, sc.dek[:]), Salt: salt}); err != nil {
		return err
	}
	_ = os.Remove(sc.deviceKeyPath())
	sc.mode = "passphrase"
	return nil
}

// unlockSavedCrypto loads a passphrase-mode keyring and unwraps the DEK with the
// supplied passphrase. Returns errBadPassphrase on a wrong passphrase.
func unlockSavedCrypto(dataDir, passphrase string) (*savedCrypto, error) {
	sc := &savedCrypto{dir: savedCryptoDir(dataDir)}
	raw, err := os.ReadFile(sc.keyringPath())
	if err != nil {
		return nil, err
	}
	var kr savedKeyring
	if err := json.Unmarshal(raw, &kr); err != nil {
		return nil, err
	}
	if kr.Mode != "passphrase" {
		return nil, errors.New("saved: not passphrase-protected")
	}
	kek := deriveKEK(passphrase, kr.Salt)
	dek, err := openBytes(kek, kr.WrappedDEK)
	if err != nil {
		return nil, errBadPassphrase
	}
	sc.mode = "passphrase"
	copy(sc.dek[:], dek)
	sc.locked = false
	return sc, nil
}

// removePassphrase reverts to transparent device mode (regenerates a device key
// and re-wraps the DEK under it). The store must be unlocked first.
func (sc *savedCrypto) removePassphrase() error {
	if sc.locked {
		return errSavedLocked
	}
	// Preserve the existing DEK so sealed data stays readable; just re-wrap.
	var kek [32]byte
	if _, err := rand.Read(kek[:]); err != nil {
		return err
	}
	if err := writeKeyFile(sc.deviceKeyPath(), kek[:]); err != nil {
		return err
	}
	if err := sc.writeKeyring(savedKeyring{Mode: "device", WrappedDEK: sealBytes(kek, sc.dek[:])}); err != nil {
		return err
	}
	sc.mode = "device"
	return nil
}

// resetSaved discards the keyring, device key, sealed store, and saved-media,
// then re-initialises a fresh transparent device-mode store. This is the
// forgotten-passphrase escape hatch — destructive, caller must confirm.
func (sc *savedCrypto) resetSaved(dataDir string) error {
	_ = os.Remove(sc.keyringPath())
	_ = os.Remove(sc.deviceKeyPath())
	_ = os.Remove(filepath.Join(dataDir, "saved.json"))
	_ = os.RemoveAll(filepath.Join(dataDir, "saved-media"))
	_ = os.MkdirAll(filepath.Join(dataDir, "saved-media"), 0o700)
	sc.locked = false
	sc.mode = "device"
	var zero [32]byte
	sc.dek = zero
	return sc.initDevice()
}
