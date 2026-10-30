package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// sessionFor builds a sealed session file over a data dir, the way telHub does.
func sessionFor(t *testing.T, dir string) tgbridge.SessionFile {
	t.Helper()
	tc, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tgbridge.SessionFile{
		Path: filepath.Join(dir, "session.json"),
		Seal: tc.seal, Open: tc.open,
	}
}

func TestSessionRoundTripsSealed(t *testing.T) {
	dir := t.TempDir()
	f := sessionFor(t, dir)
	want := []byte(`{"auth":"secret-key-material"}`)
	if err := f.StoreSession(context.Background(), want); err != nil {
		t.Fatal(err)
	}

	// On disk it must NOT be the plaintext.
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == string(want) {
		t.Fatal("session was written in the clear")
	}

	got, err := sessionFor(t, dir).LoadSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("round trip gave %q, want %q", got, want)
	}
}

// With the key gone in device mode there is nothing to protect: the user holds
// no passcode, so no one can ever decrypt those bytes. Keeping them would brick
// the section for good. Start fresh instead -- the cost is one sign-in.
func TestLostDeviceKeyStartsFreshInsteadOfBricking(t *testing.T) {
	dir := t.TempDir()
	f := sessionFor(t, dir)
	if err := f.StoreSession(context.Background(), []byte(`{"auth":"precious"}`)); err != nil {
		t.Fatal(err)
	}
	cache := loadTelCache(dir)
	cache.PutDialogs([]tgbridge.Dialog{{Peer: tgbridge.Peer{Kind: "channel", ID: 1}}})

	// The key file disappears (a partial restore, say).
	if err := os.Remove(filepath.Join(telCryptoDir(dir), "devicekey")); err != nil {
		t.Fatal(err)
	}

	tc, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tc.locked {
		t.Fatal("device mode must not lock: the user has no way to unlock it")
	}
	// The unreadable data is gone, not left to rot.
	if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
		t.Error("an undecryptable session was kept; the user can never sign in with it")
	}
	if len(loadTelCache(dir).Dialogs()) != 0 {
		t.Error("an undecryptable cache was kept")
	}
	// And the section works again straight away.
	fresh := sessionFor(t, dir)
	if err := fresh.StoreSession(context.Background(), []byte(`{"auth":"new"}`)); err != nil {
		t.Fatalf("the section must be usable after a reset: %v", err)
	}
	got, err := sessionFor(t, dir).LoadSession(context.Background())
	if err != nil || string(got) != `{"auth":"new"}` {
		t.Fatalf("round trip after reset gave %q, %v", got, err)
	}
}

// A passcode-mode store is the opposite case: the user DOES hold the key, so a
// locked store must be preserved, never wiped.
func TestPasscodeModeLocksRatherThanWiping(t *testing.T) {
	dir := t.TempDir()
	f := sessionFor(t, dir)
	want := []byte(`{"auth":"behind-a-passcode"}`)
	if err := f.StoreSession(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	tc, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := tc.setPassphrase("pass"); err != nil {
		t.Fatal(err)
	}

	// A RESTART is what "without the passcode" means. Within the run that set
	// it the key is deliberately still in memory, or turning a passcode on
	// would break the section until the app was restarted.
	forgetTelKey(telCryptoDir(dir))

	locked, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !locked.locked {
		t.Fatal("a passcode store must load locked")
	}
	if _, err := os.Stat(f.Path); err != nil {
		t.Fatal("the session was wiped even though the user can unlock it")
	}
	// A write while locked must be refused rather than clobber it.
	if err := (tgbridge.SessionFile{Path: f.Path, Seal: locked.seal, Open: locked.open}).
		StoreSession(context.Background(), []byte(`{"auth":"other"}`)); err == nil {
		t.Fatal("storing while locked must fail")
	}
	open, err := unlockTelCrypto(dir, "pass")
	if err != nil {
		t.Fatal(err)
	}
	got, err := tgbridge.SessionFile{Path: f.Path, Seal: open.seal, Open: open.open}.
		LoadSession(context.Background())
	if err != nil || string(got) != string(want) {
		t.Fatalf("after unlocking got %q, %v", got, err)
	}
}

// Setting and removing a passcode must keep the SAME key, or every existing
// session and cached message is orphaned the moment the user turns it on.
func TestPasscodeKeepsTheSameKey(t *testing.T) {
	dir := t.TempDir()
	f := sessionFor(t, dir)
	want := []byte(`{"auth":"survives-a-passcode"}`)
	if err := f.StoreSession(context.Background(), want); err != nil {
		t.Fatal(err)
	}

	tc, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := tc.setPassphrase("hunter2"); err != nil {
		t.Fatal(err)
	}
	// The device key must be gone, or the passcode protects nothing.
	if _, err := os.Stat(tc.deviceKeyPath()); !os.IsNotExist(err) {
		t.Fatal("device key survived setting a passcode; the data is still readable without it")
	}

	// Without the passcode -- that is, a fresh run -- locked, and the data
	// untouched. (In the run that set it the key stays in memory on purpose.)
	forgetTelKey(telCryptoDir(dir))

	locked, err := loadTelCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !locked.locked {
		t.Fatal("a passcode-mode keyring must load locked")
	}
	if _, err := unlockTelCrypto(dir, "wrong"); err != errBadPassphrase {
		t.Fatalf("wrong passcode gave %v, want errBadPassphrase", err)
	}

	open, err := unlockTelCrypto(dir, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	got, err := tgbridge.SessionFile{Path: f.Path, Seal: open.seal, Open: open.open}.
		LoadSession(context.Background())
	if err != nil {
		t.Fatalf("the session must survive a passcode being set: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// And removing it must not orphan anything either.
	if err := open.removePassphrase(); err != nil {
		t.Fatal(err)
	}
	back, err := sessionFor(t, dir).LoadSession(context.Background())
	if err != nil {
		t.Fatalf("the session must survive the passcode being removed: %v", err)
	}
	if string(back) != string(want) {
		t.Fatalf("got %q, want %q", back, want)
	}
}

// The Telegram keyring must be independent of Saved: resetting one must not
// take the other's data with it.
func TestTelKeyIsIndependentOfSaved(t *testing.T) {
	dir := t.TempDir()
	f := sessionFor(t, dir)
	want := []byte(`{"auth":"independent"}`)
	if err := f.StoreSession(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	// Saved's whole key directory goes away.
	if err := os.RemoveAll(savedCryptoDir(dir)); err != nil {
		t.Fatal(err)
	}
	got, err := sessionFor(t, dir).LoadSession(context.Background())
	if err != nil {
		t.Fatalf("losing Saved's key must not affect the Telegram session: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Connecting without a session makes gotd mint a fresh auth key and save it,
// so a session file exists while Telegram has never heard of it. Treating that
// file as proof of being signed in dropped the user straight into the chat list
// and then failed with AUTH_KEY_UNREGISTERED.
func TestAFreshKeyIsNotALogin(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	s.tel = newTelHub(s, dir)
	if err := s.saveProfiles(&ProfileList{}); err != nil {
		t.Fatal(err)
	}

	// Exactly what gotd does on a first connect.
	if err := s.tel.sessionFile().StoreSession(context.Background(),
		[]byte(`{"auth_key":"freshly-minted-belongs-to-nobody"}`)); err != nil {
		t.Fatal(err)
	}
	if !s.tel.sessionFile().LoggedIn() {
		t.Fatal("precondition: a file should exist")
	}

	if s.tel.wasAuthorized() {
		t.Fatal("a file nobody confirmed must not count as signed in")
	}
	// And nothing should auto-reconnect on its behalf.
	s.tel.rememberResume("some.server")
	if got := s.tel.resumeServer(); got != "" {
		t.Errorf("auto-connect offered %q for an unconfirmed key", got)
	}

	// Telegram confirms it: now it counts, and keeps counting after a restart.
	s.tel.rememberAuthorized(true)
	if !s.tel.wasAuthorized() {
		t.Fatal("a confirmed login was not remembered")
	}
	fresh := newTelHub(&Server{dataDir: dir}, dir)
	fresh.s = s
	if !fresh.wasAuthorized() {
		t.Error("the confirmation did not survive a restart")
	}

	// Telegram rejects the key: it stops counting.
	s.tel.rememberAuthorized(false)
	if s.tel.wasAuthorized() {
		t.Error("a rejected key still counted as signed in")
	}
}

// The errors that mean "this key belongs to nobody" must be recognised, and
// ordinary failures must not be.
func TestUnregisteredKeyDetection(t *testing.T) {
	for _, e := range []string{
		"rpcDoRequest: rpc error code 401: AUTH_KEY_UNREGISTERED",
		"rpc error code 401: SESSION_REVOKED",
		"rpc error code 401: AUTH_KEY_INVALID",
	} {
		if !isUnregisteredKey(errString(e)) {
			t.Errorf("missed a dead key: %q", e)
		}
	}
	for _, e := range []string{
		"rpc error code 400: PHONE_CODE_INVALID",
		"engine forcibly closed: context canceled",
		"mtproto tunnel: i/o timeout",
	} {
		if isUnregisteredKey(errString(e)) {
			t.Errorf("a recoverable failure was treated as a dead key: %q", e)
		}
	}
}
