package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/web"
)

func TestE2E_Settings_GetDefault(t *testing.T) {
	base, _ := startWebServer(t)

	resp := getJSON(t, base+"/api/settings")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/settings: expected 200, got %d", resp.StatusCode)
	}
	m := decodeJSON(t, resp)
	// Server returns fontSize, debug, version, commit fields.
	if _, ok := m["fontSize"]; !ok {
		t.Errorf("expected 'fontSize' key in settings response; got %v", m)
	}
}

func TestE2E_Settings_SaveAndRead(t *testing.T) {
	base, _ := startWebServer(t)

	body := `{"fontSize":18,"debug":false}`
	resp := postJSON(t, base+"/api/settings", body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/settings: expected 200, got %d", resp.StatusCode)
	}

	resp2 := getJSON(t, base+"/api/settings")
	m := decodeJSON(t, resp2)
	fsz, _ := m["fontSize"].(float64)
	if fsz != 18 {
		t.Errorf("fontSize = %v, want 18", m["fontSize"])
	}
}

func TestE2E_Settings_FontSizeClamped(t *testing.T) {
	base, _ := startWebServer(t)

	resp := postJSON(t, base+"/api/settings", `{"fontSize":999}`)
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		r := getJSON(t, base+"/api/settings")
		m := decodeJSON(t, r)
		fsz, _ := m["fontSize"].(float64)
		if fsz > 24 {
			t.Errorf("fontSize should be clamped to 24, got %v", fsz)
		}
	}
}

func TestE2E_Settings_Persistence(t *testing.T) {
	dataDir := t.TempDir()

	port1 := findFreePort(t, "tcp")
	srv1, err := web.New(dataDir, port1, "127.0.0.1", "")
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	go srv1.Run()
	time.Sleep(200 * time.Millisecond)

	base1 := fmt.Sprintf("http://127.0.0.1:%d", port1)
	resp, err := http.Post(base1+"/api/settings", "application/json", strings.NewReader(`{"fontSize":20,"debug":false}`))
	if err != nil {
		t.Fatalf("POST settings: %v", err)
	}
	resp.Body.Close()

	port2 := findFreePort(t, "tcp")
	srv2, err := web.New(dataDir, port2, "127.0.0.1", "")
	if err != nil {
		t.Fatalf("create second server: %v", err)
	}
	go srv2.Run()
	time.Sleep(200 * time.Millisecond)

	base2 := fmt.Sprintf("http://127.0.0.1:%d", port2)
	resp2, err := http.Get(base2 + "/api/settings")
	if err != nil {
		t.Fatalf("GET settings from second instance: %v", err)
	}
	defer resp2.Body.Close()

	var m map[string]any
	json.NewDecoder(resp2.Body).Decode(&m)
	fsz, _ := m["fontSize"].(float64)
	if fsz != 20 {
		t.Errorf("settings not persisted: fontSize = %v, want 20", m["fontSize"])
	}
}

func TestE2E_Settings_MethodNotAllowed(t *testing.T) {
	base, _ := startWebServer(t)

	req, _ := http.NewRequest(http.MethodDelete, base+"/api/settings", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestE2E_VersionCheck_NoConfig(t *testing.T) {
	base, _ := startWebServer(t)

	resp := postJSON(t, base+"/api/version-check", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("POST /api/version-check: expected 400, got %d", resp.StatusCode)
	}
}

func TestE2E_VersionCheck_MethodNotAllowed(t *testing.T) {
	base, _ := startWebServer(t)

	req, _ := http.NewRequest(http.MethodGet, base+"/api/version-check", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/version-check: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("GET /api/version-check: expected 405, got %d", resp.StatusCode)
	}
}

func TestE2E_VersionCheck_Success(t *testing.T) {
	domain := "test.example.com"
	passphrase := "testpass"
	resolver, feed, cancel := startDNSServerEx(t, domain, passphrase, false, []string{"news"}, map[int][]protocol.Message{})
	defer cancel()
	feed.SetLatestVersion("v9.9.9")

	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "127.0.0.1", "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := fmt.Sprintf(`{"domain":%q,"key":%q,"resolvers":[%q],"queryMode":"single","rateLimit":10}`, domain, passphrase, resolver)
	resp := postJSON(t, base+"/api/config", cfg)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/config status=%d body=%s", resp.StatusCode, body)
	}

	resp2 := postJSON(t, base+"/api/version-check", `{}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("POST /api/version-check status=%d body=%s", resp2.StatusCode, body)
	}
	m := decodeJSON(t, resp2)
	if m["latestVersion"] != "v9.9.9" {
		t.Fatalf("latestVersion = %v, want v9.9.9", m["latestVersion"])
	}

	resp3 := getJSON(t, base+"/api/status")
	defer resp3.Body.Close()
	status := decodeJSON(t, resp3)
	if status["latestVersion"] != "v9.9.9" {
		t.Fatalf("status latestVersion = %v, want v9.9.9", status["latestVersion"])
	}
}

func TestE2E_SettingsPage_HasVersionControls(t *testing.T) {
	base, _ := startWebServer(t)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	html := string(body)
	if !strings.Contains(html, `id="latestVersionEl"`) {
		t.Fatalf("settings page missing latestVersionEl")
	}
	if !strings.Contains(html, `id="checkVersionBtn"`) {
		t.Fatalf("settings page missing checkVersionBtn")
	}
}
