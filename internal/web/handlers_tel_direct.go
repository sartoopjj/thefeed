package web

import (
	"context"
	"net/http"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// DirectFor is the cached direct-messages conversation of a channel.
func (c *telCache) DirectFor(key string) (tgbridge.Peer, bool) {
	if c == nil {
		return tgbridge.Peer{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.data.Directs[key]
	return p, ok
}

func (c *telCache) SetDirect(key string, p tgbridge.Peer) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.data.Directs == nil {
		c.data.Directs = map[string]tgbridge.Peer{}
	}
	c.data.Directs[key] = p
	c.mu.Unlock()
	c.save()
}

// handleTelDirect finds the conversation for writing to a channel's admins:
// from the cache, or with one call the first time.
func (s *Server) handleTelDirect(w http.ResponseWriter, r *http.Request) {
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
	if req.Peer.DirectID == 0 {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_no_direct"})
		return
	}
	key := telCacheKey(req.Peer)
	// Only while it is still the channel's: a new one replaces it.
	if p, ok := h.store().DirectFor(key); ok && p.ID == req.Peer.DirectID {
		writeJSON(w, map[string]any{"ok": true, "peer": p})
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
	var p tgbridge.Peer
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		p, e = b.DirectMessages(ctx, req.Peer)
		return e
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	h.store().SetDirect(key, p)
	writeJSON(w, map[string]any{"ok": true, "peer": p})
}
