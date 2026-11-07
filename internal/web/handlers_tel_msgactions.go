package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// EditText records an edit this account made, the way an edit from Telegram
// would arrive: new text, plain, and the time of the change.
func (c *telCache) EditText(key string, id int, text string, at int) {
	if c == nil {
		return
	}
	for _, m := range c.Thread(key) {
		if m.ID == id {
			m.Text, m.Entities, m.EditDate = text, nil, at
			c.MergeThread(key, []tgbridge.Message{m})
			return
		}
	}
}

// handleTelEdit replaces the text of one of this account's messages.
func (s *Server) handleTelEdit(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer  tgbridge.Peer `json:"peer"`
		MsgID int           `json:"msgId"`
		Text  string        `json:"text"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 || req.MsgID == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "empty message", 400)
		return
	}
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		return b.EditText(ctx, req.Peer, req.MsgID, text)
	})
	// Saving the same text again is no failure.
	if err != nil && !tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	at := int(time.Now().Unix())
	h.store().EditText(telCacheKey(req.Peer), req.MsgID, text, at)
	writeJSON(w, map[string]any{"ok": true, "editDate": at})
}

// handleTelDeleteMessages deletes messages, for this account only or, where
// Telegram allows it, for everyone.
func (s *Server) handleTelDeleteMessages(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer   tgbridge.Peer `json:"peer"`
		IDs    []int         `json:"ids"`
		Revoke bool          `json:"revoke"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 || len(req.IDs) == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		return b.DeleteMessages(ctx, req.Peer, req.IDs, req.Revoke)
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	// The same path a delete from Telegram takes, so it cannot come back.
	del := tgbridge.Deletion{IDs: req.IDs}
	if req.Peer.ChannelBased() {
		del.ChannelID = req.Peer.ID
	}
	h.store().ApplyDeletes([]tgbridge.Deletion{del})
	writeJSON(w, map[string]any{"ok": true})
}

// telForwardMax is how many messages one forward may carry: Telegram's limit.
const telForwardMax = 100

// handleTelForward forwards messages to another conversation in one call, with
// Telegram's forward options riding on it.
func (s *Server) handleTelForward(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		From    tgbridge.Peer           `json:"from"`
		IDs     []int                   `json:"ids"`
		To      tgbridge.Peer           `json:"to"`
		Options tgbridge.ForwardOptions `json:"options"`
	}
	if err := s.telDecode(r, &req); err != nil || req.From.ID == 0 || req.To.ID == 0 ||
		len(req.IDs) == 0 || len(req.IDs) > telForwardMax {
		http.Error(w, "bad request", 400)
		return
	}
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	rnd := make([]int64, len(req.IDs))
	for i := range rnd {
		rnd[i] = randomID()
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	var msgs []tgbridge.Message
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		msgs, e = b.ForwardMessages(ctx, req.From, req.IDs, rnd, req.To, req.Options)
		return e
	}); err != nil {
		h.noteYouBlocked(req.To, err)
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	if len(msgs) > 0 {
		h.store().MergeThread(telCacheKey(req.To), msgs)
	}
	if msgs == nil {
		msgs = []tgbridge.Message{}
	}
	writeJSON(w, map[string]any{"ok": true, "messages": msgs})
}
