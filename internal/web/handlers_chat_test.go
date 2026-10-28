package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

func newChatTestServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		dataDir: t.TempDir(),
		clients: make(map[chan string]struct{}),
	}
	s.chat = newChatHub(s, s.dataDir)
	return s
}

func TestChatEnableCreatesIdentity(t *testing.T) {
	s := newChatTestServer(t)

	// Fresh hub: info must report exists:false and create NOTHING — creating
	// (and later publishing) keys is an explicit opt-in.
	rec := httptest.NewRecorder()
	s.handleChatInfo(rec, httptest.NewRequest(http.MethodGet, "/api/chat/info", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["exists"] != false {
		t.Fatalf("fresh hub should report exists:false, got %v", resp)
	}
	s.chat.mu.Lock()
	created := s.chat.identity != nil
	s.chat.mu.Unlock()
	if created {
		t.Fatal("handleChatInfo must not create an identity")
	}

	// Availability without an identity tells the UI to prompt, not probe.
	recA := httptest.NewRecorder()
	s.handleChatAvailability(recA, httptest.NewRequest(http.MethodGet, "/api/chat/availability", nil))
	var availResp map[string]any
	if err := json.Unmarshal(recA.Body.Bytes(), &availResp); err != nil {
		t.Fatal(err)
	}
	if availResp["available"] != false || availResp["needsCreate"] != true {
		t.Fatalf("availability without identity: %v", availResp)
	}

	// Explicit opt-in creates the identity.
	recE := httptest.NewRecorder()
	s.handleChatEnable(recE, httptest.NewRequest(http.MethodPost, "/api/chat/enable", strings.NewReader(`{"action":"create"}`)))
	if recE.Code != 200 {
		t.Fatalf("enable create: %d %s", recE.Code, recE.Body.String())
	}
	var en map[string]any
	_ = json.Unmarshal(recE.Body.Bytes(), &en)
	addr, _ := en["address"].(string)
	if len(addr) != 20 {
		t.Fatalf("address = %q", addr)
	}

	// Now info reports the identity, and it persists across a hub reload.
	rec2 := httptest.NewRecorder()
	s.handleChatInfo(rec2, httptest.NewRequest(http.MethodGet, "/api/chat/info", nil))
	var resp2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2["exists"] != true || resp2["address"] != addr {
		t.Fatalf("info after create: %v", resp2)
	}
	if h2 := newChatHub(s, s.dataDir); h2.identity == nil {
		t.Fatal("identity not persisted")
	}
}

// TestChatServerEnableConsent guards the per-server opt-in: a server is not
// enabled (so never polled/registered) until the user turns it on, and the
// choice persists.
func TestChatServerEnableConsent(t *testing.T) {
	s := newChatTestServer(t)
	s.chat.mu.Lock()
	_ = s.chat.ensureIdentityLocked()
	s.chat.mu.Unlock()
	const key = "srv.example.com"

	if s.chat.serverEnabled(key) {
		t.Fatal("server enabled by default")
	}

	// Enable (case-insensitive server key).
	rec := httptest.NewRecorder()
	s.handleChatEnable(rec, httptest.NewRequest(http.MethodPost, "/api/chat/enable",
		strings.NewReader(`{"action":"server","server":"SRV.Example.com","on":true}`)))
	if rec.Code != 200 {
		t.Fatalf("enable server: %d %s", rec.Code, rec.Body.String())
	}
	if !s.chat.serverEnabled(key) {
		t.Fatal("server not enabled after opt-in")
	}
	if h2 := newChatHub(s, s.dataDir); !h2.enabled[key] {
		t.Fatal("enabled set not persisted")
	}

	// Disable.
	rec = httptest.NewRecorder()
	s.handleChatEnable(rec, httptest.NewRequest(http.MethodPost, "/api/chat/enable",
		strings.NewReader(`{"action":"server","server":"srv.example.com","on":false}`)))
	if s.chat.serverEnabled(key) {
		t.Fatal("server still enabled after opt-out")
	}

	// Enabling a server before any identity exists is refused.
	s3 := newChatTestServer(t)
	rec = httptest.NewRecorder()
	s3.handleChatEnable(rec, httptest.NewRequest(http.MethodPost, "/api/chat/enable",
		strings.NewReader(`{"action":"server","server":"srv.example.com","on":true}`)))
	if rec.Code == 200 {
		t.Fatal("server enabled without an identity")
	}
}

// TestChatPostSendServer covers the binding decision after a send: a deliberate
// mid-send switch must stick (no revert to the server we happened to send on),
// while a reroute off a disabled bound server is followed.
func TestChatPostSendServer(t *testing.T) {
	cases := []struct {
		name                           string
		current, boundAtStart, sentVia string
		wantServer                     string
		wantChanged                    bool
	}{
		{"first send binds", "", "", "a.example", "a.example", true},
		{"normal send, unchanged", "a.example", "a.example", "a.example", "a.example", false},
		{"disabled reroute followed", "x.disabled", "x.disabled", "a.example", "a.example", true},
		{"deliberate mid-send switch kept", "b.example", "a.example", "a.example", "b.example", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := chatPostSendServer(c.current, c.boundAtStart, c.sentVia)
			if got != c.wantServer || changed != c.wantChanged {
				t.Fatalf("chatPostSendServer(%q,%q,%q) = (%q,%v), want (%q,%v)",
					c.current, c.boundAtStart, c.sentVia, got, changed, c.wantServer, c.wantChanged)
			}
		})
	}
}

// TestChatInfoEnabledCount checks /api/chat/info reports the enabled-server
// count instantly (no DNS), so a set-up messenger renders its header without the
// "checking…" flash on every open.
func TestChatInfoEnabledCount(t *testing.T) {
	s := newChatTestServer(t)
	s.chat.mu.Lock()
	_ = s.chat.ensureIdentityLocked()
	s.chat.servers = map[string]*perServerChat{
		"a.example.com": {serverKey: "a.example.com"},
		"b.example.com": {serverKey: "b.example.com"},
	}
	s.chat.enabled = map[string]bool{"a.example.com": true}
	s.chat.mu.Unlock()

	rec := httptest.NewRecorder()
	s.handleChatInfo(rec, httptest.NewRequest(http.MethodGet, "/api/chat/info", nil))
	if rec.Code != 200 {
		t.Fatalf("info: %d", rec.Code)
	}
	var resp struct {
		Exists       bool `json:"exists"`
		AnyEnabled   bool `json:"anyEnabled"`
		EnabledCount int  `json:"enabledCount"`
		ServerCount  int  `json:"serverCount"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Exists || !resp.AnyEnabled || resp.EnabledCount != 1 || resp.ServerCount != 2 {
		t.Fatalf("unexpected info: %+v", resp)
	}
}

func TestChatSeedRecoveryRoundTrip(t *testing.T) {
	s := newChatTestServer(t)
	// Create identity (explicit opt-in).
	s.chat.mu.Lock()
	_ = s.chat.ensureIdentityLocked()
	s.chat.mu.Unlock()

	rec := httptest.NewRecorder()
	s.handleChatSeed(rec, httptest.NewRequest(http.MethodGet, "/api/chat/seed", nil))
	var seed struct {
		Recovery string `json:"recovery"`
		BackedUp bool   `json:"backedUp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &seed); err != nil {
		t.Fatal(err)
	}
	if seed.BackedUp || seed.Recovery == "" {
		t.Fatalf("seed resp: %+v", seed)
	}

	// Mark backed up.
	req := httptest.NewRequest(http.MethodPost, "/api/chat/seed", strings.NewReader(`{"action":"backedup"}`))
	s.handleChatSeed(httptest.NewRecorder(), req)
	if !s.chat.backedUp {
		t.Fatal("backedUp not set")
	}

	// Import the same code into a fresh hub → same address.
	origAddr := s.chat.identity.Addr
	s2 := newChatTestServer(t)
	body := `{"action":"import","recovery":"` + seed.Recovery + `"}`
	rec3 := httptest.NewRecorder()
	s2.handleChatSeed(rec3, httptest.NewRequest(http.MethodPost, "/api/chat/seed", strings.NewReader(body)))
	if rec3.Code != 200 {
		t.Fatalf("import failed: %d %s", rec3.Code, rec3.Body.String())
	}
	if s2.chat.identity.Addr != origAddr {
		t.Fatal("imported identity differs")
	}

	// Garbage rejected.
	rec4 := httptest.NewRecorder()
	s2.handleChatSeed(rec4, httptest.NewRequest(http.MethodPost, "/api/chat/seed", strings.NewReader(`{"action":"import","recovery":"zzz"}`)))
	if rec4.Code == 200 {
		t.Fatal("invalid recovery accepted")
	}
}

func TestChatContactsAndThreads(t *testing.T) {
	s := newChatTestServer(t)
	const addr = "aebagbafaydqqcikbmga"

	// Add a contact.
	body := `{"addr":"` + addr + `","name":"mom"}`
	rec := httptest.NewRecorder()
	s.handleChatContacts(rec, httptest.NewRequest(http.MethodPost, "/api/chat/contacts", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("add contact: %d %s", rec.Code, rec.Body.String())
	}

	// Bad address rejected.
	rec = httptest.NewRecorder()
	s.handleChatContacts(rec, httptest.NewRequest(http.MethodPost, "/api/chat/contacts", strings.NewReader(`{"addr":"short","name":"x"}`)))
	if rec.Code == 200 {
		t.Fatal("invalid contact address accepted")
	}

	// Contact shows up in the thread list even without messages.
	rec = httptest.NewRecorder()
	s.handleChatThreads(rec, httptest.NewRequest(http.MethodGet, "/api/chat/threads", nil))
	var threads []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &threads); err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0]["addr"] != addr || threads[0]["name"] != "mom" {
		t.Fatalf("threads: %+v", threads)
	}

	// Contacts persist across hub reload.
	h2 := newChatHub(s, s.dataDir)
	if h2.contacts[addr] != "mom" {
		t.Fatal("contact not persisted")
	}

	// Messages endpoint returns an empty thread (and validates peer).
	rec = httptest.NewRecorder()
	s.handleChatMessages(rec, httptest.NewRequest(http.MethodGet, "/api/chat/messages?peer="+addr, nil))
	var msgs map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatal(err)
	}
	if msgs["peer"] != addr {
		t.Fatalf("messages: %+v", msgs)
	}
	rec = httptest.NewRecorder()
	s.handleChatMessages(rec, httptest.NewRequest(http.MethodGet, "/api/chat/messages?peer=nope", nil))
	if rec.Code == 200 {
		t.Fatal("invalid peer accepted")
	}
}

// TestChatResetNoDeadlock guards the startup hang: reset is called from
// initFetcher while it holds Server.mu (write); reset's rebuild takes
// Server.mu (read) via chatResolvers. Doing that on the caller's goroutine
// deadlocks (RWMutex isn't reentrant). reset must not block under Server.mu.
func TestChatResetNoDeadlock(t *testing.T) {
	s := newChatTestServer(t)
	// An identity must exist so reset's rebuild actually reaches chatResolvers.
	s.chat.mu.Lock()
	_ = s.chat.ensureIdentityLocked()
	s.chat.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.mu.Lock() // mimic initFetcher holding the write lock
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.chat.reset(ctx) // must return immediately, not block on Server.mu
		s.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("chat reset deadlocked while Server.mu was held")
	}
}

func TestChatResolveServerPrecedence(t *testing.T) {
	s := newChatTestServer(t)
	h := s.chat
	h.mu.Lock()
	h.servers = map[string]*perServerChat{
		"a.example.com": {serverKey: "a.example.com"},
		"b.example.com": {serverKey: "b.example.com"},
	}
	h.activeKey = "a.example.com"
	h.enabled = map[string]bool{"a.example.com": true, "b.example.com": true}
	h.threads["addr1"] = &chatThreadFile{Server: "b.example.com"}
	h.mu.Unlock()

	// Requested server wins over everything.
	if ps := h.resolveServer("addr1", "a.example.com"); ps == nil || ps.serverKey != "a.example.com" {
		t.Fatal("requested server should win")
	}
	// No request → the thread's bound server.
	if ps := h.resolveServer("addr1", ""); ps == nil || ps.serverKey != "b.example.com" {
		t.Fatal("thread server should win when none requested")
	}
	// No request, no thread server → the active server.
	if ps := h.resolveServer("addr2", ""); ps == nil || ps.serverKey != "a.example.com" {
		t.Fatal("active server should be the fallback")
	}

	// Disabling the bound server routes the conversation to an enabled one
	// instead of silently continuing on the disabled server.
	h.mu.Lock()
	h.enabled["b.example.com"] = false
	h.mu.Unlock()
	if ps := h.resolveServer("addr1", ""); ps == nil || ps.serverKey != "a.example.com" {
		t.Fatalf("disabled bound server should fall back to an enabled one, got %v", ps)
	}
	// But an explicit pick of the disabled server is still honored (the send
	// path re-enables it).
	if ps := h.resolveServer("addr1", "b.example.com"); ps == nil || ps.serverKey != "b.example.com" {
		t.Fatal("explicit request should win even when disabled")
	}
	// No enabled server at all → nil.
	h.mu.Lock()
	h.enabled = map[string]bool{}
	h.mu.Unlock()
	if ps := h.resolveServer("addr1", ""); ps != nil {
		t.Fatal("expected nil when no server is enabled")
	}

	// No servers at all → nil.
	h.mu.Lock()
	h.servers = map[string]*perServerChat{}
	h.enabled = map[string]bool{"a.example.com": true}
	h.mu.Unlock()
	if ps := h.resolveServer("addr1", ""); ps != nil {
		t.Fatal("expected nil with no servers")
	}
}

// TestChatBackoffConcurrent guards the per-server backoff against the data
// race the review found: pollAllServers + availability probes mutate
// backoffUntil concurrently. Run under -race.
func TestChatBackoffConcurrent(t *testing.T) {
	h := newChatTestServer(t).chat
	ps := &perServerChat{serverKey: "x"}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.setBackoff(ps, time.Now().Add(time.Minute)) }()
		go func() { defer wg.Done(); _ = h.backedOff(ps, time.Now()) }()
	}
	wg.Wait()
}

// TestChatAvailabilitySkipsBackedOffDisabled guards the multi-config fix: a
// DISABLED server that recently failed (e.g. a stale extra config running an
// old/incompatible chat) must be reported from cache, NOT re-probed on every
// availability call. Re-probing dead disabled servers floods the shared
// resolver pool and starves real sends — the exact bug a couple of extra
// configs caused. Enabled servers are always probed for prompt recovery, and a
// manual retry clears every backoff for a fresh sweep.
func TestChatAvailabilitySkipsBackedOffDisabled(t *testing.T) {
	h := newChatTestServer(t).chat
	old := &perServerChat{serverKey: "old.example.com"}
	good := &perServerChat{serverKey: "good.example.com"}
	h.mu.Lock()
	h.servers = map[string]*perServerChat{old.serverKey: old, good.serverKey: good}
	h.enabled = map[string]bool{good.serverKey: true}
	h.mu.Unlock()

	now := time.Now()
	// The old config failed an earlier probe (incompatible version) → parked.
	h.setProbeFailure(old, now.Add(30*time.Minute), "chat_err_version")

	// Disabled + backed off → skipped, reported from cache with its reason.
	if skip, reason := h.shouldSkipProbe(old, h.enabled[old.serverKey], now); !skip || reason != "chat_err_version" {
		t.Fatalf("disabled backed-off server: skip=%v reason=%q, want true/chat_err_version", skip, reason)
	}
	// Enabled server is always probed, even if it were backed off.
	h.setProbeFailure(good, now.Add(30*time.Minute), "chat_err_unreachable")
	if skip, _ := h.shouldSkipProbe(good, h.enabled[good.serverKey], now); skip {
		t.Fatal("enabled server must never be skipped (it needs prompt recovery)")
	}
	// A reachable result (zero backoff) drops the stale reason.
	h.setBackoff(old, time.Time{})
	if backed, reason := h.probeBackoff(old, now); backed || reason != "" {
		t.Fatalf("clearing backoff should clear reason: backed=%v reason=%q", backed, reason)
	}
	// Manual retry wipes every backoff for a fresh full sweep.
	h.setProbeFailure(old, now.Add(30*time.Minute), "chat_err_version")
	h.clearBackoffs()
	if skip, _ := h.shouldSkipProbe(old, false, now); skip {
		t.Fatal("clearBackoffs should re-arm every server for probing")
	}
}

// TestChatReplayWatermark guards #2: an incoming seq at/below the acked
// watermark (per server) is a replay and must be dropped, while a genuinely new
// higher seq passes — and the watermark is per-server so the two never bleed.
func TestChatReplayWatermark(t *testing.T) {
	th := &chatThreadFile{}
	const sv = "a.example.com"

	// Nothing acked yet → nothing is a replay.
	if th.replayedIn(sv, 1) {
		t.Fatal("seq 1 flagged as replay before any ack")
	}
	if !th.markAckedIn(sv, 3) {
		t.Fatal("first ack should move the watermark")
	}
	// Re-ack at/below the mark doesn't move it.
	if th.markAckedIn(sv, 2) || th.markAckedIn(sv, 3) {
		t.Fatal("watermark regressed or re-moved at/below current")
	}
	// seq ≤ 3 are replays; 4+ are new.
	for _, seq := range []uint32{1, 2, 3} {
		if !th.replayedIn(sv, seq) {
			t.Fatalf("seq %d should be a replay (≤ watermark 3)", seq)
		}
	}
	if th.replayedIn(sv, 4) {
		t.Fatal("seq 4 should not be a replay")
	}
	// A different server has its own watermark.
	if th.replayedIn("b.example.com", 1) {
		t.Fatal("watermark leaked across servers")
	}
}

// TestChatSettingsPreset: the cell-size preset is applied, persisted, and read
// back; an unknown preset is rejected.
func TestChatSettingsPreset(t *testing.T) {
	s := newChatTestServer(t)
	if s.chat.budget != protocol.ChatCellPlainSize {
		t.Fatalf("default budget = %d, want Standard", s.chat.budget)
	}

	post := func(body string) int {
		w := httptest.NewRecorder()
		s.handleChatSettings(w, httptest.NewRequest(http.MethodPost, "/api/chat/settings", strings.NewReader(body)))
		return w.Code
	}
	if code := post(`{"preset":"compact"}`); code != 200 {
		t.Fatalf("post compact = %d", code)
	}
	if s.chat.budget != chatBudgetPresets["compact"] {
		t.Fatalf("budget not applied: %d", s.chat.budget)
	}
	// Persisted across a reload.
	if h2 := newChatHub(s, s.dataDir); h2.budget != chatBudgetPresets["compact"] {
		t.Fatalf("budget not persisted: %d", h2.budget)
	}
	// GET reflects it.
	w := httptest.NewRecorder()
	s.handleChatSettings(w, httptest.NewRequest(http.MethodGet, "/api/chat/settings", nil))
	var got struct {
		Budget int    `json:"budget"`
		Preset string `json:"preset"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Preset != "compact" || got.Budget != chatBudgetPresets["compact"] {
		t.Fatalf("get = %+v", got)
	}
	// Unknown preset rejected.
	if code := post(`{"preset":"huge"}`); code != 400 {
		t.Fatalf("unknown preset = %d, want 400", code)
	}
}

func TestChatThreadPinAndDelete(t *testing.T) {
	s := newChatTestServer(t)
	const addr = "aebagbafaydqqcikbmga"

	post := func(action string) int {
		rec := httptest.NewRecorder()
		body := `{"peer":"` + addr + `","action":"` + action + `"}`
		s.handleChatThread(rec, httptest.NewRequest(http.MethodPost, "/api/chat/thread", strings.NewReader(body)))
		return rec.Code
	}

	// Pin creates the thread and marks it pinned (and persists).
	if post("pin") != 200 {
		t.Fatal("pin failed")
	}
	if !s.chat.threads[addr].Pinned {
		t.Fatal("thread not pinned")
	}
	if h2 := newChatHub(s, s.dataDir); h2.threads[addr] == nil || !h2.threads[addr].Pinned {
		t.Fatal("pin not persisted")
	}
	if post("unpin") != 200 || s.chat.threads[addr].Pinned {
		t.Fatal("unpin failed")
	}

	// Delete removes the thread.
	if post("delete") != 200 {
		t.Fatal("delete failed")
	}
	if _, ok := s.chat.threads[addr]; ok {
		t.Fatal("thread not deleted")
	}

	// Bad action / bad peer rejected.
	if post("frobnicate") == 200 {
		t.Fatal("unknown action accepted")
	}
	rec := httptest.NewRecorder()
	s.handleChatThread(rec, httptest.NewRequest(http.MethodPost, "/api/chat/thread", strings.NewReader(`{"peer":"nope","action":"pin"}`)))
	if rec.Code == 200 {
		t.Fatal("invalid peer accepted")
	}
}

// TestChatStatusPerServerSurvivesSwitch guards the cross-server tick bug:
// switching the send server (a status probe there reporting 0/0) must not blank
// the old server's delivered ✓✓, since seq numbering is per server.
func TestChatStatusPerServerSurvivesSwitch(t *testing.T) {
	th := &chatThreadFile{Server: "a.example"}
	th.bumpStatus("a.example", 2, 2) // both messages delivered on server A

	// Switch to server B; its first peer-status probe knows nothing yet.
	if th.bumpStatus("b.example", 0, 0) {
		t.Fatal("zero probe on a new server reported a change")
	}
	if th.Accepted["a.example"] != 2 || th.Delivered["a.example"] != 2 {
		t.Fatalf("server A ticks regressed: acc=%v del=%v", th.Accepted, th.Delivered)
	}

	// A delivery on B is tracked independently of A.
	th.bumpStatus("b.example", 1, 1)
	if th.Delivered["b.example"] != 1 || th.Delivered["a.example"] != 2 {
		t.Fatalf("per-server delivered wrong: %v", th.Delivered)
	}

	// Ticks never regress (a stale lower probe is ignored).
	th.bumpStatus("a.example", 1, 1)
	if th.Delivered["a.example"] != 2 {
		t.Fatalf("delivered regressed to %d", th.Delivered["a.example"])
	}
}

// TestChatStatusMigratesLegacy checks the old single-server acc/del scalars are
// folded into the per-server maps (under the bound server) on load.
func TestChatStatusMigratesLegacy(t *testing.T) {
	s := newChatTestServer(t)
	dir := filepath.Join(s.dataDir, chatDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const addr = "aebagbafaydqqcikbmga"
	legacy := `{"threads":{"` + addr + `":{"server":"a.example","acc":3,"del":2,` +
		`"msgs":[{"dir":"out","seq":1,"text":"hi","server":"a.example"}]}}}`
	if err := os.WriteFile(filepath.Join(dir, "threads.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	h := newChatHub(s, s.dataDir)
	th := h.threads[addr]
	if th == nil {
		t.Fatal("thread not loaded")
	}
	if th.Accepted["a.example"] != 3 || th.Delivered["a.example"] != 2 {
		t.Fatalf("legacy not migrated: acc=%v del=%v", th.Accepted, th.Delivered)
	}
	if th.LastAccepted != 0 || th.LastDelivered != 0 {
		t.Fatalf("legacy scalars not cleared: %d/%d", th.LastAccepted, th.LastDelivered)
	}
}

func TestSameOriginGuard(t *testing.T) {
	called := false
	guarded := sameOriginGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	run := func(method, fetchSite string) (code int, reached bool) {
		called = false
		req := httptest.NewRequest(method, "/api/chat/send", nil)
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code, called
	}

	// Cross-site state-changing request is blocked before reaching the handler.
	if code, reached := run(http.MethodPost, "cross-site"); code != http.StatusForbidden || reached {
		t.Fatalf("cross-site POST: code=%d reached=%v, want 403/false", code, reached)
	}
	// Same-origin / native (no header) writes pass through.
	for _, site := range []string{"same-origin", "same-site", "none", ""} {
		if code, reached := run(http.MethodPost, site); code != http.StatusOK || !reached {
			t.Fatalf("POST Sec-Fetch-Site=%q: code=%d reached=%v, want 200/true", site, code, reached)
		}
	}
	// Reads are never blocked, even cross-site.
	if code, reached := run(http.MethodGet, "cross-site"); code != http.StatusOK || !reached {
		t.Fatalf("cross-site GET: code=%d reached=%v, want 200/true", code, reached)
	}
}

// A signed "this server HAS a messenger" must not be overruled by a probe
// that merely failed to arrive.
//
// EnsureInfo answers "disabled" when the ChatInfo block does not turn up and
// the server has never been confirmed — a guess that a silent server has none.
// On a shutdown-grade link that guess is wrong: the block is late, not absent.
// Reporting it as "disabled" contradicts metadata whose signature was just
// verified, and costs the user a thirty-minute backoff instead of two minutes.
func TestChatUnreachableIsNotReportedAsDisabled(t *testing.T) {
	// The reason shown to the user.
	if got := chatStatusKey(client.ErrChatUnreachable); got != "chat_err_unreachable" {
		t.Fatalf("unreachable maps to %q", got)
	}
	// And the cost of being wrong: a transient failure must not buy the long
	// ceiling that a verified "no messenger" does.
	short := chatServerBackoff(client.ErrChatUnreachable)
	long := chatServerBackoff(client.ErrChatDisabled)
	if short >= long {
		t.Fatalf("unreachable backs off %v, disabled %v — a late block costs as much "+
			"as a server that really has no messenger", short, long)
	}

	// The probe must carry out the substitution when the verified bit says the
	// server advertises chat.
	src, err := os.ReadFile("handlers_chat.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src),
		"if advertised && fresh && errors.Is(err, client.ErrChatDisabled) {") {
		t.Error("the probe no longer prefers the verified metadata bit over a " +
			"ChatInfo fetch that simply did not arrive")
	}
}

// Turning a server on must not report a failure for something that simply has
// not finished.
//
// The first registration on a shutdown-grade link is many round trips from a
// cold cache and regularly outlives its budget, while the retry behind it goes
// on and usually succeeds seconds later. Reporting that as an error told the
// user their server was broken every single time they turned it on.
func TestChatEnableSeparatesTimeoutFromRefusal(t *testing.T) {
	src, err := os.ReadFile("handlers_chat.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, `out["checking"] = true`) {
		t.Fatal("the enable path no longer distinguishes running out of time from " +
			"being refused; a slow first registration reads as a broken server")
	}
	// And the distinction has to be made on the deadline, not guessed.
	if !strings.Contains(body, "sctx.Err() != nil || errors.Is(err, context.DeadlineExceeded)") {
		t.Error("the timeout case is not identified from the deadline")
	}
	// A real refusal must still carry its reason through.
	if !strings.Contains(body, `out["reason"] = chatStatusKey(err)`) {
		t.Error("a genuine refusal no longer reports why")
	}
}

// "Cannot reach the server" must reach the user as exactly that.
//
// Sending with no healthy resolver, or an attempt that runs out of time, both
// mean the link is down — the one failure the user can actually do something
// about. Both used to fall through to the catch-all "something went wrong",
// which says nothing and sends them looking in the wrong place.
func TestChatLinkFailuresSayTheLinkIsDown(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"no resolver to send over", client.ErrChatNoResolvers},
		{"wrapped", fmt.Errorf("send: %w", client.ErrChatNoResolvers)},
		{"ran out of time", context.DeadlineExceeded},
		{"the existing unreachable", client.ErrChatUnreachable},
	}
	for _, tc := range cases {
		if got := chatStatusKey(tc.err); got != "chat_err_unreachable" {
			t.Errorf("%s: reported %q, want chat_err_unreachable", tc.name, got)
		}
	}
	// A genuine server verdict must NOT be swallowed into "unreachable".
	if got := chatStatusKey(client.ErrChatServerDisabled); got != "chat_err_server_disabled" {
		t.Errorf("a real server verdict became %q", got)
	}
}
