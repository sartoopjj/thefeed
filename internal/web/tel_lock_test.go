package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Binding the device passcode to the Telegram data key.
//
// The machinery for this existed and was tested, but nothing ever called it:
// loadTelCrypto was the only production path and it always built a
// device-mode store, so the passcode protected the lock screen and nothing at
// rest. These tests cover the wiring, not the primitives.

// The one thing that must never break. The session file IS the Telegram
// account: re-wrapping the key under a passcode must keep the SAME key, or
// every existing session and every cached message is orphaned and the user is
// silently signed out by setting a passcode.
func TestBindingAPasscodeKeepsTheDataReadable(t *testing.T) {
	dir := t.TempDir()
	tc, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tc.mode != "device" {
		t.Fatalf("a fresh store is in %q mode, want device", tc.mode)
	}
	sealed, err := tc.seal([]byte("the telegram auth key"))
	if err != nil {
		t.Fatal(err)
	}

	if err := tc.setPassphrase("1234"); err != nil {
		t.Fatal(err)
	}

	// Reloaded from disk the way every caller does, in the same process: the
	// passcode was just typed, so the store must still be usable.
	again, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.locked {
		t.Fatal("setting a passcode locked the store for the rest of the run, " +
			"so the session cannot be read until a restart")
	}
	got, err := again.open(sealed)
	if err != nil || string(got) != "the telegram auth key" {
		t.Fatalf("the session no longer opens after binding the passcode: %q %v", got, err)
	}

	// And the device key is gone, which is the whole point: the files beside
	// the data are no longer enough.
	if _, err := os.Stat(filepath.Join(telCryptoDir(dir), "devicekey")); !os.IsNotExist(err) {
		t.Error("the device key is still on disk, so the passcode protects nothing at rest")
	}
}

// A fresh process (the normal case: app restarted, passcode wrapped) must be
// locked until the passcode arrives, and must not touch the sealed bytes.
func TestAFreshRunIsLockedUntilUnlocked(t *testing.T) {
	dir := t.TempDir()
	tc, _ := loadTelCrypto(dir)
	sealed, _ := tc.seal([]byte("session"))
	if err := tc.setPassphrase("hunter2"); err != nil {
		t.Fatal(err)
	}

	// Forget what this run knows -- that is what a restart is.
	forgetTelKey(telCryptoDir(dir))

	locked, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !locked.locked {
		t.Fatal("a passcode-wrapped store opened without the passcode")
	}
	if _, err := locked.open(sealed); err != errTelLocked {
		t.Errorf("open on a locked store = %v, want errTelLocked -- anything else "+
			"risks writing over the session", err)
	}
	if _, err := locked.seal([]byte("x")); err != errTelLocked {
		t.Errorf("seal on a locked store = %v, want errTelLocked", err)
	}

	// The wrong passcode must not wipe anything.
	if _, err := unlockTelCrypto(dir, "wrong"); err != errBadPassphrase {
		t.Fatalf("unlock with a wrong passcode = %v, want errBadPassphrase", err)
	}
	if _, err := loadTelCrypto(dir); err != nil {
		t.Fatalf("a failed unlock damaged the store: %v", err)
	}

	// The right one opens it, and it STAYS open for the run -- every caller
	// re-loads per use, so an unlock that did not stick would be no unlock.
	if _, err := unlockTelCrypto(dir, "hunter2"); err != nil {
		t.Fatalf("the correct passcode failed: %v", err)
	}
	reopened, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.locked {
		t.Fatal("the store re-locked itself on the next load, so unlocking one " +
			"caller does not unlock the next")
	}
	got, err := reopened.open(sealed)
	if err != nil || string(got) != "session" {
		t.Fatalf("after unlocking, the session does not open: %q %v", got, err)
	}
}

// Removing the passcode must put a device key back and keep the data, or
// turning the lock off signs the user out.
func TestRemovingThePasscodeKeepsTheData(t *testing.T) {
	dir := t.TempDir()
	tc, _ := loadTelCrypto(dir)
	sealed, _ := tc.seal([]byte("still here"))
	if err := tc.setPassphrase("pw"); err != nil {
		t.Fatal(err)
	}
	if err := tc.removePassphrase(); err != nil {
		t.Fatal(err)
	}

	// Forget the in-memory key: device mode must stand on its own file.
	forgetTelKey(telCryptoDir(dir))

	back, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.mode != "device" || back.locked {
		t.Fatalf("after removing the passcode: mode=%q locked=%v", back.mode, back.locked)
	}
	got, err := back.open(sealed)
	if err != nil || string(got) != "still here" {
		t.Fatalf("removing the passcode orphaned the session: %q %v", got, err)
	}
}

// A passcode set before this feature existed has a device-mode store behind
// it. Unlocking must bind it then rather than failing -- otherwise those
// users keep an unwrapped key for ever and the native side sees an error on
// every unlock.
func TestUnlockBindsAStoreThatWasNeverWrapped(t *testing.T) {
	dir := t.TempDir()
	tc, _ := loadTelCrypto(dir)
	sealed, _ := tc.seal([]byte("legacy session"))

	// What handleTelUnlock does for a device-mode store.
	cur, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cur.mode != "device" {
		t.Fatalf("mode = %q", cur.mode)
	}
	if err := cur.setPassphrase("0000"); err != nil {
		t.Fatal(err)
	}

	forgetTelKey(telCryptoDir(dir))
	if _, err := unlockTelCrypto(dir, "0000"); err != nil {
		t.Fatalf("the newly bound passcode does not open it: %v", err)
	}
	after, _ := loadTelCrypto(dir)
	got, err := after.open(sealed)
	if err != nil || string(got) != "legacy session" {
		t.Fatalf("binding an existing store lost the session: %q %v", got, err)
	}
}

// The endpoints the native lock screen actually calls, over HTTP, in the
// order a device goes through them: set a passcode, restart, unlock, remove.
func TestTelLockEndpointsFollowTheDeviceLifecycle(t *testing.T) {
	data := t.TempDir()
	s := &Server{dataDir: data}
	keys := telCryptoDir(s.telDir())

	// Something sealed under the device key, as a signed-in user would have.
	tc, err := loadTelCrypto(s.telDir())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := tc.seal([]byte("auth key"))
	if err != nil {
		t.Fatal(err)
	}

	post := func(h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	// 1. The user sets a passcode.
	if rec := post(s.handleTelLock, "/api/tel/lock", `{"passphrase":"4321"}`); rec.Code != http.StatusOK {
		t.Fatalf("set passcode got %d: %s", rec.Code, rec.Body)
	}
	rec := httptest.NewRecorder()
	s.handleTelLock(rec, httptest.NewRequest(http.MethodGet, "/api/tel/lock", nil))
	if !strings.Contains(rec.Body.String(), `"mode":"passphrase"`) {
		t.Errorf("state after setting a passcode: %s", rec.Body)
	}

	// 2. The app restarts.
	forgetTelKey(keys)
	rec = httptest.NewRecorder()
	s.handleTelLock(rec, httptest.NewRequest(http.MethodGet, "/api/tel/lock", nil))
	if !strings.Contains(rec.Body.String(), `"locked":true`) {
		t.Errorf("a wrapped store did not report itself locked: %s", rec.Body)
	}

	// 3. The wrong passcode is refused and changes nothing.
	if rec := post(s.handleTelUnlock, "/api/tel/unlock", `{"passphrase":"0000"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong passcode got %d, want 401", rec.Code)
	}
	// 4. The right one opens it, and the session is still readable.
	if rec := post(s.handleTelUnlock, "/api/tel/unlock", `{"passphrase":"4321"}`); rec.Code != http.StatusOK {
		t.Fatalf("unlock got %d: %s", rec.Code, rec.Body)
	}
	opened, err := loadTelCrypto(s.telDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := opened.open(sealed); err != nil || string(got) != "auth key" {
		t.Fatalf("after unlocking over HTTP the session does not open: %q %v", got, err)
	}

	// 5. The user removes the passcode: device mode, data intact.
	if rec := post(s.handleTelLockRemove, "/api/tel/lock/remove", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("remove got %d: %s", rec.Code, rec.Body)
	}
	forgetTelKey(keys)
	back, err := loadTelCrypto(s.telDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := back.open(sealed); err != nil || string(got) != "auth key" {
		t.Fatalf("removing the passcode orphaned the session: %q %v", got, err)
	}

	// And an empty passcode is never accepted -- it would wrap the key under
	// nothing and read as success.
	if rec := post(s.handleTelLock, "/api/tel/lock", `{"passphrase":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an empty passcode got %d, want 400", rec.Code)
	}
}
