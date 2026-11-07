package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gotd/td/tgerr"
)

func TestTelForwardValidates(t *testing.T) {
	s := newTelTestServer(t)
	from := `{"kind":"channel","id":"100","accessHash":"1"}`
	to := `{"kind":"user","id":"8","accessHash":"9"}`
	many := strings.Repeat("1,", telForwardMax) + "1"
	for name, c := range map[string]struct {
		method, body string
		code         int
	}{
		"GET":              {http.MethodGet, "", 405},
		"no source":        {http.MethodPost, `{"ids":[5],"to":` + to + `}`, 400},
		"no target":        {http.MethodPost, `{"from":` + from + `,"ids":[5]}`, 400},
		"no messages":      {http.MethodPost, `{"from":` + from + `,"ids":[],"to":` + to + `}`, 400},
		"over the limit":   {http.MethodPost, `{"from":` + from + `,"ids":[` + many + `],"to":` + to + `}`, 400},
		"no link":          {http.MethodPost, `{"from":` + from + `,"ids":[5],"to":` + to + `}`, 200},
		"options, no link": {http.MethodPost, `{"from":` + from + `,"ids":[5],"to":` + to + `,"options":{"hideSender":true,"silent":true}}`, 200},
	} {
		rec := httptest.NewRecorder()
		s.handleTelForward(rec, httptest.NewRequest(c.method, "/", strings.NewReader(c.body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != c.code {
			t.Errorf("%s: %d %v", name, rec.Code, out)
		}
		if rec.Code == 200 && out["reason"] != "tel_err_not_connected" {
			t.Errorf("%s: %v", name, out)
		}
	}
}

func TestTelForwardErrorsAreReadable(t *testing.T) {
	for code, want := range map[string]string{
		"CHAT_FORWARDS_RESTRICTED":  "tel_err_forward_protected",
		"CHAT_WRITE_FORBIDDEN":      "tel_err_cannot_write",
		"CHAT_SEND_PLAIN_FORBIDDEN": "tel_err_cannot_write",
		"USER_IS_BLOCKED":           "tel_err_cannot_write",
		"YOU_BLOCKED_USER":          "tel_err_you_blocked", // says how to fix it
	} {
		if got := telStatusKey(tgerr.New(400, code)); got != want {
			t.Errorf("%s: %s", code, got)
		}
	}
}
