package web

import (
	"context"
	"errors"
	"fmt"

	"encoding/json"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tgerr"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

func newTelTestServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{dataDir: t.TempDir(), clients: make(map[chan string]struct{})}
	s.tel = newTelHub(s, s.dataDir)
	return s
}

// Opening the tab must cost nothing: no probing, just the signed capability
// bit the feed already cached.
func TestTelInfoReportsStateWithoutProbing(t *testing.T) {
	s := newTelTestServer(t)
	rec := httptest.NewRecorder()
	s.handleTelInfo(rec, httptest.NewRequest(http.MethodGet, "/api/tel/info", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var resp struct {
		HasSession bool `json:"hasSession"`
		Connected  bool `json:"connected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.HasSession || resp.Connected {
		t.Fatalf("fresh hub should be idle: %+v", resp)
	}
}

// telJSONReq builds a POST with a JSON body.
func telJSONReq(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// Every call that needs Telegram must fail cleanly when nothing is connected,
// rather than hanging or panicking.
func TestTelCallsRequireConnection(t *testing.T) {
	s := newTelTestServer(t)
	for _, tc := range []struct {
		name    string
		req     *http.Request
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"dialogs", httptest.NewRequest(http.MethodGet, "/api/tel/dialogs", nil), s.handleTelDialogs},
		{"history", httptest.NewRequest(http.MethodGet, "/api/tel/history?kind=user&id=5", nil), s.handleTelHistory},
		{"send", telJSONReq("/api/tel/send", `{"peer":{"kind":"user","id":"5","accessHash":"0"},"text":"hi"}`), s.handleTelSend},
		{"delete", telJSONReq("/api/tel/delete", `{"peer":{"kind":"user","id":"5","accessHash":"0"}}`), s.handleTelDelete},
		{"resolve", telJSONReq("/api/tel/resolve", `{"username":"someone"}`), s.handleTelResolve},
		{"read", telJSONReq("/api/tel/read", `{"peer":{"kind":"user","id":"5","accessHash":"0"},"maxId":3}`), s.handleTelRead},
	} {
		rec := httptest.NewRecorder()
		tc.handler(rec, tc.req)
		var resp struct {
			OK bool `json:"ok"`
			// The UI shows a translated key, so that -- not an English
			// sentence -- is what every failure has to carry.
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if resp.OK || resp.Reason != "tel_err_not_connected" {
			t.Errorf("%s: expected a clean not-connected answer, got %+v", tc.name, resp)
		}
	}
}

// A peer is only addressable with its kind and id; a bad one must be rejected
// rather than silently addressed as the wrong type of chat.
func TestTelPeerFromQuery(t *testing.T) {
	for _, tc := range []struct {
		query string
		ok    bool
		kind  tgbridge.PeerKind
	}{
		{"kind=user&id=7&accessHash=9", true, tgbridge.PeerUser},
		{"kind=channel&id=7", true, tgbridge.PeerChannel},
		{"kind=forum&id=7", true, tgbridge.PeerForum},
		{"kind=bot&id=7", true, tgbridge.PeerBot},
		{"kind=user", false, ""},       // no id
		{"id=7", false, ""},            // no kind
		{"kind=bogus&id=7", false, ""}, // unknown kind
		{"kind=user&id=notanumber", false, ""},
	} {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		peer, ok := telPeerFromQuery(q, "")
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.query, ok, tc.ok)
			continue
		}
		if ok && peer.Kind != tc.kind {
			t.Errorf("%s: kind = %s, want %s", tc.query, peer.Kind, tc.kind)
		}
	}
}

// Dialog paging carries its cursor as a SECOND peer in the same query, so the
// prefixed form must read those fields and not the request's own peer.
func TestTelPeerFromQueryPrefixed(t *testing.T) {
	q, err := url.ParseQuery("kind=user&id=7&offsetKind=channel&offsetId=11&offsetAccessHash=13")
	if err != nil {
		t.Fatal(err)
	}
	peer, ok := telPeerFromQuery(q, "offset")
	if !ok {
		t.Fatal("prefixed peer not read")
	}
	if peer.Kind != tgbridge.PeerChannel || peer.ID != 11 || peer.AccessHash != 13 {
		t.Fatalf("prefixed peer wrong: %+v", peer)
	}
	// Absent cursor must report "no peer", not the request's own peer.
	q2, _ := url.ParseQuery("kind=user&id=7")
	if p, ok := telPeerFromQuery(q2, "offset"); ok {
		t.Fatalf("missing cursor reported a peer: %+v", p)
	}
}

// The session file is the auth key: deleting it must be the thing that signs
// out locally, and wiping a non-existent one is not an error.
func TestTelWipeSession(t *testing.T) {
	s := newTelTestServer(t)
	h := s.tel
	if err := h.wipeSession(); err != nil {
		t.Fatalf("wiping an absent session errored: %v", err)
	}
	path := h.sessionFile().Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !h.sessionFile().LoggedIn() {
		t.Fatal("stored session not reported")
	}
	if err := h.wipeSession(); err != nil {
		t.Fatal(err)
	}
	if h.sessionFile().LoggedIn() {
		t.Fatal("session survived the wipe")
	}
}

// The session path must be where backup looks for it, or a restore would put
// the auth key somewhere the bridge never reads.
func TestTelSessionPathMatchesBackup(t *testing.T) {
	s := newTelTestServer(t)
	if got, want := s.tel.sessionFile().Path, telegramSessionPath(s.dataDir); got != want {
		t.Fatalf("hub uses %q but backup uses %q", got, want)
	}
}

// Only a VERIFIED bit may claim a tunnel: an unsigned metadata read must never
// make a config look tunnel-capable.
func TestMTPAdvRequiresVerifiedMetadata(t *testing.T) {
	s := newTelTestServer(t)
	s.recordMTPAdv("a.example.com", true, false) // unverified
	if adv, fresh := s.mtpAdvVerified("a.example.com"); adv || fresh {
		t.Fatal("unverified metadata claimed a tunnel")
	}
	s.recordMTPAdv("a.example.com", true, true)
	if adv, fresh := s.mtpAdvVerified("a.example.com"); !adv || !fresh {
		t.Fatal("verified tunnel bit not recorded")
	}
	// A verified negative is also a fact worth caching.
	s.recordMTPAdv("b.example.com", false, true)
	if adv, fresh := s.mtpAdvVerified("b.example.com"); adv || !fresh {
		t.Fatal("verified 'no tunnel' not recorded as fresh")
	}
	if _, fresh := s.mtpAdvVerified("never.example.com"); fresh {
		t.Fatal("unknown config reported fresh")
	}
}

// The Telegram session holds the MTProto auth key, so it must survive an
// encrypted backup round trip -- otherwise migrating to a new device means
// logging in again -- and it must stay opt-in and 0600 on restore.
func TestBackupRoundTripsTelegramSession(t *testing.T) {
	src := newTelTestServer(t)
	want := []byte(`{"auth_key":"not-a-real-key"}`)
	// Stored the way the app stores it: sealed. The export has to go through
	// the same layer to put the session, not ciphertext, into the backup.
	if err := src.tel.sessionFile().StoreSession(context.Background(), want); err != nil {
		t.Fatal(err)
	}

	// Export WITHOUT the telegram section: the key must not ride along.
	data := telExport(t, src, `["profiles"]`)
	if p := telOpen(t, data); len(p.Sections.Telegram) != 0 {
		t.Fatal("session exported even though the section was not selected")
	}

	// Export WITH it.
	data = telExport(t, src, `["telegram"]`)
	p := telOpen(t, data)
	if string(p.Sections.Telegram) != string(want) {
		t.Fatalf("session not in the backup: %q", p.Sections.Telegram)
	}

	// Restore into a fresh server.
	dst := newTelTestServer(t)
	body, ct := backupMultipart(t, data, "pw123456", []string{"telegram"})
	req := httptest.NewRequest("POST", "/api/backup/restore", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	dst.handleBackupRestore(rec, req)
	if rec.Code != 200 {
		t.Fatalf("restore status %d (%s)", rec.Code, rec.Body.String())
	}

	// Read back THROUGH the sealing layer: the session is encrypted at rest
	// now, so the bytes on disk are deliberately not the session.
	got, err := dst.tel.sessionFile().LoadSession(context.Background())
	if err != nil {
		t.Fatalf("session not restored: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("restored session = %q", got)
	}
	raw, err := os.ReadFile(telegramSessionPath(dst.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == string(want) {
		t.Fatal("the restored session was left in the clear on disk")
	}
	st, err := os.Stat(telegramSessionPath(dst.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("restored session mode %o, want 600", perm)
	}
	// And the bridge must find it exactly where it was put.
	if !dst.tel.sessionFile().LoggedIn() {
		t.Fatal("restored session is not where the bridge looks for it")
	}
}

func telExport(t *testing.T, s *Server, sectionsJSON string) []byte {
	t.Helper()
	body := `{"password":"pw123456","sections":` + sectionsJSON + `}`
	req := httptest.NewRequest("POST", "/api/backup/export", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleBackupExport(rec, req)
	if rec.Code != 200 {
		t.Fatalf("export status %d (%s)", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func telOpen(t *testing.T, data []byte) *backupPayload {
	t.Helper()
	p, err := openBackup(data, "pw123456")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Without app credentials nothing can connect, so the UI must be told up
// front rather than the bridge failing with a raw error at connect time.
func TestTelCredentialsFlow(t *testing.T) {
	s := newTelTestServer(t)
	if err := s.saveProfiles(&ProfileList{}); err != nil {
		t.Fatal(err)
	}

	info := func() map[string]any {
		rec := httptest.NewRecorder()
		s.handleTelInfo(rec, httptest.NewRequest(http.MethodGet, "/api/tel/info", nil))
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if info()["hasCredentials"] != false {
		t.Fatal("a fresh install should report no app credentials")
	}

	// Rejected: both halves are required.
	for _, body := range []string{`{"apiId":0,"apiHash":"h"}`, `{"apiId":5,"apiHash":"  "}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tel/credentials", strings.NewReader(body))
		s.handleTelCredentials(rec, req)
		if rec.Code == 200 {
			t.Errorf("accepted incomplete credentials: %s", body)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tel/credentials",
		strings.NewReader(`{"apiId":12345,"apiHash":"abcdef"}`))
	s.handleTelCredentials(rec, req)
	if rec.Code != 200 {
		t.Fatalf("saving credentials: status %d (%s)", rec.Code, rec.Body.String())
	}
	if info()["hasCredentials"] != true {
		t.Fatal("credentials saved but not reported")
	}

	// They must actually reach the bridge.
	id, hash := s.tel.apiCredentials()
	if id != 12345 || hash != "abcdef" {
		t.Fatalf("bridge sees %d/%q", id, hash)
	}
}

// On a shared backend the Telegram session lands on the MACHINE running the
// client, and everyone pointing a browser at it gets the same session. The UI
// can only warn about that if the backend says so.
func TestTelInfoReportsSharedBackend(t *testing.T) {
	s := newTelTestServer(t)

	read := func() bool {
		rec := httptest.NewRecorder()
		s.handleTelInfo(rec, httptest.NewRequest(http.MethodGet, "/api/tel/info", nil))
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		shared, ok := m["shared"].(bool)
		if !ok {
			t.Fatalf("info has no shared flag: %v", m)
		}
		return shared
	}

	if read() {
		t.Fatal("a single-user backend must not warn")
	}
	s.SetSharedBackend(true)
	if !read() {
		t.Fatal("shared backend not reported, so the UI cannot warn")
	}
}

// A connect takes minutes; holding the hub lock blocks every status call and
// deadlocks anything reporting progress from inside it.
func TestTelStatusStaysResponsiveDuringConnect(t *testing.T) {
	s := newTelTestServer(t)
	h := s.tel

	// Stand in for a connect in flight.
	h.mu.Lock()
	h.connecting = true
	h.mu.Unlock()

	// Progress reporting must not need the hub lock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			h.cells.Add(1)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("counting cells blocked: the progress path takes a lock it must not")
	}

	// And the status endpoint must answer while all that is happening.
	ready := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.handleTelInfo(rec, httptest.NewRequest(http.MethodGet, "/api/tel/info", nil))
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		n, _ := m["cells"].(float64)
		if m["connecting"] != true {
			t.Error("status did not report the connect in flight")
		}
		ready <- int(n)
	}()
	select {
	case got := <-ready:
		if got != 100 {
			t.Fatalf("cells = %d, want 100", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status blocked while a connect was in flight")
	}
}

// gotd replaces SESSION_PASSWORD_NEEDED with a typed error reading "2FA
// required", so matching the Telegram name never fires.
func TestTelDetectsTwoStepVerification(t *testing.T) {
	// What gotd actually returns from SignIn.
	if !isPasswordNeeded(auth.ErrPasswordAuthNeeded) {
		t.Fatal("gotd's typed 2FA error not recognised: the password step is unreachable")
	}
	// Wrapped, as it arrives through withBridge.
	if !isPasswordNeeded(fmt.Errorf("sign in: %w", auth.ErrPasswordAuthNeeded)) {
		t.Fatal("wrapped 2FA error not recognised")
	}
	// The raw Telegram error, in case a future gotd stops translating it.
	if !isPasswordNeeded(tgerr.New(401, "SESSION_PASSWORD_NEEDED")) {
		t.Fatal("raw SESSION_PASSWORD_NEEDED not recognised")
	}
	// And it must not fire on anything else, or a wrong code would silently
	// turn into a password prompt.
	for _, e := range []error{
		nil,
		errors.New("boom"),
		tgerr.New(400, "PHONE_CODE_INVALID"),
	} {
		if isPasswordNeeded(e) {
			t.Fatalf("false positive on %v", e)
		}
	}
}

// A wrong 2FA password must say so, not fall back to a generic failure.
func TestTelMapsWrongPassword(t *testing.T) {
	if got := telStatusKey(auth.ErrPasswordInvalid); got != "tel_err_password_invalid" {
		t.Fatalf("typed invalid-password error mapped to %q", got)
	}
	if got := telStatusKey(tgerr.New(400, "PASSWORD_HASH_INVALID")); got != "tel_err_password_invalid" {
		t.Fatalf("raw invalid-password error mapped to %q", got)
	}
}

// The background watcher must announce arrivals and nothing else: not the
// backlog it finds on its first run, and not a count that fell because the
// user read the messages on another device.
func TestTelUnreadDelta(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prev  int
		seen  bool
		total int
		want  int
	}{
		{"first check sets the baseline", 0, false, 12, 0},
		{"new messages", 3, true, 5, 2},
		{"nothing new", 5, true, 5, 0},
		{"read elsewhere", 5, true, 1, 0},
		{"read elsewhere to zero", 5, true, 0, 0},
	} {
		if got := unreadDelta(tc.prev, tc.seen, tc.total); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The "load more chats" button hangs off one flag that has to survive the
// whole way out to the browser. Every cache-served answer used to drop it, and
// the first page comes from the cache on every load except a forced refresh --
// so on an account with more conversations than one page holds, the button and
// the background pager behind it never appeared at all.
func TestTelDialogsFromCacheOffersTheNextPage(t *testing.T) {
	s := newTelTestServer(t)
	rows := make([]tgbridge.Dialog, 40)
	for i := range rows {
		rows[i] = tgbridge.Dialog{
			Peer:   tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: int64(i + 1), AccessHash: 9},
			TopMsg: 500 - i, When: 1700000000 - i,
		}
	}
	s.tel.store().PutDialogs(rows)

	rec := httptest.NewRecorder()
	s.handleTelDialogs(rec, httptest.NewRequest(
		http.MethodGet, "/api/tel/dialogs?cacheOnly=1&limit=40", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var resp struct {
		OK     bool `json:"ok"`
		Cached bool `json:"cached"`
		Page   struct {
			Dialogs  []tgbridge.Dialog `json:"dialogs"`
			More     bool              `json:"more"`
			NextID   int               `json:"nextId"`
			NextPeer tgbridge.Peer     `json:"nextPeer"`
		} `json:"page"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.Cached || len(resp.Page.Dialogs) != 40 {
		t.Fatalf("cache not served: %+v", resp)
	}
	if !resp.Page.More {
		t.Fatal("more=false — the browser hides the button and never asks for page 2")
	}
	// The cursor has to come with it, or the button fetches the same page.
	if resp.Page.NextID != rows[39].TopMsg || resp.Page.NextPeer.AccessHash != 9 {
		t.Errorf("cursor = id %d peer %+v", resp.Page.NextID, resp.Page.NextPeer)
	}
}

// Fewer conversations than a page is the end of the list, and must say so:
// a button that fetches nothing is worse than no button.
func TestTelDialogsFromCacheStopsWhenTheListIsShort(t *testing.T) {
	s := newTelTestServer(t)
	s.tel.store().PutDialogs([]tgbridge.Dialog{
		{Peer: tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 1, AccessHash: 9}, TopMsg: 10},
	})
	rec := httptest.NewRecorder()
	s.handleTelDialogs(rec, httptest.NewRequest(
		http.MethodGet, "/api/tel/dialogs?cacheOnly=1&limit=40", nil))
	var resp struct {
		Page struct {
			More bool `json:"more"`
		} `json:"page"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Page.More {
		t.Error("one conversation is not a full page")
	}
}

// A bridge whose gotd loop has exited is dead, not connected: it clears its
// API handle and answers every later call with ErrNotConnected for ever. Two
// things used to miss that, and together they produced "the link says it is
// connected but nothing works, not even sending a message":
//
//   - isDeadConnection matched gotd's own strings only, so the hub never
//     dropped the corpse and never rebuilt;
//   - /api/tel/info reported "connected" from the bridge merely existing.
func TestDeadBridgeIsNotConnected(t *testing.T) {
	if !isDeadConnection(tgbridge.ErrNotConnected) {
		t.Error("a bridge with no client is dead; the hub must rebuild rather than keep it")
	}
	// Wrapped, which is how it arrives from a call several layers down.
	if !isDeadConnection(fmt.Errorf("dialogs: %w", tgbridge.ErrNotConnected)) {
		t.Error("must survive wrapping")
	}
	// And the distinction that already mattered must still hold: one
	// connection generation dying under a call in flight is NOT terminal.
	if isDeadConnection(errors.New("engine forcibly closed")) {
		t.Error("a restarting connection must not be thrown away")
	}
	if isDeadConnection(nil) {
		t.Error("no error is not a dead connection")
	}
}

// An unstarted bridge must report itself as not running, which is what the
// info endpoint now asks before claiming a live link.
func TestBridgeRunningGatesConnected(t *testing.T) {
	s := newTelTestServer(t)
	s.tel.mu.Lock()
	s.tel.bridge = &tgbridge.Bridge{} // published but never started
	s.tel.mu.Unlock()

	rec := httptest.NewRecorder()
	s.handleTelInfo(rec, httptest.NewRequest(http.MethodGet, "/api/tel/info", nil))
	var resp struct {
		Connected bool `json:"connected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Connected {
		t.Error("a bridge with no running client must not be reported as connected")
	}
}

// Every endpoint that changes something must require POST.
//
// The cross-site guard deliberately lets GET through, because a read cannot be
// an attack. Disconnect accepted GET, so any page the user opened could have
// torn down their tunnel with an <img src> pointed at the loopback port.
func TestTelStateChangingEndpointsRefuseGET(t *testing.T) {
	s := newTelTestServer(t)
	for _, tc := range []struct {
		name string
		h    func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"disconnect", s.handleTelDisconnect, "/api/tel/disconnect"},
		{"credentials", s.handleTelCredentials, "/api/tel/credentials"},
		{"auth", s.handleTelAuth, "/api/tel/auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.h(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("GET %s returned %d, want 405 — a page the user visits could "+
					"trigger this across origins", tc.path, rec.Code)
			}
		})
	}
}

// A rebuild must hold the same flag a user-initiated connect does.
//
// callBridge's retry path dropped the dead bridge and built a new one without
// ever marking the hub "connecting". For the whole of that run the hub had no
// bridge and was not connecting — the two conditions /api/tel/connect checks
// before starting one of its own — so a reconnect tapped in that window built
// a second bridge alongside it. Whichever lost the assignment was never
// stopped, and its tunnel went on polling DNS for ever.
func TestTelRebuildClaimsTheConnectFlag(t *testing.T) {
	s := newTelTestServer(t)
	h := s.tel

	// A connect already running: the rebuild must stand down rather than
	// race it.
	h.mu.Lock()
	h.lastCfg = Config{Domain: "m.example.com"}
	h.lastNick = "test"
	h.connecting = true
	h.bridge = &tgbridge.Bridge{}
	h.mu.Unlock()

	sentinel := errors.New("dead")
	called := 0
	err := h.callBridge(context.Background(), true, func(*tgbridge.Bridge) error {
		called++
		return tgbridge.ErrNotConnected
	})
	if called != 1 {
		t.Fatalf("fn called %d times; a rebuild was attempted while another connect was running", called)
	}
	if !errors.Is(err, tgbridge.ErrNotConnected) {
		t.Fatalf("err = %v, want the original failure (%v is only a marker)", err, sentinel)
	}
	// The other connect's flag must be left exactly as it was.
	h.mu.Lock()
	still := h.connecting
	h.mu.Unlock()
	if !still {
		t.Fatal("the rebuild cleared a flag it never set, freeing the guard for a third connect")
	}
}

// Signing out must take the account off THIS DEVICE first, and must not wait
// on a link that is down.
//
// It used to tell Telegram first, through withBridge on the eight-minute call
// timeout — and withBridge rebuilds a dead connection before trying, so asking
// to sign out with the link down could sit through a fifteen-minute reconnect
// to a server that is not there. The key is a file on this machine; deleting
// it is what ends the session here.
func TestTelLogoutIsLocalAndImmediate(t *testing.T) {
	s := newTelTestServer(t)
	h := s.tel

	// A stored session, as a signed-in account has.
	if err := os.MkdirAll(filepath.Dir(h.sessionFile().Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.sessionFile().Path, []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.rememberAuthorized(true)

	start := time.Now()
	rec := httptest.NewRecorder()
	s.handleTelAuth(rec, telJSONReq("/api/tel/auth", `{"action":"logout"}`))
	took := time.Since(start)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// Nothing is connected here, which is the case that used to hang.
	if took > 5*time.Second {
		t.Fatalf("signing out with no link took %v — it must not wait on a tunnel "+
			"that is not there", took)
	}
	// The account is off the device: that is the whole point.
	if _, err := os.Stat(h.sessionFile().Path); !os.IsNotExist(err) {
		t.Fatalf("the session file survived the sign-out (%v)", err)
	}
	if h.wasAuthorized() {
		t.Error("still marked as signed in")
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Errorf("reported failure although the account was removed: %s", rec.Body.String())
	}
}

// The courtesy call to Telegram must be bounded far below an ordinary RPC, so
// a slow link can never make signing out feel broken.
func TestTelLogoutRemoteCallIsShortLived(t *testing.T) {
	if telLogoutRemoteTimeout >= telCallTimeout {
		t.Fatalf("logout waits %v, the same as any other call (%v)",
			telLogoutRemoteTimeout, telCallTimeout)
	}
	if telLogoutRemoteTimeout > 15*time.Second {
		t.Errorf("logout may wait %v for a courtesy message; the sign-out itself "+
			"is already done locally", telLogoutRemoteTimeout)
	}
}

// The two code paths that take a short page must merge it, not replace the
// list with it. Guarded here as well as in the cache test, because the bug was
// the CALL SITE choosing the wrong method, not the method being wrong.
func TestShortPagesAreMergedNotReplaced(t *testing.T) {
	for _, f := range []string{"handlers_tel.go", "handlers_tel_api.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "PutDialogs(page.Dialogs)") {
			t.Errorf("%s replaces the cached list with one page; every row past "+
				"that page is lost, including the ones the user paid to load", f)
		}
	}
}

// The end marker has to be RECORDED, not just honoured.
//
// cachedDialogPage stops offering another page once Telegram has said the list
// ends, but only if the dialogs handler writes that down when the answer comes
// back. Guarded at the source because the recording needs a live bridge to
// exercise, and a cache that is never told is indistinguishable from one that
// ignores what it is told: both leave the button fetching nothing for ever.
func TestDialogsHandlerRecordsTheEndMarker(t *testing.T) {
	src, err := os.ReadFile("handlers_tel_api.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "h.store().SetDialogsComplete(!page.More)") {
		t.Error("the dialogs handler no longer records whether Telegram said the " +
			"list ends; the cached answer can then never stop offering another page")
	}
}
