package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sartoopjj/thefeed/internal/client"
)

func chatThreadAction(t *testing.T, s *Server, peer, action string) {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"peer":"` + peer + `","action":"` + action + `"}`
	s.handleChatThread(rec, httptest.NewRequest(http.MethodPost, "/api/chat/thread", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("%s: status %d: %s", action, rec.Code, rec.Body.String())
	}
}

// A blocked sender's messages never reach the screen, yet are acknowledged
// like any other, so the sender cannot hold the inbox. A friend's message in
// the same fetch is unaffected.
func TestBlockedSenderIsDroppedButAcknowledged(t *testing.T) {
	s := newChatTestServer(t)
	h := s.chat
	abuser, friend := addr(1), addr(2)
	ab, fr := client.ChatAddressString(abuser), client.ChatAddressString(friend)
	chatThreadAction(t, s, ab, "block")

	msgs := []client.ChatIncoming{
		{From: abuser, Seq: 1, Text: "abuse"},
		{From: abuser, Seq: 2, Text: "more abuse"},
		{From: friend, Seq: 1, Text: "hello"},
	}
	maxSeq := ackWatermarks(msgs)
	h.mu.Lock()
	added := h.storeInboxLocked("srv", "me", msgs, maxSeq)
	h.mu.Unlock()

	if added != 1 {
		t.Fatalf("stored %d messages, want only the friend's", added)
	}
	if th := h.threads[ab]; th != nil && len(th.Msgs) != 0 {
		t.Fatalf("the blocked sender's messages were stored: %+v", th.Msgs)
	}
	if th := h.threads[fr]; th == nil || len(th.Msgs) != 1 || th.Msgs[0].Text != "hello" {
		t.Fatalf("the friend's message is missing")
	}
	if maxSeq[ab] != 2 {
		t.Errorf("ack watermark for the blocked sender = %d, want 2", maxSeq[ab])
	}

	// Unblocked, they come through again.
	chatThreadAction(t, s, ab, "unblock")
	h.mu.Lock()
	added = h.storeInboxLocked("srv", "me", []client.ChatIncoming{{From: abuser, Seq: 3, Text: "sorry"}}, nil)
	h.mu.Unlock()
	if added != 1 {
		t.Fatal("an unblocked sender is still dropped")
	}
}

// Deleting the conversation must not lift the block, and the block survives
// a restart and goes into backups.
func TestChatBlockOutlivesDeleteRestartAndBackup(t *testing.T) {
	s := newChatTestServer(t)
	ab := client.ChatAddressString(addr(1))
	chatThreadAction(t, s, ab, "block")
	chatThreadAction(t, s, ab, "delete")

	again := newChatHub(s, s.dataDir)
	if _, ok := again.blocked[ab]; !ok {
		t.Fatal("deleting the conversation or restarting lifted the block")
	}

	var files map[string]json.RawMessage
	if err := json.Unmarshal(s.collectChatBackup(), &files); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files["blocked"]), ab) {
		t.Fatalf("the backup leaves the blocklist behind: %v", files)
	}
}

// The blocked list includes an address whose conversation was deleted, with
// the name it was saved under, so it can still be unblocked.
func TestChatBlockedListKeepsDeletedConversations(t *testing.T) {
	s := newChatTestServer(t)
	ab := client.ChatAddressString(addr(1))
	s.chat.mu.Lock()
	s.chat.contacts[ab] = "Spammer"
	s.chat.mu.Unlock()
	chatThreadAction(t, s, ab, "block")
	chatThreadAction(t, s, ab, "delete")
	rec := httptest.NewRecorder()
	s.handleChatBlocked(rec, httptest.NewRequest(http.MethodGet, "/api/chat/blocked", nil))
	var list []struct{ Addr, Name string }
	// Named as it was saved, though deleting the conversation dropped the contact.
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].Addr != ab || list[0].Name != "Spammer" {
		t.Fatalf("got %s", rec.Body.String())
	}
	chatThreadAction(t, s, ab, "unblock")
	rec = httptest.NewRecorder()
	s.handleChatBlocked(rec, httptest.NewRequest(http.MethodGet, "/api/chat/blocked", nil))
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("after unblock: %s", rec.Body.String())
	}
}
