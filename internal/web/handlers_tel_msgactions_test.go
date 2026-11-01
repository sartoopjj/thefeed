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

func TestTelEditAndDeleteValidate(t *testing.T) {
	s := newTelTestServer(t)
	call := func(h func(http.ResponseWriter, *http.Request), body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	peer := `{"kind":"user","id":"8","accessHash":"9"}`
	for name, c := range map[string]struct {
		h    func(http.ResponseWriter, *http.Request)
		body string
		code int
	}{
		"edit without a message": {s.handleTelEdit, `{"peer":` + peer + `,"text":"x"}`, 400},
		"edit to nothing":        {s.handleTelEdit, `{"peer":` + peer + `,"msgId":5,"text":"   "}`, 400},
		"edit, no link":          {s.handleTelEdit, `{"peer":` + peer + `,"msgId":5,"text":"x"}`, 200},
		"delete nothing":         {s.handleTelDeleteMessages, `{"peer":` + peer + `,"ids":[]}`, 400},
		"delete, no link":        {s.handleTelDeleteMessages, `{"peer":` + peer + `,"ids":[5],"revoke":true}`, 200},
	} {
		code, out := call(c.h, c.body)
		if code != c.code {
			t.Errorf("%s: %d %v", name, code, out)
		}
		if code == 200 && out["reason"] != "tel_err_not_connected" {
			t.Errorf("%s: %v", name, out)
		}
	}
}

// An edit made here lands in the cache as an edit from Telegram would.
func TestTelEditIsCached(t *testing.T) {
	c := loadTelCache(t.TempDir())
	key := telCacheKey(tgbridge.Peer{Kind: tgbridge.PeerUser, ID: 8})
	c.MergeThread(key, []tgbridge.Message{{ID: 5, Text: "teh", Out: true,
		Entities: []tgbridge.Entity{{Type: "bold", Offset: 0, Length: 3}}}})
	c.EditText(key, 5, "the", 777)
	got := c.Thread(key)[0]
	if got.Text != "the" || got.EditDate != 777 || got.Entities != nil {
		t.Fatalf("cached edit: %+v", got)
	}
}

func TestTelEditDeleteErrorsAreReadable(t *testing.T) {
	for code, want := range map[string]string{
		"MESSAGE_EDIT_TIME_EXPIRED": "tel_err_edit_expired",
		"MESSAGE_AUTHOR_REQUIRED":   "tel_err_edit_not_yours",
		"MESSAGE_DELETE_FORBIDDEN":  "tel_err_delete_forbidden",
	} {
		if got := telStatusKey(tgerr.New(400, code)); got != want {
			t.Errorf("%s: %s", code, got)
		}
	}
}
