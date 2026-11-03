package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A config is saved only when its domains are DNS names.
func TestProfileDomainMustBeADNSName(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		Profiles: []Profile{{ID: "p1", Nickname: "a", Config: Config{Domain: "a.example.com"}}},
		Active:   "p1",
	})
	post := func(body string) int {
		rec := httptest.NewRecorder()
		s.handleProfiles(rec, httptest.NewRequest(http.MethodPost, "/api/profiles", strings.NewReader(body)))
		return rec.Code
	}
	for _, bad := range []string{
		`{"action":"create","profile":{"config":{"domain":"x');alert(1);('.evil.com"}}}`,
		`{"action":"create","profile":{"config":{"domain":"ok.example.com","extraDomains":["b\"c.example.com"]}}}`,
		`{"action":"update","profile":{"id":"p1","config":{"domain":"a b.example.com"}}}`,
		`{"action":"create","profile":{"config":{"domain":"a'b.example.com"}}}`,
		`{"action":"create","profile":{"config":{"domain":"` + strings.Repeat("a", 254) + `"}}}`,
	} {
		if code := post(bad); code != http.StatusBadRequest {
			t.Errorf("%.60s: %d, want 400", bad, code)
		}
	}
	pl, _ := s.loadProfiles()
	if len(pl.Profiles) != 1 || pl.Profiles[0].Config.Domain != "a.example.com" {
		t.Fatalf("a refused config changed the list: %+v", pl.Profiles)
	}
	if code := post(`{"action":"create","profile":{"config":{"domain":"T_1.Example-2.com.","extraDomains":["b.example.com"]}}}`); code != http.StatusOK {
		t.Fatalf("a valid domain was refused: %d", code)
	}
}
