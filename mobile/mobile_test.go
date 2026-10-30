package mobile

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// testDir creates a temp directory whose cleanup tolerates background
// goroutines that are still writing files (e.g. cache cleanup).
func testDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "thefeed-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 3 {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
}

func TestNewServerEmptyDir(t *testing.T) {
	if _, err := NewServer("", 0); err == nil {
		t.Errorf("NewServer(\"\") succeeded, want error")
	}
}

func TestServerLifecycle(t *testing.T) {
	dir := testDir(t)
	s, err := NewServer(dir, 0)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Stop()

	if s.Port() <= 0 {
		t.Fatalf("Port() = %d, want > 0", s.Port())
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/api/status", s.Port())
	if _, err := pollGet(url, 5*time.Second); err != nil {
		t.Fatalf("server never came up: %v", err)
	}

	// The API requires the per-launch Basic secret (see Password), so the
	// status is fetched the way the app's own WebView does.
	resp, err := authedGet(s, url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "version") {
		t.Errorf("body = %q, expected to contain 'version'", string(body))
	}
}

func TestStopIsIdempotent(t *testing.T) {
	s, err := NewServer(testDir(t), 0)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.Stop()
	s.Stop() // must not panic
}

func TestStopReleasesPort(t *testing.T) {
	dir := testDir(t)
	s, err := NewServer(dir, 0)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/status", s.Port())
	if _, err := pollGet(url, 3*time.Second); err != nil {
		t.Fatalf("server never came up: %v", err)
	}
	s.Stop()

	// After Stop the listener is closed — a fresh request must fail.
	c := http.Client{Timeout: 500 * time.Millisecond}
	if resp, err := c.Get(url); err == nil {
		resp.Body.Close()
		t.Errorf("server still answering after Stop")
	}
}

// pollGet retries until the server answers at all, since the Serve goroutine
// may take a moment to start accepting on the listener. Any completed exchange
// counts as up, including the 403 an unauthenticated probe now gets -- which is
// exactly what MainActivity's readiness probe does.
func pollGet(url string, total time.Duration) (*http.Response, error) {
	deadline := time.Now().Add(total)
	c := http.Client{Timeout: 500 * time.Millisecond}
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := c.Get(url)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return nil, lastErr
}

// authedGet requests a path the way the app's own WebView does: HTTP Basic
// with the secret this launch generated. Any user name is accepted.
func authedGet(s *Server, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("thefeed", s.Password())
	return (&http.Client{Timeout: 20 * time.Second}).Do(req)
}

// The whole chain, started the way Android and iOS start it.
//
// This is the test that matters: the hole it closes was precisely a missing
// wiring -- web.New(dataDir, port, "127.0.0.1", "") with an EMPTY password,
// started before the lock screen, so any other app on the device could read
// the messenger seed, the feed passphrase and the chat history while the app
// sat locked.
func TestMobileServerRefusesOtherAppsOnTheDevice(t *testing.T) {
	s, err := NewServer(testDir(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	base := fmt.Sprintf("http://127.0.0.1:%d", s.Port())
	if s.Password() == "" {
		t.Fatal("no secret, so the API is open to every app on the device")
	}
	// 32 random bytes, base64url. Short enough to guess is the only way this
	// fails quietly.
	if len(s.Password()) < 40 {
		t.Errorf("secret is %d characters, too short to be unguessable", len(s.Password()))
	}
	if _, err := pollGet(base+"/", 20*time.Second); err != nil {
		t.Fatalf("server never came up: %v", err)
	}

	// Another app: no credentials. These are the three that matter -- the
	// messenger recovery code, the feed passphrase, the chat history.
	for _, path := range []string{"/", "/api/chat/seed", "/api/config", "/api/chat/threads"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("an app with no secret read %s: %d\n%s", path, resp.StatusCode, body)
		}
	}

	// A wrong secret is no better than none.
	req, _ := http.NewRequest(http.MethodGet, base+"/api/chat/seed", nil)
	req.SetBasicAuth("thefeed", s.Password()[:len(s.Password())-1])
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a near-miss secret got %d, want 401", resp.StatusCode)
	}

	// The app's own WebView: in, for the page and for the API.
	resp, err = authedGet(s, base+"/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the app's own first load got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "<html") && !strings.Contains(string(body), "<!DOCTYPE") {
		t.Errorf("the first load did not return the UI (%d bytes)", len(body))
	}
	resp, err = authedGet(s, base+"/api/config")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the app's own API call got %d -- the fix would break the app", resp.StatusCode)
	}

	// The challenge has to ASK for Basic, or neither native auth callback
	// (Android's onReceivedHttpAuthRequest, iOS's didReceive(challenge:))
	// ever fires and the app comes up blank.
	resp, err = http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if a := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(a, "Basic ") {
		t.Errorf("WWW-Authenticate = %q; the native auth callbacks key off this", a)
	}
}

// Two launches must not share a secret, or one leaked once is leaked for good.
func TestEachLaunchGetsItsOwnSecret(t *testing.T) {
	a, err := NewServer(testDir(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	b, err := NewServer(testDir(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if a.Password() == b.Password() {
		t.Fatal("two servers were given the same secret")
	}
}
