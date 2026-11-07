package web

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gotd/td/tgerr"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// handleTelReport reports messages to Telegram, one step of Telegram's own
// dialogue per call: the answer says whether to pick a reason, add a comment,
// or that the report was taken.
func (s *Server) handleTelReport(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer    tgbridge.Peer `json:"peer"`
		IDs     []int         `json:"ids"`
		Option  []byte        `json:"option"`
		Comment string        `json:"comment"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 || len(req.IDs) == 0 || len(req.IDs) > 100 {
		http.Error(w, "bad request", 400)
		return
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	var rep *tgbridge.Report
	// Once: a report repeated on a rebuilt link would be filed twice.
	if err := h.withBridgeOnce(ctx, func(b *tgbridge.Bridge) error {
		var e error
		rep, e = b.ReportMessages(ctx, req.Peer, req.IDs, req.Option, req.Comment)
		return e
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	writeJSON(w, map[string]any{"ok": true, "report": rep})
}

// telBlockedPage is how much of the blocklist one read brings.
const telBlockedPage = 100

// handleTelBlock blocks or unblocks a person or bot (POST), or lists the
// conversations blocked from here (GET), which is what the menus read.
func (s *Server) handleTelBlock(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method == http.MethodGet {
		// refresh=1 reads Telegram's own list: one query, only when the user
		// opens the blocked list. Otherwise this is the local copy.
		if r.URL.Query().Get("refresh") != "1" {
			writeJSON(w, map[string]any{"ok": true, "blocked": h.store().BlockedPeers()})
			return
		}
		h.beginOp()
		defer h.endOp()
		ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
		defer cancel()
		total, err := h.readBlocked(ctx)
		if err != nil {
			telFail(w, h, err)
			return
		}
		h.setErr(nil)
		writeJSON(w, map[string]any{"ok": true, "blocked": h.store().BlockedPeers(), "total": total})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer    tgbridge.Peer `json:"peer"`
		Blocked bool          `json:"blocked"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 ||
		(req.Peer.Kind != tgbridge.PeerUser && req.Peer.Kind != tgbridge.PeerBot) {
		http.Error(w, "bad request", 400)
		return
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		return b.SetBlocked(ctx, req.Peer, req.Blocked)
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	// Other open tabs learn it too.
	if h.store().ApplyBlocks([]tgbridge.PeerBlock{{Peer: req.Peer, Blocked: req.Blocked}}) {
		h.s.broadcast("event: update\ndata: \"tel-blocks\"\n\n")
	}
	writeJSON(w, map[string]any{"ok": true, "blocked": req.Blocked})
}

// noteYouBlocked records a block Telegram reports while refusing a write to
// peer: blocked on another device, so the chat can say so.
func (h *telHub) noteYouBlocked(peer tgbridge.Peer, err error) {
	if tgerr.Is(err, "YOU_BLOCKED_USER") && h.store().ApplyBlocks([]tgbridge.PeerBlock{{Peer: peer, Blocked: true}}) {
		h.s.broadcast("event: update\ndata: \"tel-blocks\"\n\n")
	}
}

// noteDelivered clears a block the cache still holds for peer: Telegram just
// took a message to them, so it was lifted, here or on another device.
func (h *telHub) noteDelivered(peer tgbridge.Peer) {
	if (peer.Kind == tgbridge.PeerUser || peer.Kind == tgbridge.PeerBot) &&
		h.store().ApplyBlocks([]tgbridge.PeerBlock{{Peer: peer, Blocked: false}}) {
		h.s.broadcast("event: update\ndata: \"tel-blocks\"\n\n")
	}
}

// readBlocked reads Telegram's blocklist into the cache (one query) and tells
// the page. It returns Telegram's count of the whole list.
func (h *telHub) readBlocked(ctx context.Context) (int, error) {
	var rows []tgbridge.BlockedPeer
	total := 0
	edits := h.store().BlockEdits()
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		rows, total, e = b.Blocked(ctx, telBlockedPage)
		return e
	}); err != nil {
		return 0, err
	}
	before := blockedIDs(h.store().BlockedPeers())
	h.store().ReplaceBlocked(rows, total <= telBlockedPage, edits)
	if blockedIDs(h.store().BlockedPeers()) != before {
		h.s.broadcast("event: update\ndata: \"tel-blocks\"\n\n")
	}
	return total, nil
}

// blockedIDs is a comparable fingerprint of who is on the list.
func blockedIDs(rows []tgbridge.BlockedPeer) string {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, strconv.FormatInt(b.Peer.ID, 10))
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// handleTelInvite previews or joins a private chat by invite link: "check"
// reads where the link leads (one query), "join" joins (one query). Both only
// on the user's tap.
func (s *Server) handleTelInvite(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Link   string `json:"link"`
		Action string `json:"action"`
	}
	if err := s.telDecode(r, &req); err != nil || (req.Action != "check" && req.Action != "join") {
		http.Error(w, "bad request", 400)
		return
	}
	hash, ok := tgbridge.InviteHash(req.Link)
	if !ok {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_invite_invalid"})
		return
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	if req.Action == "check" {
		var inv *tgbridge.Invite
		if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
			var e error
			inv, e = b.CheckInvite(ctx, hash)
			return e
		}); err != nil {
			telFail(w, h, err)
			return
		}
		h.setErr(nil)
		writeJSON(w, map[string]any{"ok": true, "invite": inv})
		return
	}
	var p tgbridge.Peer
	// Once: a join that landed before the link dropped must not be sent again.
	err := h.withBridgeOnce(ctx, func(b *tgbridge.Bridge) error {
		var e error
		p, e = b.JoinInvite(ctx, hash)
		return e
	})
	if tgerr.Is(err, "USER_ALREADY_PARTICIPANT") {
		// An earlier attempt got through after all: open what was joined.
		var inv *tgbridge.Invite
		if e := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
			var e2 error
			inv, e2 = b.CheckInvite(ctx, hash)
			return e2
		}); e == nil && inv != nil && inv.Member != nil {
			p, err = *inv.Member, nil
		}
	}
	if tgerr.Is(err, "INVITE_REQUEST_SENT") {
		// Not a failure: the link needs an admin's approval, and the request
		// is now with them.
		h.setErr(nil)
		writeJSON(w, map[string]any{"ok": true, "requested": true})
		return
	}
	if err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	// In the list at once; the next refresh fills in its counters.
	h.store().MergeDialogs([]tgbridge.Dialog{{Peer: p}})
	writeJSON(w, map[string]any{"ok": true, "peer": p})
}
