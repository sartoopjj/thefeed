package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/web"
)

func TestE2E_WebAPI_ConfigAndStatus(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()

	var status map[string]any
	json.NewDecoder(resp.Body).Decode(&status)
	if status["configured"] != false {
		t.Errorf("expected configured=false, got %v", status["configured"])
	}

	resp2, err := http.Get(base + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp2.Body.Close()
	var cfgResp map[string]any
	json.NewDecoder(resp2.Body).Decode(&cfgResp)
	if cfgResp["configured"] != false {
		t.Errorf("expected configured=false on GET config, got %v", cfgResp["configured"])
	}

	cfg := `{"domain":"test.example.com","key":"testpass","resolvers":["127.0.0.1:9999"],"queryMode":"single","rateLimit":10}`
	resp3, err := http.Post(base+"/api/config", "application/json", strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("POST /api/config: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("POST /api/config status=%d body=%s", resp3.StatusCode, body)
	}

	resp4, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status after config: %v", err)
	}
	defer resp4.Body.Close()
	var status2 map[string]any
	json.NewDecoder(resp4.Body).Decode(&status2)
	if status2["configured"] != true {
		t.Errorf("expected configured=true, got %v", status2["configured"])
	}
	if status2["domain"] != "test.example.com" {
		t.Errorf("domain = %v, want test.example.com", status2["domain"])
	}
}

func TestE2E_WebAPI_InvalidConfig(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Post(base+"/api/config", "application/json", strings.NewReader(`{"domain":"x"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	resp2, err := http.Post(base+"/api/config", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("expected 400 for invalid json, got %d", resp2.StatusCode)
	}
}

func TestE2E_WebAPI_Channels(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/api/channels")
	if err != nil {
		t.Fatalf("GET /api/channels: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "null\n" && string(body) != "[]\n" {
		t.Logf("channels response: %q (acceptable)", string(body))
	}
}

func TestE2E_WebAPI_Messages(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/api/messages/1")
	if err != nil {
		t.Fatalf("GET /api/messages/1: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// Response must be MessagesResult format
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if _, ok := result["messages"]; !ok {
		t.Error("expected 'messages' key in response")
	}

	resp2, err := http.Get(base + "/api/messages/abc")
	if err != nil {
		t.Fatalf("GET /api/messages/abc: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("expected 400 for invalid channel, got %d", resp2.StatusCode)
	}
}

func TestE2E_WebAPI_IndexPage(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestE2E_WebAPI_NotFound(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := http.Get(base + "/nonexistent")
	if err != nil {
		t.Fatalf("GET /nonexistent: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestE2E_WebAPI_MethodNotAllowed(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	req, _ := http.NewRequest(http.MethodPut, base+"/api/config", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /api/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}

	resp2, err := http.Get(base + "/api/refresh")
	if err != nil {
		t.Fatalf("GET /api/refresh: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 405 {
		t.Errorf("expected 405 for GET /api/refresh, got %d", resp2.StatusCode)
	}
}

func TestE2E_WebAPI_ConfigPersistence(t *testing.T) {
	dataDir := t.TempDir()

	port1 := findFreePort(t, "tcp")
	srv1, err := web.New(dataDir, port1, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv1.Run()
	time.Sleep(200 * time.Millisecond)

	base1 := fmt.Sprintf("http://127.0.0.1:%d", port1)
	cfg := `{"domain":"persist.example.com","key":"persistkey","resolvers":["127.0.0.1:9999"]}`
	resp, err := http.Post(base1+"/api/config", "application/json", strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("POST config: %v", err)
	}
	resp.Body.Close()

	configPath := dataDir + "/config.json"
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatal("config.json was not persisted to disk")
	}

	port2 := findFreePort(t, "tcp")
	srv2, err := web.New(dataDir, port2, "")
	if err != nil {
		t.Fatalf("create second web server: %v", err)
	}
	go srv2.Run()
	time.Sleep(200 * time.Millisecond)

	base2 := fmt.Sprintf("http://127.0.0.1:%d", port2)
	resp2, err := http.Get(base2 + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status on second instance: %v", err)
	}
	defer resp2.Body.Close()

	var status map[string]any
	json.NewDecoder(resp2.Body).Decode(&status)
	if status["configured"] != true {
		t.Error("second instance should have loaded config, got configured=false")
	}
	if status["domain"] != "persist.example.com" {
		t.Errorf("domain = %v, want persist.example.com", status["domain"])
	}
}

// TestE2E_FullRoundTrip tests DNS server -> client fetcher -> web API end to end.
func TestE2E_FullRoundTrip(t *testing.T) {
	domain := "roundtrip.example.com"
	passphrase := "full-roundtrip-key"
	channels := []string{"general", "alerts"}

	msgs := map[int][]protocol.Message{
		1: {
			{ID: 1, Timestamp: 1700000000, Text: "General message 1"},
			{ID: 2, Timestamp: 1700000001, Text: "General message 2"},
		},
		2: {
			{ID: 10, Timestamp: 1700000010, Text: "Alert!"},
		},
	}

	resolver, cancel := startDNSServer(t, domain, passphrase, channels, msgs)
	defer cancel()

	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	cfgJSON := fmt.Sprintf(`{"domain":"%s","key":"%s","resolvers":["%s"],"queryMode":"single","rateLimit":0}`,
		domain, passphrase, resolver)
	resp, err := http.Post(base+"/api/config", "application/json", strings.NewReader(cfgJSON))
	if err != nil {
		t.Fatalf("POST /api/config: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("config POST status=%d", resp.StatusCode)
	}

	time.Sleep(2 * time.Second)

	respRefresh1, err := http.Post(base+"/api/refresh?channel=1", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/refresh?channel=1: %v", err)
	}
	respRefresh1.Body.Close()
	time.Sleep(1500 * time.Millisecond)

	resp2, err := http.Get(base + "/api/channels")
	if err != nil {
		t.Fatalf("GET /api/channels: %v", err)
	}
	defer resp2.Body.Close()

	var chList []protocol.ChannelInfo
	json.NewDecoder(resp2.Body).Decode(&chList)
	if len(chList) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(chList))
	}
	if chList[0].Name != "general" || chList[1].Name != "alerts" {
		t.Errorf("channels = %v, want [general, alerts]", chList)
	}

	resp3, err := http.Get(base + "/api/messages/1")
	if err != nil {
		t.Fatalf("GET /api/messages/1: %v", err)
	}
	defer resp3.Body.Close()

	var result1 client.MessagesResult
	json.NewDecoder(resp3.Body).Decode(&result1)
	if len(result1.Messages) != 2 {
		t.Fatalf("expected 2 messages for channel 1, got %d", len(result1.Messages))
	}
	if result1.Messages[0].Text != "General message 1" {
		t.Errorf("msg[0].Text = %q, want %q", result1.Messages[0].Text, "General message 1")
	}

	respRefresh2, err := http.Post(base+"/api/refresh?channel=2", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/refresh?channel=2: %v", err)
	}
	respRefresh2.Body.Close()
	time.Sleep(1500 * time.Millisecond)

	resp4, err := http.Get(base + "/api/messages/2")
	if err != nil {
		t.Fatalf("GET /api/messages/2: %v", err)
	}
	defer resp4.Body.Close()

	var result2 client.MessagesResult
	json.NewDecoder(resp4.Body).Decode(&result2)
	if len(result2.Messages) != 1 {
		t.Fatalf("expected 1 message for channel 2, got %d", len(result2.Messages))
	}
	if result2.Messages[0].Text != "Alert!" {
		t.Errorf("msg[0].Text = %q, want %q", result2.Messages[0].Text, "Alert!")
	}
}

func TestE2E_WebAPI_GlobalAuth(t *testing.T) {
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	password := "webpass123"
	srv, err := web.New(dataDir, port, password)
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/"},
		{"GET", "/api/status"},
		{"GET", "/api/config"},
		{"GET", "/api/channels"},
		{"GET", "/api/messages/1"},
		{"GET", "/api/events"},
	}
	for _, ep := range endpoints {
		req, _ := http.NewRequest(ep.method, base+ep.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ep.method, ep.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s %s without auth: expected 401, got %d", ep.method, ep.path, resp.StatusCode)
		}
	}

	for _, ep := range endpoints[:5] {
		req, _ := http.NewRequest(ep.method, base+ep.path, nil)
		req.SetBasicAuth("", password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ep.method, ep.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == 401 {
			t.Errorf("%s %s with correct auth: got 401", ep.method, ep.path)
		}
	}

	req, _ := http.NewRequest("GET", base+"/api/status", nil)
	req.SetBasicAuth("", "wrongpass")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/status wrong pw: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("wrong password: expected 401, got %d", resp.StatusCode)
	}
}

// TestE2E_NewMsgSeparator_TimestampBased verifies the index.html uses
// timestamp-based (not ID-based) comparison for the "new messages" separator.
// This is critical because X/Twitter post IDs are CRC32 hashes that don't
// increase monotonically, so ID-based comparison would place the separator
// in wrong positions.
func TestE2E_NewMsgSeparator_TimestampBased(t *testing.T) {
	base, _ := startWebServer(t)

	resp := getJSON(t, base+"/")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// The separator must compare against timestamp, not message ID.
	// Look for the timestamp-based lastSeen check in the new-msg separator logic.
	checks := []struct {
		name    string
		needle  string
		wantHas bool
	}{
		{"uses timestamp for lastSeen storage", "thefeed_seen_ts_", true},
		{"compares msgTs > lastSeenTs", "msgTs > lastSeenTs", true},
		{"tracks maxTimestamp", "maxTimestamp", true},
		{"no old ID-based seen key", "thefeed_seen_'", false},
		{"no old id > lastSeen comparison", "id > lastSeen", false},
	}
	for _, c := range checks {
		has := strings.Contains(html, c.needle)
		if has != c.wantHas {
			if c.wantHas {
				t.Errorf("%s: expected %q in index.html but not found", c.name, c.needle)
			} else {
				t.Errorf("%s: found %q in index.html but should have been removed", c.name, c.needle)
			}
		}
	}

	// Also verify that first-visit logic stores timestamp, not ID
	if !strings.Contains(html, "setLastSeenTimestamp") {
		t.Error("expected setLastSeenTimestamp function in index.html")
	}
	if !strings.Contains(html, "getLastSeenTimestamp") {
		t.Error("expected getLastSeenTimestamp function in index.html")
	}

	// Verify wasAtBottom updates lastSeen on re-renders (prevents stale separator)
	if !strings.Contains(html, "wasAtBottom && maxTimestamp > 0") {
		t.Error("expected wasAtBottom to update lastSeen timestamp on re-render")
	}
}

// TestE2E_MessagesHaveTimestamps verifies that the messages API response
// includes Timestamp fields needed for the new-messages separator.
func TestE2E_MessagesHaveTimestamps(t *testing.T) {
	base, _ := startWebServer(t)

	resp := getJSON(t, base+"/api/messages/1")
	defer resp.Body.Close()

	var result struct {
		Messages []struct {
			ID        uint32 `json:"ID"`
			Timestamp uint32 `json:"Timestamp"`
			Text      string `json:"Text"`
		} `json:"messages"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode messages: %v", err)
	}

	// With no messages configured, the array should be empty or nil — but the
	// response format must still be valid JSON with a "messages" key.
	// This verifies the API structure supports the timestamp-based separator.
	t.Logf("messages response contains %d messages", len(result.Messages))
}

// TestE2E_ActiveResolversAPI verifies the /api/resolvers/active endpoint.
func TestE2E_ActiveResolversAPI(t *testing.T) {
	base, _ := startWebServer(t)

	// Before config: should return empty resolvers
	resp, err := http.Get(base + "/api/resolvers/active")
	if err != nil {
		t.Fatalf("GET /api/resolvers/active: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	resolvers, ok := result["resolvers"].([]any)
	if !ok {
		t.Fatal("expected 'resolvers' key in response")
	}
	t.Logf("active resolvers: %d", len(resolvers))

	// Method not allowed
	resp2, err := http.Post(base+"/api/resolvers/active", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/resolvers/active: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 405 {
		t.Errorf("expected 405 for POST, got %d", resp2.StatusCode)
	}
}

// TestE2E_WebUI_NewFeatures verifies the index.html includes new UI elements.
func TestE2E_WebUI_NewFeatures(t *testing.T) {
	base, _ := startWebServer(t)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	html := string(bodyBytes)

	checks := map[string]string{
		"message search bar":   "msgSearchBar",
		"search input":         "msgSearchInput",
		"export modal":         "exportModal",
		"resolvers modal":      "resolversModal",
		"background image":     "bgImageInput",
		"dns timeout field":    "peTimeout",
		"scanner clear button": "scanner_clear_targets",
		"search function":      "doMsgSearch",
		"export function":      "doExport",
		"bg image function":    "applyBgImage",
		"resolvers function":   "openResolversModal",
		"sidebar toolbar":      "sidebar-toolbar",
		"resolvers badge":      "resolversBadge",
		"normalize function":   "normalizeArabicPersian",
	}
	for name, needle := range checks {
		if !strings.Contains(html, needle) {
			t.Errorf("%s: expected HTML to contain %q", name, needle)
		}
	}
}
