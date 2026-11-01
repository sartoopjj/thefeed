package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

var (
	delUser   = tgbridge.Peer{Kind: tgbridge.PeerUser, ID: 10, AccessHash: 1}
	delBasic  = tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 20}
	delSuper  = tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 30, AccessHash: 3}
	delChan   = tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 40, AccessHash: 4}
	delOrphan = tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 50} // no row: kind unknown
)

type delCase struct {
	p    tgbridge.Peer
	want []int
}

func delMsgs(ids ...int) []tgbridge.Message {
	out := make([]tgbridge.Message, 0, len(ids))
	for _, id := range ids {
		out = append(out, tgbridge.Message{ID: id, Date: 1000 + id, Text: "m" + itoaInt(id)})
	}
	return out
}

func itoaInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func delIDs(c *telCache, p tgbridge.Peer) []int {
	var ids []int
	for _, m := range c.Thread(telCacheKey(p)) {
		ids = append(ids, m.ID)
	}
	return ids
}

func delCache(t *testing.T) *telCache {
	t.Helper()
	c := loadTelCache(t.TempDir())
	c.PutDialogs([]tgbridge.Dialog{{Peer: delUser}, {Peer: delBasic}, {Peer: delSuper}, {Peer: delChan}})
	for _, p := range []tgbridge.Peer{delUser, delBasic, delSuper, delChan, delOrphan} {
		c.MergeThread(telCacheKey(p), delMsgs(5, 6, 7))
	}
	return c
}

// Box ids are unique across private chats and basic groups -- and only there.
// A supergroup or channel has its own numbering, so the same number in it is a
// different message that must stay.
func TestBoxDeleteOnlyTouchesTheBox(t *testing.T) {
	c := delCache(t)
	c.ApplyDeletes([]tgbridge.Deletion{{IDs: []int{6}}})
	for _, tc := range []delCase{
		{delUser, []int{5, 7}}, {delBasic, []int{5, 7}},
		{delSuper, []int{5, 6, 7}}, {delChan, []int{5, 6, 7}}, {delOrphan, []int{5, 6, 7}},
	} {
		if got := delIDs(c, tc.p); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", telCacheKey(tc.p), got, tc.want)
		}
	}
}

func TestChannelDeleteOnlyTouchesThatChannel(t *testing.T) {
	c := delCache(t)
	c.ApplyDeletes([]tgbridge.Deletion{{ChannelID: 30, IDs: []int{5}}, {ChannelID: 40, IDs: []int{7}}})
	for _, tc := range []delCase{
		{delSuper, []int{6, 7}}, {delChan, []int{5, 6}},
		{delUser, []int{5, 6, 7}}, {delBasic, []int{5, 6, 7}}, {delOrphan, []int{5, 6, 7}},
	} {
		if got := delIDs(c, tc.p); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", telCacheKey(tc.p), got, tc.want)
		}
	}
}

// A page fetched before the delete must not bring the message back, and the
// page learns what to drop from its own copy.
func TestDeletedStaysDeleted(t *testing.T) {
	c := delCache(t)
	c.ApplyDeletes([]tgbridge.Deletion{{IDs: []int{6}}, {ChannelID: 40, IDs: []int{7}}})
	c.MergeThread(telCacheKey(delUser), delMsgs(6, 8))
	c.MergeThread(telCacheKey(delChan), delMsgs(7, 8))
	if got := delIDs(c, delUser); !reflect.DeepEqual(got, []int{5, 7, 8}) {
		t.Errorf("box message came back: %v", got)
	}
	if got := delIDs(c, delChan); !reflect.DeepEqual(got, []int{5, 6, 8}) {
		t.Errorf("channel message came back: %v", got)
	}
	if got := c.Deleted(telCacheKey(delBasic)); !reflect.DeepEqual(got, []int{6}) {
		t.Errorf("box tombstones: %v", got)
	}
	if got := c.Deleted(telCacheKey(delChan)); !reflect.DeepEqual(got, []int{7}) {
		t.Errorf("channel tombstones: %v", got)
	}
	if got := c.Deleted(telCacheKey(delSuper)); len(got) != 0 {
		t.Errorf("a supergroup got another numbering's tombstones: %v", got)
	}
}

// The conversation list must not keep showing a deleted last message.
func TestDeletedLastMessageLeavesThePreview(t *testing.T) {
	c := loadTelCache(t.TempDir())
	c.PutDialogs([]tgbridge.Dialog{{Peer: delUser, TopMsg: 7, Preview: "secret"}})
	c.MergeThread(telCacheKey(delUser), delMsgs(5, 6, 7))
	c.ApplyDeletes([]tgbridge.Deletion{{IDs: []int{7}}})
	row := c.Dialogs()[0]
	if row.TopMsg != 6 || row.Preview != "m6" {
		t.Errorf("row after delete: top %d preview %q", row.TopMsg, row.Preview)
	}
	c.ApplyDeletes([]tgbridge.Deletion{{IDs: []int{5, 6}}})
	if row := c.Dialogs()[0]; row.Preview != "" {
		t.Errorf("nothing left, but preview %q", row.Preview)
	}
}

// A self-destructing message is gone at its time, delete update or not, and
// never becomes a preview.
func TestSelfDestructedMessagesAreNotServed(t *testing.T) {
	c := loadTelCache(t.TempDir())
	now := int(time.Now().Unix())
	c.MergeThread(telCacheKey(delUser), []tgbridge.Message{
		{ID: 1, Text: "past", Expires: now - 1},
		{ID: 2, Text: "later", Expires: now + 3600},
		{ID: 3, Text: "plain"},
	})
	if got := delIDs(c, delUser); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Errorf("served %v", got)
	}
	c.ApplyNew([]tgbridge.PeerMessages{{Peer: delUser, Messages: []tgbridge.Message{{ID: 4, Text: "timed", Expires: now + 60}}}})
	if row := c.Dialogs()[0]; row.Preview != "" {
		t.Errorf("a timed message became the preview: %q", row.Preview)
	}
}

// Deleted ids reach the page even when nothing is left in the thread.
func TestHistoryCarriesDeletedIDs(t *testing.T) {
	s := newTelTestServer(t)
	c := s.tel.store()
	c.PutDialogs([]tgbridge.Dialog{{Peer: delUser}})
	c.MergeThread(telCacheKey(delUser), delMsgs(5))
	c.ApplyDeletes([]tgbridge.Deletion{{IDs: []int{5}}})
	rec := httptest.NewRecorder()
	s.handleTelHistory(rec, httptest.NewRequest(http.MethodGet,
		"/api/tel/history?kind=user&id=10&accessHash=1&cacheOnly=1", strings.NewReader("")))
	var out struct {
		OK      bool  `json:"ok"`
		Deleted []int `json:"deleted"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || !reflect.DeepEqual(out.Deleted, []int{5}) {
		t.Fatalf("got %s", rec.Body.String())
	}
}

// Direct messages to a channel are a supergroup with no row of their own.
func TestDeleteReachesDirectMessages(t *testing.T) {
	c := delCache(t)
	direct := tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 60, AccessHash: 6, Direct: true}
	c.SetDirect(telCacheKey(delChan), direct)
	c.MergeThread(telCacheKey(direct), delMsgs(5, 6, 7))
	c.ApplyDeletes([]tgbridge.Deletion{{ChannelID: 60, IDs: []int{6}}})
	if got := delIDs(c, direct); !reflect.DeepEqual(got, []int{5, 7}) {
		t.Errorf("direct messages kept a deleted one: %v", got)
	}
	if got := c.Deleted(telCacheKey(direct)); !reflect.DeepEqual(got, []int{6}) {
		t.Errorf("the page is not told: %v", got)
	}
	c.ApplyDeletes([]tgbridge.Deletion{{IDs: []int{5}}})
	if got := delIDs(c, direct); !reflect.DeepEqual(got, []int{5, 7}) {
		t.Errorf("a box delete reached a channel's numbering: %v", got)
	}
}

// A self-destructed message leaves the stored cache, not just the answer.
func TestExpiredMessagesLeaveTheFile(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	now := int(time.Now().Unix())
	key := telCacheKey(delUser)
	c.MergeThread(key, []tgbridge.Message{
		{ID: 1, Text: "stays"}, {ID: 2, Text: "later", Expires: now + 3600}, {ID: 3, Text: "gone", Expires: now - 1},
	})
	if got := c.data.Threads[key].Msgs; len(got) != 2 {
		t.Errorf("merge stored an expired message: %+v", got)
	}
	// One that expires while held.
	c.mu.Lock()
	tt := c.data.Threads[key]
	tt.Msgs[1].Expires = now - 1
	c.data.Threads[key] = tt
	c.mu.Unlock()
	if got := delIDs(c, delUser); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("read: %v", got)
	}
	if got := c.data.Threads[key].Msgs; len(got) != 1 {
		t.Errorf("an expired message stayed in the cache: %+v", got)
	}
}
