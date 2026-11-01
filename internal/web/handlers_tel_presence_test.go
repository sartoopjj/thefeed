package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Both are accepted as the page sends them, refused only for want of a link,
// and never put an error on the screen.
func TestTelPresenceRequests(t *testing.T) {
	s := newTelTestServer(t)
	call := func(h func(http.ResponseWriter, *http.Request), body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	for name, c := range map[string]struct {
		h    func(http.ResponseWriter, *http.Request)
		body string
	}{
		"online":  {s.handleTelStatus, `{"online":true}`},
		"offline": {s.handleTelStatus, `{"online":false}`},
		"typing":  {s.handleTelTyping, `{"peer":{"kind":"user","id":"8","accessHash":"9"}}`},
	} {
		code, out := call(c.h, c.body)
		if code != 200 || out["reason"] != "tel_err_not_connected" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, _ := call(s.handleTelTyping, `{"peer":{}}`); code != http.StatusBadRequest {
		t.Errorf("typing without a conversation: %d", code)
	}
	if code, _ := call(s.handleTelStatus, `not json`); code != http.StatusBadRequest {
		t.Errorf("garbage status: %d", code)
	}
	if s.tel.lastErr != "" {
		t.Errorf("a presence call set the visible error: %q", s.tel.lastErr)
	}
}
