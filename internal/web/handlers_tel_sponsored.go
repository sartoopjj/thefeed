package web

import (
	"context"
	"net/http"
	"time"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// Sponsored messages: Telegram's ads in channels, which the API terms require
// every client showing channels to display. They are the lowest-priority call
// there is -- the page asks only after everything else has loaded and cancels
// it the moment the user does anything -- so these handlers never touch the
// progress counters or the error state, and a failure is only logged.

// telAdTTL is how long one channel's ads are reused; Telegram asks for five
// minutes.
const telAdTTL = 5 * time.Minute

type telAdEntry struct {
	at  time.Time
	ads []tgbridge.Sponsored
}

func (h *telHub) adsFor(key string, now time.Time) ([]tgbridge.Sponsored, bool) {
	h.adsMu.Lock()
	defer h.adsMu.Unlock()
	e, ok := h.ads[key]
	if !ok || now.Sub(e.at) >= telAdTTL {
		return nil, false
	}
	return e.ads, true
}

func (h *telHub) setAds(key string, ads []tgbridge.Sponsored, now time.Time) {
	h.adsMu.Lock()
	defer h.adsMu.Unlock()
	if h.ads == nil {
		h.ads = map[string]telAdEntry{}
	}
	h.ads[key] = telAdEntry{at: now, ads: ads}
}

// handleTelSponsored returns the ads for one channel, from the cache when it
// is fresh enough.
func (s *Server) handleTelSponsored(w http.ResponseWriter, r *http.Request) {
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
	empty := []tgbridge.Sponsored{}
	if !tgbridge.WantsSponsored(req.Peer) {
		writeJSON(w, map[string]any{"ok": true, "ads": empty})
		return
	}
	key := telCacheKey(req.Peer)
	if ads, ok := h.adsFor(key, time.Now()); ok {
		if ads == nil {
			ads = empty
		}
		writeJSON(w, map[string]any{"ok": true, "ads": ads})
		return
	}
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	var ads []tgbridge.Sponsored
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		ads, e = b.SponsoredMessages(ctx, req.Peer)
		return e
	}); err != nil {
		h.s.addLog("[tel] ads: " + err.Error())
		writeJSON(w, map[string]any{"ok": false})
		return
	}
	h.setAds(key, ads, time.Now())
	if ads == nil {
		ads = empty
	}
	writeJSON(w, map[string]any{"ok": true, "ads": ads})
}

// handleTelSponsoredAct reports what the user did with an ad: saw it, opened
// its link, or reported it. Only a report is the user's own request, so only a
// report shows its failure.
func (s *Server) handleTelSponsoredAct(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Action   string `json:"action"`
		RandomID []byte `json:"randomId"`
		Option   []byte `json:"option"`
	}
	err := s.telDecode(r, &req)
	if err != nil || len(req.RandomID) == 0 ||
		(req.Action != "view" && req.Action != "click" && req.Action != "report") {
		http.Error(w, "bad request", 400)
		return
	}
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	var rep *tgbridge.SponsoredReport
	err = h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		switch req.Action {
		case "view":
			_, e = b.ViewSponsored(ctx, req.RandomID)
		case "click":
			_, e = b.ClickSponsored(ctx, req.RandomID)
		default:
			rep, e = b.ReportSponsored(ctx, req.RandomID, req.Option)
		}
		return e
	})
	if err != nil {
		if req.Action == "report" {
			telFail(w, h, err)
			return
		}
		h.s.addLog("[tel] ad " + req.Action + ": " + err.Error())
		writeJSON(w, map[string]any{"ok": false})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "report": rep})
}
