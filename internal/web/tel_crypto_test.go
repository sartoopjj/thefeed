package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/session"
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

func TestTelCryptoConcurrentFirstUse(t *testing.T) {
	run := func(t *testing.T, dirs ...string) {
		t.Helper()
		type result struct {
			dir int
			tc  *telCrypto
			err error
		}
		start := make(chan struct{})
		results := make(chan result, 16*len(dirs))
		var workers sync.WaitGroup
		for i, dir := range dirs {
			for range 16 {
				workers.Go(func() {
					<-start
					tc, err := loadTelCrypto(dir)
					results <- result{dir: i, tc: tc, err: err}
				})
			}
		}
		close(start)
		workers.Wait()
		close(results)

		handles := make([][]*telCrypto, len(dirs))
		for r := range results {
			if r.err != nil {
				t.Fatalf("directory %d: concurrent first load: %v", r.dir, r.err)
			}
			if r.tc == nil || r.tc.locked {
				t.Fatalf("directory %d: concurrent first load did not return an unlocked handle", r.dir)
			}
			handles[r.dir] = append(handles[r.dir], r.tc)
		}
		for i, dir := range dirs {
			first := handles[i][0]
			plain := []byte("synthetic concurrent session")
			sealed, err := first.seal(plain)
			if err != nil {
				t.Fatal(err)
			}
			for j, tc := range handles[i] {
				if tc.dek != first.dek {
					t.Fatalf("directory %d: worker %d holds a different DEK", i, j)
				}
				if got, err := tc.open(sealed); err != nil || string(got) != string(plain) {
					t.Fatalf("directory %d: worker %d cannot open the original ciphertext: %v", i, j, err)
				}
			}
			again, err := loadTelCrypto(dir)
			if err != nil {
				t.Fatal(err)
			}
			if again.locked || again.dek != first.dek {
				t.Fatalf("directory %d: final reload did not retain the original DEK", i)
			}
		}
		if len(dirs) == 2 && handles[0][0].dek == handles[1][0].dek {
			t.Fatal("distinct directories share a DEK")
		}
	}

	for round := range 4 {
		t.Run(fmt.Sprintf("round_%d", round), func(t *testing.T) {
			run(t, t.TempDir())
		})
	}
	t.Run("independent_directories", func(t *testing.T) {
		run(t, t.TempDir(), t.TempDir())
	})
}

func TestTelCryptoLifecycleUsesSharedLock(t *testing.T) {
	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "load", mode: "device"},
		{name: "set", mode: "passphrase"},
		{name: "unlock", mode: "passphrase"},
		{name: "remove", mode: "device"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Cleanup(func() { forgetTelKey(telCryptoDir(dir)) })
			tc, err := loadTelCrypto(dir)
			if err != nil {
				t.Fatal(err)
			}
			dek := tc.dek
			fixtures := []struct {
				name   string
				plain  string
				sealed []byte
			}{
				{name: "session.json", plain: "synthetic lifecycle session"},
				{name: "cache.bin", plain: "synthetic lifecycle cache"},
			}
			for i := range fixtures {
				f := &fixtures[i]
				f.sealed, err = tc.seal([]byte(f.plain))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, f.name), f.sealed, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "unlock" || test.name == "remove" {
				if err := tc.setPassphrase("synthetic lifecycle passphrase"); err != nil {
					t.Fatal(err)
				}
				if test.name == "unlock" {
					forgetTelKey(tc.dir)
				}
			}
			var deviceKey []byte
			if test.name == "load" {
				deviceKey, err = os.ReadFile(tc.deviceKeyPath())
				if err != nil {
					t.Fatal(err)
				}
			}

			telCryptoMu.Lock()
			held := true
			release := func() {
				if !held {
					return
				}
				if test.name == "load" {
					if err := os.WriteFile(tc.deviceKeyPath(), deviceKey, 0o600); err != nil {
						t.Errorf("restore device key before releasing transaction: %v", err)
					}
				}
				held = false
				telCryptoMu.Unlock()
			}
			defer release()
			if test.name == "load" {
				if err := os.Remove(tc.deviceKeyPath()); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				tc  *telCrypto
				err error
			}
			started := make(chan struct{})
			done := make(chan result, 1)
			go func() {
				close(started)
				got := tc
				var err error
				switch test.name {
				case "load":
					got, err = loadTelCrypto(dir)
				case "set":
					err = got.setPassphrase("synthetic lifecycle passphrase")
				case "unlock":
					got, err = unlockTelCrypto(dir, "synthetic lifecycle passphrase")
				case "remove":
					err = got.removePassphrase()
				}
				done <- result{tc: got, err: err}
			}()
			<-started
			select {
			case <-done:
				t.Fatal("lifecycle operation completed while the shared lock was held")
			case <-time.After(100 * time.Millisecond):
			}
			release()

			var got *telCrypto
			select {
			case r := <-done:
				if r.err != nil {
					t.Fatal(r.err)
				}
				got = r.tc
			case <-time.After(10 * time.Second):
				t.Fatal("lifecycle operation deadlocked after releasing the shared lock")
			}
			if got == nil || got.locked || got.mode != test.mode || got.dek != dek {
				t.Fatal("lifecycle operation did not preserve the unlocked DEK and expected mode")
			}
			if test.name == "remove" {
				forgetTelKey(tc.dir)
			}
			again, err := loadTelCrypto(dir)
			if err != nil {
				t.Fatal(err)
			}
			if again.locked || again.mode != test.mode || again.dek != dek {
				t.Fatal("fresh load did not retain the unlocked DEK and expected mode")
			}
			for _, f := range fixtures {
				raw, err := os.ReadFile(filepath.Join(dir, f.name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(raw, f.sealed) {
					t.Fatalf("%s changed during the lifecycle operation", f.name)
				}
				if plain, err := got.open(raw); err != nil || string(plain) != f.plain {
					t.Fatalf("%s is unreadable after the lifecycle operation: %v", f.name, err)
				}
			}
		})
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

// A keyring cut short (a crash mid-write) holds no key any more: as with a
// missing one, start fresh and keep sealing.
func TestTruncatedKeyringStillSeals(t *testing.T) {
	h := newTelTestServer(t).tel
	secret := []byte(`{"auth":"secret-key-material"}`)
	if err := h.sessionFile().StoreSession(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(telCryptoDir(h.dir), "keyring.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// What nothing can open any more is gone, so the app offers a sign-in.
	if _, err := h.sessionFile().LoadSession(context.Background()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("after the damage: %v, want not-found", err)
	}
	if err := h.sessionFile().StoreSession(context.Background(), secret); err != nil {
		t.Fatalf("no way back after a broken keyring: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(h.dir, "session.json"))
	if strings.Contains(string(raw), "secret-key-material") {
		t.Fatal("session written in the clear")
	}
	got, err := h.sessionFile().LoadSession(context.Background())
	if err != nil || string(got) != string(secret) {
		t.Fatalf("round trip gave %q, %v", got, err)
	}
}

// With no usable key at all, nothing is written.
func TestNoKeyNoSessionWritten(t *testing.T) {
	h := newTelTestServer(t).tel
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(telCryptoDir(h.dir), nil, 0o600); err != nil { // the keys dir cannot be made
		t.Fatal(err)
	}
	if err := h.sessionFile().StoreSession(context.Background(), []byte(`{"auth":"x"}`)); err == nil {
		t.Fatal("stored without a key")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "session.json")); !os.IsNotExist(err) {
		t.Fatal("a session file was written without a key")
	}
}

// A connection still holds the key after the user signs out and saves it again
// on every reconnect: none of that may reach the disk. A new sign-in may.
func TestSignOutStopsTheConnectionsSessionHandle(t *testing.T) {
	s := newTelTestServer(t)
	h := s.tel
	held := h.bridgeSession()
	if err := held.StoreSession(context.Background(), []byte(`{"auth":"account-key"}`)); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleTelAuth(rec, httptest.NewRequest(http.MethodPost, "/api/tel/auth", strings.NewReader(`{"action":"logout"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	if err := held.StoreSession(context.Background(), []byte(`{"auth":"account-key"}`)); err != nil {
		t.Fatalf("a refused save must still report success, or the connection drops: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "session.json")); !os.IsNotExist(err) {
		t.Fatal("the key came back after sign-out")
	}
	next := h.bridgeSession()
	if err := next.StoreSession(context.Background(), []byte(`{"auth":"new-login"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := h.sessionFile().LoadSession(context.Background())
	if err != nil || string(got) != `{"auth":"new-login"}` {
		t.Fatalf("a sign-in after sign-out was not kept: %q %v", got, err)
	}
}

// Dropping a key Telegram does not know keeps the connection: the user signs
// in on it, and that sign-in must be saved.
func TestUnregisteredKeyWipeKeepsTheLiveHandle(t *testing.T) {
	h := newTelTestServer(t).tel
	held := h.bridgeSession()
	held.StoreSession(context.Background(), []byte(`{"auth":"unregistered"}`))
	if err := h.wipeSession(); err != nil {
		t.Fatal(err)
	}
	if err := held.StoreSession(context.Background(), []byte(`{"auth":"signed-in"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := h.sessionFile().LoadSession(context.Background())
	if err != nil || string(got) != `{"auth":"signed-in"}` {
		t.Fatalf("the sign-in on the same connection was lost: %q %v", got, err)
	}
}
