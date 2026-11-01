package web

import (
	"context"
	"net/http"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// Online status and "typing…". Telegram's API terms forbid hiding either, so
// the page reports them -- at the lowest priority: nothing here touches the
// progress counters or the error state, and a failure is only logged.

// handleTelStatus reports the user online or offline.
func (s *Server) handleTelStatus(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Online bool `json:"online"`
	}
	if err := s.telDecode(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.telQuietCall(w, h, r.Context(), "status", func(ctx context.Context, b *tgbridge.Bridge) error {
		return b.SetOnline(ctx, req.Online)
	})
}

// handleTelTyping shows "typing…" in one conversation.
func (s *Server) handleTelTyping(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer tgbridge.Peer `json:"peer"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	s.telQuietCall(w, h, r.Context(), "typing", func(ctx context.Context, b *tgbridge.Bridge) error {
		return b.SetTyping(ctx, req.Peer)
	})
}

// telQuietCall runs one low-priority call: no progress, no error on screen.
func (s *Server) telQuietCall(w http.ResponseWriter, h *telHub, parent context.Context, what string,
	call func(context.Context, *tgbridge.Bridge) error) {
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	ctx, cancel := context.WithTimeout(parent, telCallTimeout)
	defer cancel()
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error { return call(ctx, b) }); err != nil {
		h.s.addLog("[tel] " + what + ": " + err.Error())
		writeJSON(w, map[string]any{"ok": false})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
