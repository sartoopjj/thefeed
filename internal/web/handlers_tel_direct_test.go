package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gotd/td/tgerr"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

func telDirectCall(t *testing.T, s *Server, p tgbridge.Peer) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"peer": p})
	rec := httptest.NewRecorder()
	s.handleTelDirect(rec, httptest.NewRequest(http.MethodPost, "/api/tel/direct", strings.NewReader(string(b))))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestTelDirectNeedsAChannelThatTakesThem(t *testing.T) {
	s := newTelTestServer(t)
	out := telDirectCall(t, s, tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 100, AccessHash: 1})
	if out["ok"] != false || out["reason"] != "tel_err_no_direct" {
		t.Fatalf("got %v", out)
	}
}

// Found once, then served from the cache: no call, not even a link needed.
func TestTelDirectComesFromTheCache(t *testing.T) {
	s := newTelTestServer(t)
	ch := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 100, AccessHash: 1, DirectID: 200}
	if out := telDirectCall(t, s, ch); out["reason"] != "tel_err_not_connected" {
		t.Fatalf("uncached without a link: %v", out)
	}
	s.tel.store().SetDirect(telCacheKey(ch), tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 200, AccessHash: 2, Direct: true})
	out := telDirectCall(t, s, ch)
	peer, _ := out["peer"].(map[string]any)
	if out["ok"] != true || peer["id"] != "200" || peer["direct"] != true {
		t.Fatalf("cached: %v", out)
	}
}

// A channel that got a new direct-messages conversation is asked again.
func TestTelDirectCacheFollowsTheChannel(t *testing.T) {
	s := newTelTestServer(t)
	ch := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 100, AccessHash: 1, DirectID: 300}
	s.tel.store().SetDirect(telCacheKey(ch), tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 200, AccessHash: 2, Direct: true})
	if out := telDirectCall(t, s, ch); out["reason"] != "tel_err_not_connected" {
		t.Fatalf("served the old conversation: %v", out)
	}
}

func TestTelDirectErrorsAreReadable(t *testing.T) {
	if got := telStatusKey(tgerr.New(403, "ALLOW_PAYMENT_REQUIRED_50")); got != "tel_err_paid" {
		t.Errorf("paid: %s", got)
	}
	if got := telStatusKey(tgbridge.ErrNoDirectMessages); got != "tel_err_no_direct" {
		t.Errorf("no direct: %s", got)
	}
}

// Removing direct messages from the list needs no link: nothing is sent.
func TestTelRemoveDirectNeedsNoLink(t *testing.T) {
	s := newTelTestServer(t)
	direct := tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 200, AccessHash: 2, Direct: true}
	s.tel.store().PutDialogs([]tgbridge.Dialog{{Peer: direct}})
	b, _ := json.Marshal(map[string]any{"peer": direct})
	rec := httptest.NewRecorder()
	s.handleTelDelete(rec, httptest.NewRequest(http.MethodPost, "/api/tel/delete", strings.NewReader(string(b))))
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("got %s", rec.Body.String())
	}
	if n := len(s.tel.store().Dialogs()); n != 0 {
		t.Errorf("still listed: %d rows", n)
	}
	// Anything else still needs the link.
	b, _ = json.Marshal(map[string]any{"peer": tgbridge.Peer{Kind: tgbridge.PeerUser, ID: 8, AccessHash: 9}})
	rec = httptest.NewRecorder()
	s.handleTelDelete(rec, httptest.NewRequest(http.MethodPost, "/api/tel/delete", strings.NewReader(string(b))))
	if !strings.Contains(rec.Body.String(), "tel_err_not_connected") {
		t.Errorf("a private chat without a link: %s", rec.Body.String())
	}
}
