package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func signOut(t *testing.T, s *Server) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleTelAuth(rec, httptest.NewRequest(http.MethodPost, "/api/tel/auth", strings.NewReader(`{"action":"logout"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body.String())
	}
}

func onDisk(h *telHub) (string, error) {
	b, err := h.sessionFile().LoadSession(context.Background())
	return string(b), err
}

// The connection's session handle is the guarded one.
func TestConnectionsGetTheGuardedSessionHandle(t *testing.T) {
	src, err := os.ReadFile("handlers_tel.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "Session:   h.bridgeSession(),") || strings.Contains(string(src), "Session:   h.sessionFile(),") {
		t.Fatal("a connection is not given bridgeSession()")
	}
}

// Sign in and out, over and over, with every old connection still saving
// after each sign-out: only the current sign-in is ever on disk.
func TestSignInAndOutRepeatedly(t *testing.T) {
	s := newTelTestServer(t)
	h := s.tel
	var old []interface {
		StoreSession(context.Context, []byte) error
	}
	for round := 0; round < 5; round++ {
		live := h.bridgeSession()
		key := fmt.Sprintf(`{"auth":"login-%d"}`, round)
		if err := live.StoreSession(context.Background(), []byte(key)); err != nil {
			t.Fatal(err)
		}
		if got, err := onDisk(h); err != nil || got != key {
			t.Fatalf("round %d: sign-in not kept: %q %v", round, got, err)
		}
		old = append(old, live)
		signOut(t, s)
		signOut(t, s)             // tapped twice
		for i := 0; i < 20; i++ { // every old connection reconnects, again and again
			for _, o := range old {
				if err := o.StoreSession(context.Background(), []byte(`{"auth":"stale"}`)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := os.Stat(filepath.Join(h.dir, "session.json")); !os.IsNotExist(err) {
			t.Fatalf("round %d: a key is on disk after sign-out", round)
		}
	}
}

// Two sign-outs at once, while the connection keeps saving.
func TestSignOutRacesWithSaves(t *testing.T) {
	for round := 0; round < 20; round++ {
		s := newTelTestServer(t)
		h := s.tel
		live := h.bridgeSession()
		live.StoreSession(context.Background(), []byte(`{"auth":"k"}`))
		var wg sync.WaitGroup
		stop := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					live.StoreSession(context.Background(), []byte(`{"auth":"k"}`))
				}
			}
		}()
		var outs sync.WaitGroup
		for i := 0; i < 2; i++ {
			outs.Add(1)
			go func() {
				defer outs.Done()
				rec := httptest.NewRecorder()
				s.handleTelAuth(rec, httptest.NewRequest(http.MethodPost, "/api/tel/auth", strings.NewReader(`{"action":"logout"}`)))
				if rec.Code != http.StatusOK {
					t.Errorf("logout: %d", rec.Code)
				}
			}()
		}
		outs.Wait()
		close(stop)
		wg.Wait()
		if _, err := os.Stat(filepath.Join(h.dir, "session.json")); !os.IsNotExist(err) {
			t.Fatalf("round %d: the key survived a sign-out made during saves", round)
		}
	}
}

// The keyring breaks, recovers, and breaks again: each time the user can sign
// in and nothing is ever written in the clear.
func TestKeyringBreaksRepeatedly(t *testing.T) {
	h := newTelTestServer(t).tel
	kr := filepath.Join(telCryptoDir(h.dir), "keyring.json")
	for round := 0; round < 4; round++ {
		key := fmt.Sprintf(`{"auth":"secret-%d"}`, round)
		if err := h.bridgeSession().StoreSession(context.Background(), []byte(key)); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		raw, _ := os.ReadFile(filepath.Join(h.dir, "session.json"))
		if strings.Contains(string(raw), "secret-") {
			t.Fatalf("round %d: written in the clear", round)
		}
		if got, err := onDisk(h); err != nil || got != key {
			t.Fatalf("round %d: %q %v", round, got, err)
		}
		os.WriteFile(kr, []byte(`{"mode":"dev`), 0o600) // cut short again
	}
}

// Telegram rejects the key several times in a row on one connection; each
// sign-in on it is kept.
func TestUnregisteredKeyRepeatedly(t *testing.T) {
	h := newTelTestServer(t).tel
	live := h.bridgeSession()
	for round := 0; round < 5; round++ {
		if err := h.wipeSession(); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf(`{"auth":"signed-in-%d"}`, round)
		live.StoreSession(context.Background(), []byte(key))
		if got, err := onDisk(h); err != nil || got != key {
			t.Fatalf("round %d: lost: %q %v", round, got, err)
		}
	}
}

// Leftovers from writes cut short (crashes) do not get in the way, however
// many there were.
func TestLeftoverTempFilesAfterCrashes(t *testing.T) {
	h := newTelTestServer(t).tel
	if err := h.bridgeSession().StoreSession(context.Background(), []byte(`{"auth":"k1"}`)); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		os.WriteFile(filepath.Join(h.dir, "session.json.tmp"), []byte("half"), 0o600)
		os.WriteFile(filepath.Join(telCryptoDir(h.dir), "keyring.json.tmp"), []byte("{"), 0o600)
		if got, err := onDisk(h); err != nil || !strings.HasPrefix(got, `{"auth":"k`) {
			t.Fatalf("round %d: %q %v", round, got, err)
		}
		key := fmt.Sprintf(`{"auth":"k%d"}`, round+2)
		if err := h.bridgeSession().StoreSession(context.Background(), []byte(key)); err != nil {
			t.Fatal(err)
		}
		if got, err := onDisk(h); err != nil || got != key {
			t.Fatalf("round %d: %q %v", round, got, err)
		}
	}
}
