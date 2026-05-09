package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestE2E_Scanner_ProgressIdle(t *testing.T) {
	base, _ := startWebServer(t)

	resp := getJSON(t, base+"/api/scanner/progress")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/scanner/progress: expected 200, got %d", resp.StatusCode)
	}
	m := decodeJSON(t, resp)
	if m["state"] != "idle" {
		t.Errorf("state = %v, want idle", m["state"])
	}
}

func TestE2E_Scanner_StartWithoutBody(t *testing.T) {
	base, _ := startWebServer(t)

	resp, err := http.Post(base+"/api/scanner/start", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	// Should fail — no targets.
	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 400, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestE2E_Scanner_StartNoProfile(t *testing.T) {
	base, _ := startWebServer(t)

	body := `{"targets":["192.168.0.0/28"]}`
	resp := postJSON(t, base+"/api/scanner/start", body)
	defer resp.Body.Close()
	// Without any profile configured, should fail.
	if resp.StatusCode != 400 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 400, got %d: %s", resp.StatusCode, string(b))
	}
}

func TestE2E_Scanner_StopIdle(t *testing.T) {
	base, _ := startWebServer(t)

	resp, err := http.Post(base+"/api/scanner/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestE2E_Scanner_PauseIdle(t *testing.T) {
	base, _ := startWebServer(t)

	resp, err := http.Post(base+"/api/scanner/pause", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestE2E_Scanner_ResumeIdle(t *testing.T) {
	base, _ := startWebServer(t)

	resp, err := http.Post(base+"/api/scanner/resume", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestE2E_Scanner_ApplyNoResults(t *testing.T) {
	base, _ := startWebServer(t)

	body := `{"mode":"append"}`
	resp := postJSON(t, base+"/api/scanner/apply", body)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 (no results), got %d", resp.StatusCode)
	}
}

func TestE2E_Scanner_MethodNotAllowed(t *testing.T) {
	base, _ := startWebServer(t)

	// GET on a POST-only endpoint.
	resp, err := http.Get(base + "/api/scanner/start")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestE2E_Scanner_StartWithProfile(t *testing.T) {
	base, srv := startWebServer(t)

	// Create a profile first.
	profileBody := `{"action":"create","profile":{"id":"","nickname":"ScanTest","config":{"domain":"test.example.com","key":"testkey","resolvers":["127.0.0.1:19999"],"queryMode":"single","rateLimit":5}}}`
	resp := postJSON(t, base+"/api/profiles", profileBody)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create profile: expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	// Start a scan with non-routable IPs (will time out quickly).
	scanBody := `{"targets":["192.0.2.1","192.0.2.2"],"timeout":1,"rateLimit":2}`
	resp2 := postJSON(t, base+"/api/scanner/start", scanBody)
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("start scanner: expected 200, got %d: %s", resp2.StatusCode, string(b))
	}

	// Check progress.
	time.Sleep(500 * time.Millisecond)
	resp3 := getJSON(t, base+"/api/scanner/progress")
	m := decodeJSON(t, resp3)
	state := m["state"].(string)
	if state != "running" && state != "done" {
		t.Errorf("state = %s, want running or done", state)
	}

	// Wait for completion.
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		resp4 := getJSON(t, base+"/api/scanner/progress")
		m2 := decodeJSON(t, resp4)
		if m2["state"].(string) == "done" || m2["state"].(string) == "idle" {
			break
		}
	}

	_ = srv
}

func TestE2E_Scanner_Presets(t *testing.T) {
	base, _ := startWebServer(t)

	resp := getJSON(t, base+"/api/scanner/presets")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/scanner/presets: expected 200, got %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var data struct {
		Presets []struct {
			Name  string `json:"name"`
			Label string `json:"label"`
			Count int    `json:"count"`
		} `json:"presets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(data.Presets) == 0 {
		t.Error("expected non-empty presets list")
	}
	if data.Presets[0].Name != "default" {
		t.Errorf("first preset name = %q, want default", data.Presets[0].Name)
	}
	if data.Presets[0].Count == 0 {
		t.Error("expected non-zero count for default preset")
	}
}
