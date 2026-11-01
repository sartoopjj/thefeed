package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

func telAdsCall(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleTelSponsored(rec, httptest.NewRequest(http.MethodPost, "/api/tel/sponsored", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func telAdPeerJSON(t *testing.T, kind tgbridge.PeerKind) string {
	t.Helper()
	b, err := json.Marshal(tgbridge.Peer{Kind: kind, ID: 1234567890123, AccessHash: -5, Title: "c"})
	if err != nil {
		t.Fatal(err)
	}
	return `{"peer":` + string(b) + `}`
}

// Only broadcast channels carry ads, so nothing else costs a round trip --
// not even a check that the link is up.
func TestTelAdsSkipEverythingButChannels(t *testing.T) {
	s := newTelTestServer(t)
	for _, kind := range []tgbridge.PeerKind{tgbridge.PeerGroup, tgbridge.PeerUser, tgbridge.PeerForum, tgbridge.PeerBot} {
		code, out := telAdsCall(t, s, telAdPeerJSON(t, kind))
		if code != 200 || out["ok"] != true {
			t.Errorf("%s: %d %v", kind, code, out)
		}
		if ads, _ := out["ads"].([]any); ads == nil || len(ads) != 0 {
			t.Errorf("%s: ads %v, want an empty list", kind, out["ads"])
		}
	}
	if code, _ := telAdsCall(t, s, `{"peer":{}}`); code != http.StatusBadRequest {
		t.Errorf("no peer: %d", code)
	}
}

// Within five minutes the cached answer is served without a connection; after
// that it is asked again (and here there is nothing to ask through).
func TestTelAdsComeFromTheCacheForFiveMinutes(t *testing.T) {
	s := newTelTestServer(t)
	body := telAdPeerJSON(t, tgbridge.PeerChannel)
	key := telCacheKey(tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 1234567890123})
	s.tel.setAds(key, []tgbridge.Sponsored{{RandomID: []byte{1, 2, 3}, Title: "Shop"}}, time.Now())

	_, out := telAdsCall(t, s, body)
	ads, _ := out["ads"].([]any)
	if out["ok"] != true || len(ads) != 1 {
		t.Fatalf("fresh cache: %v", out)
	}
	if id := ads[0].(map[string]any)["randomId"]; id != "AQID" {
		t.Errorf("randomId on the wire = %v, want base64 AQID", id)
	}

	s.tel.setAds(key, []tgbridge.Sponsored{{RandomID: []byte{1}}}, time.Now().Add(-telAdTTL))
	if _, out := telAdsCall(t, s, body); out["ok"] != false {
		t.Fatalf("stale cache was served: %v", out)
	}
	// An empty answer is cached too: a channel without ads is not asked again.
	s.tel.setAds(key, nil, time.Now())
	if _, out := telAdsCall(t, s, body); out["ok"] != true {
		t.Fatalf("cached empty answer: %v", out)
	}
}

func TestTelAdActValidatesWhatThePageSends(t *testing.T) {
	s := newTelTestServer(t)
	send := func(body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		s.handleTelSponsoredAct(rec, httptest.NewRequest(http.MethodPost, "/api/tel/sponsored/act", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	for _, bad := range []string{
		`{"action":"view"}`,
		`{"action":"delete","randomId":"AQID"}`,
		`{"action":"click","randomId":"not base64!"}`,
	} {
		if code, _ := send(bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
	// Accepted, then refused only because nothing is connected.
	for _, act := range []string{"view", "click", "report"} {
		code, out := send(`{"action":"` + act + `","randomId":"AQID","option":"UQ=="}`)
		if code != 200 || out["reason"] != "tel_err_not_connected" {
			t.Errorf("%s: %d %v", act, code, out)
		}
	}
}

// Ads belong to the account that fetched them: a sign-out drops them, so the
// next account neither sees them nor reports views on them.
func TestTelAdsGoWithTheSession(t *testing.T) {
	s := newTelTestServer(t)
	key := telCacheKey(tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 1234567890123})
	s.tel.setAds(key, []tgbridge.Sponsored{{RandomID: []byte{1}}}, time.Now())
	if err := s.tel.wipeSession(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.tel.adsFor(key, time.Now()); ok {
		t.Fatal("ads survived the sign-out")
	}
}

// Telegram writes its own texts -- an ad's button among them -- in the
// language the client declares, so the page's language is kept for connecting.
func TestTelConnectKeepsTheUILanguage(t *testing.T) {
	s := newTelTestServer(t)
	connect := func(body string) {
		rec := httptest.NewRecorder()
		s.handleTelConnect(rec, httptest.NewRequest(http.MethodPost, "/api/tel/connect", strings.NewReader(body)))
	}
	connect(`{"server":"none","lang":"fa"}`)
	if s.tel.lang != "fa" {
		t.Fatalf("lang = %q, want fa", s.tel.lang)
	}
	connect(`{"server":"none","lang":"xx<script>"}`)
	if s.tel.lang != "fa" {
		t.Fatalf("an unknown language replaced it: %q", s.tel.lang)
	}
	connect(`{"server":"none"}`)
	if s.tel.lang != "fa" {
		t.Fatalf("a request without one cleared it: %q", s.tel.lang)
	}
}
