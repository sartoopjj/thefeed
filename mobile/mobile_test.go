package mobile

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewServerEmptyDir(t *testing.T) {
	if _, err := NewServer("", 0); err == nil {
		t.Errorf("NewServer(\"\") succeeded, want error")
	}
}

func TestServerLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServer(dir, 0)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Stop()

	if s.Port() <= 0 {
		t.Fatalf("Port() = %d, want > 0", s.Port())
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/api/status", s.Port())
	resp, err := pollGet(url, 5*time.Second)
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
	s, err := NewServer(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.Stop()
	s.Stop() // must not panic
}

func TestStopReleasesPort(t *testing.T) {
	dir := t.TempDir()
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

// pollGet retries until the server is up, since the Serve goroutine
// may take a moment to start accepting on the listener.
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
