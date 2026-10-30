package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tgerr"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// HTTP surface for the "Tel" tab. Every call is bounded: an RPC over the
// tunnel is many DNS queries, and a hung request would wedge the UI.

func (s *Server) telHubOrFail(w http.ResponseWriter) *telHub {
	if s.tel == nil {
		http.Error(w, "telegram bridge not available", http.StatusNotFound)
		return nil
	}
	return s.tel
}

// handleTelInfo reports what the UI needs to choose a screen: is a session
// stored, are we connected, which configs can carry the tunnel.
func (s *Server) handleTelInfo(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	h.mu.Lock()
	// Having a bridge is not the same as having a connection. gotd's Run loop
	// clears the client when it exits, and the bridge object stays behind
	// answering every call with "not connected" -- so reporting "connected"
	// from its mere existence is what let the UI show a live link while
	// nothing, not even sending a message, could get through. The bridge is
	// only published once Start has succeeded, so this cannot flap during a
	// connect.
	connected := h.bridge != nil && h.bridge.Running()
	serverKey := h.serverKey
	lastErr, lastErrKey := h.lastErr, h.lastErrKey
	connecting := h.connecting
	h.mu.Unlock()
	cells := h.cells.Load()

	apiID, apiHash := h.apiCredentials()
	resp := map[string]any{
		// Whether an account is signed in -- confirmed by Telegram, not guessed
		// from a file that exists whenever gotd has minted any key at all.
		"hasSession": h.wasAuthorized(),
		"connected":  connected,
		"server":     serverKey,
		// Without app credentials nothing can connect, so the UI asks for them
		// first rather than letting the bridge fail with a raw error.
		"hasCredentials": apiID > 0 && apiHash != "",
		// Shared backend: the session lives on the machine running the client,
		// so every browser pointed here shares the account.
		"shared": s.sharedBackend,
		// Echoed so a typo is visible and fixable; these identify the app, not
		// the account.
		"apiId":   apiID,
		"apiHash": apiHash,
		// Background checking, so the menu can show its state.
		"watch": h.telWatchEnabled(),
	}
	// connecting + cells let the UI show that a slow link is still moving
	// rather than an unmoving spinner.
	resp["connecting"] = connecting
	resp["cells"] = cells
	// Where the message in flight has got, and what is still coming back, so
	// the UI shows a bar rather than a number that only climbs.
	resp["phase"] = h.currentPhase()
	// Set when a dropped connection can be rebuilt without asking the user
	// which server to use again.
	resp["resumeServer"] = h.resumeServer()
	resp["sentCells"] = h.sentCells.Load()
	resp["totalCells"] = h.totalCells.Load()
	resp["downLeft"] = h.downLeft.Load()
	if lastErr != "" {
		resp["error"] = lastErr
		resp["reason"] = lastErrKey
	}
	// Configs whose server advertises the tunnel, from the signed metadata
	// bit — no probing, so opening the tab costs nothing.
	var servers []map[string]any
	if pl, err := s.loadProfiles(); err == nil {
		for _, p := range pl.Profiles {
			if strings.TrimSpace(p.Config.ServerKey) == "" {
				continue
			}
			key := chatServerKey(p.Config.Domain)
			advertised, fresh := s.mtpAdvVerified(key)
			servers = append(servers, map[string]any{
				"key":    key,
				"name":   p.Nickname,
				"domain": p.Config.Domain,
				// A server that once said yes stays offered even when the bit
				// is stale; "verified" carries whether we have checked lately,
				// so the UI can mark it unconfirmed instead of hiding it.
				"tunnel":   advertised,
				"verified": fresh,
			})
		}
	}
	resp["servers"] = servers
	writeJSON(w, resp)
}

// handleTelConnect brings up the tunnel on one config.
func (s *Server) handleTelConnect(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Server string `json:"server"`
	}
	_ = s.telDecode(r, &req)

	cfg, nickname, ok := s.configForServerKey(chatServerKey(req.Server))
	if !ok {
		http.Error(w, "unknown config", http.StatusBadRequest)
		return
	}
	// A new attempt makes the previous failure stale; this path does not go
	// through beginOp, so it clears it itself.
	h.setErr(nil)

	h.mu.Lock()
	if h.bridge != nil {
		h.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "already": true})
		return
	}
	if h.connecting {
		h.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "connecting": true})
		return
	}
	h.connecting = true
	// The only place the running total restarts: it counts what THIS
	// connection has cost, which is what the UI reports "in total".
	h.cells.Store(0)
	h.lastErr, h.lastErrKey = "", ""
	base := h.ctx
	h.mu.Unlock()

	// Runs in the background and answers now: the handshake outlives any
	// browser timeout. The UI follows /api/tel/info.
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, telConnectTimeout)
	h.mu.Lock()
	h.connectCancel = cancel
	h.mu.Unlock()
	go func() {
		defer cancel()
		err := h.connect(ctx, cfg, nickname)
		h.mu.Lock()
		h.connecting = false
		h.connectCancel = nil
		h.mu.Unlock()
		h.setErr(err)
	}()
	writeJSON(w, map[string]any{"ok": true, "connecting": true})
}

// handleTelCredentials stores the user's own app credentials, which identify
// an app to Telegram rather than a user.
func (s *Server) handleTelCredentials(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		APIID   int    `json:"apiId"`
		APIHash string `json:"apiHash"`
	}
	if err := s.telDecode(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if req.APIID <= 0 || strings.TrimSpace(req.APIHash) == "" {
		http.Error(w, "api id and hash required", 400)
		return
	}
	oldID, oldHash := h.apiCredentials()
	changed := oldID != req.APIID || oldHash != strings.TrimSpace(req.APIHash)
	if err := s.setTelAPICredentials(req.APIID, req.APIHash); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// A stored session belongs to the app that created it: Telegram ties the
	// authorization to the api id it was signed in with. Keeping it across a
	// credential change connects under one app identity with a key issued to
	// another, and does so silently until something refuses it.
	//
	// So the session goes with the credentials and the user signs in again --
	// better said out loud than as a login that fails days later. An unchanged
	// save, the common case, signs nobody out.
	if changed {
		h.disconnect()
		if err := h.wipeSession(); err != nil {
			s.addLog("[tel] credentials changed but the session would not go: " + err.Error())
		}
	}
	writeJSON(w, map[string]any{"ok": true, "signedOut": changed})
}

// cachedDialogPage wraps cached rows as a page, INCLUDING whether another one
// exists and where it continues from. Built from the rows alone it would claim
// "no more pages", and since the cache serves every unforced first page that
// hides "load more chats" entirely.
//
// The rule is the live path's own: a short page ends the list. The cursor comes
// from the last cached row, which carries what Telegram needs to continue.
func cachedDialogPage(cached []tgbridge.Dialog, limit, total int, complete bool) *tgbridge.DialogPage {
	page := &tgbridge.DialogPage{Dialogs: cached, TotalKnown: total}
	// Telegram saying "that was the last page" is the only trustworthy end
	// marker. The count is not one: it includes rows this client cannot map,
	// so "held < total" can stay true however many pages are fetched -- a list
	// stuck on "197 of 198" with a button that fetches nothing.
	//
	// It is still worth showing: forty rows of three hundred and forty of
	// forty look identical without it.
	if complete {
		return page
	}
	if total > 0 {
		if len(cached) >= total {
			return page
		}
	} else if limit <= 0 || len(cached) < limit {
		return page
	}
	last := cached[len(cached)-1]
	// Without a top message there is nothing to page from; claiming another
	// page exists would hand the user a button that fetches the same rows.
	if last.TopMsg == 0 {
		return page
	}
	page.More = true
	page.NextID = last.TopMsg
	page.NextDate = last.When
	page.NextPeer = last.Peer
	return page
}

// handleTelRecheck re-reads each config's metadata, since the tunnel bit
// otherwise only arrives with ordinary feed traffic.
func (s *Server) handleTelRecheck(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	pl, err := s.loadProfiles()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_generic"})
		return
	}
	// One fetch per config, in parallel, waiting only for a first paint: a
	// dead config must not hold the answer for its whole timeout.
	h.mu.Lock()
	bg := h.ctx
	h.mu.Unlock()
	if bg == nil {
		bg = context.Background()
	}
	resolvers := s.chatResolvers()
	var wg sync.WaitGroup
	for _, p := range pl.Profiles {
		if strings.TrimSpace(p.Config.ServerKey) == "" {
			continue
		}
		wg.Add(1)
		go func(cfg Config) {
			defer wg.Done()
			// Detached from the request: it outlives the answer below.
			fctx, cancel := context.WithTimeout(bg, telRecheckTimeout)
			defer cancel()
			f, err := s.buildChatFetcher(cfg, resolvers, fctx)
			if err != nil {
				return
			}
			// The metadata callback records the verified bit; the result here
			// is irrelevant, so a failure leaves the old answer alone.
			_, _ = f.FetchMetadata(fctx)
		}(p.Config)
	}
	waitUpTo(&wg, telRecheckFirstPaint)

	// Answer with whatever has landed, so the UI updates immediately.
	s.handleTelInfo(w, r)
}

func (s *Server) handleTelDisconnect(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	// POST, like every other endpoint that changes something. The cross-site
	// guard deliberately lets GET through -- a read cannot be an attack -- so
	// without this any page the user happened to open could tear down their
	// tunnel with an <img src> pointed at the loopback port.
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	h.disconnect()
	// Chosen deliberately, so stop offering to resume it.
	h.forgetResume()
	writeJSON(w, map[string]any{"ok": true})
}

// handleTelAuth drives the login steps. The code is sent by Telegram to the
// user's own device and typed here; it never passes through the relay.
func (s *Server) handleTelAuth(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Action   string `json:"action"` // status | code | signin | password | logout
		Phone    string `json:"phone"`
		Code     string `json:"code"`
		CodeHash string `json:"codeHash"`
		Password string `json:"password"`
	}
	if err := s.telDecode(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	// Signing out is handled BEFORE the connection check, and outside the
	// operation lock.
	//
	// Every other action here asks Telegram something and is meaningless
	// without a link. Signing out is the opposite: the account lives in a file
	// on this machine, and removing it is local. Refusing it when nothing is
	// connected meant that during a shutdown -- exactly when someone most
	// needs their account off their device -- the app answered "not
	// connected" and kept the session.
	if req.Action == "logout" {
		s.telLogout(w, r, h)
		return
	}
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "not connected", "reason": "tel_err_not_connected"})
		return
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()

	switch req.Action {
	case "status":
		var st *auth.Status
		err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
			var e error
			st, e = b.AuthStatus(ctx)
			return e
		})
		if err != nil {
			// Telegram says this key belongs to nobody. That happens when a
			// connect without a session made gotd mint a fresh key: the file
			// exists but is useless, and keeping it would send the user round
			// this loop again on the next start.
			if isUnregisteredKey(err) {
				h.rememberAuthorized(false)
				_ = h.wipeSession()
			}
			telFail(w, h, err)
			return
		}
		// A confirmed answer, remembered: a later connection drop must not be
		// mistaken for a logout.
		h.rememberAuthorized(st.Authorized)
		resp := map[string]any{"ok": true, "authorized": st.Authorized}
		if st.User != nil {
			resp["user"] = map[string]any{
				"id": st.User.ID, "firstName": st.User.FirstName,
				"username": st.User.Username,
			}
		}
		writeJSON(w, resp)

	case "code":
		var hash, via string
		err := h.withBridgeOnce(ctx, func(b *tgbridge.Bridge) error {
			var e error
			hash, via, e = b.SendCode(ctx, strings.TrimSpace(req.Phone))
			return e
		})
		if err != nil {
			telFailAs(w, h, err, "tel_err_lost_code_hash")
			return
		}
		writeJSON(w, map[string]any{"ok": true, "codeHash": hash, "via": via})

	case "resend":
		phone := strings.TrimSpace(req.Phone)
		var hash, via string
		err := h.withBridgeOnce(ctx, func(b *tgbridge.Bridge) error {
			var e error
			hash, via, e = b.ResendCode(ctx, phone, req.CodeHash)
			// resendCode needs a next delivery method; without one Telegram
			// says SEND_CODE_UNAVAILABLE, so ask for a fresh code instead.
			if e != nil && tgerr.Is(e, "SEND_CODE_UNAVAILABLE") {
				hash, via, e = b.SendCode(ctx, phone)
			}
			return e
		})
		if err != nil {
			telFailAs(w, h, err, "tel_err_lost_code_hash")
			return
		}
		writeJSON(w, map[string]any{"ok": true, "codeHash": hash, "via": via})

	case "signin":
		if err := h.withBridgeOnce(ctx, func(b *tgbridge.Bridge) error {
			return b.SignIn(ctx, strings.TrimSpace(req.Phone), strings.TrimSpace(req.Code), req.CodeHash)
		}); err != nil {
			// A 2FA account needs the password step next; say so explicitly
			// rather than looking like a wrong code.
			if isPasswordNeeded(err) {
				// The hint comes from the same connection, so fetch it while
				// we are still on it.
				var hint string
				_ = h.withBridge(ctx, func(b *tgbridge.Bridge) error {
					hint = b.PasswordHint(ctx)
					return nil
				})
				writeJSON(w, map[string]any{"ok": false, "needPassword": true, "hint": hint})
				return
			}
			telFail(w, h, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})

	case "password":
		if err := h.withBridgeOnce(ctx, func(b *tgbridge.Bridge) error {
			return b.SignInPassword(ctx, req.Password)
		}); err != nil {
			// SRP parameters belong to the connection, so a rebuilt link needs
			// the step repeating.
			telFail(w, h, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "unknown action", 400)
	}
}

// telLogout removes the account from THIS DEVICE, then tells Telegram if it
// can. Never gated on a connection: see handleTelAuth.
func (s *Server) telLogout(w http.ResponseWriter, r *http.Request, h *telHub) {
	// The account comes off this device first, before anything that can hang
	// or fail. Deleting the key and the conversations is the part that matters
	// and the part that cannot go wrong: it is a file on this machine.
	wipeErr := h.wipeSession()

	// Revoking the key at Telegram is worth doing and not worth waiting for:
	// withBridge REBUILDS a dead connection first, so on a down link this
	// would sit through a fifteen-minute reconnect to deliver a message that
	// only matters if it arrives.
	//
	// gotd keeps the auth key in memory, so the bridge can still carry the
	// revocation after the file is gone.
	var logoutErr error
	if _, err := h.current(); err != nil {
		// Nothing to send it over. Not a failure of the sign-out: the account
		// is already off this device.
		logoutErr = err
	} else {
		lctx, lcancel := context.WithTimeout(r.Context(), telLogoutRemoteTimeout)
		logoutErr = h.withBridgeOnce(lctx, func(b *tgbridge.Bridge) error {
			return b.LogOut(lctx)
		})
		lcancel()
	}
	h.disconnect()

	// ok reports what the user asked for: is the account off this device?
	// Whether Telegram heard about it is separate, and reported separately.
	resp := map[string]any{"ok": wipeErr == nil}
	if logoutErr != nil {
		resp["remoteError"] = logoutErr.Error()
	}
	if wipeErr != nil {
		resp["error"] = wipeErr.Error()
	}
	writeJSON(w, resp)
}

// handleTelDialogs lists conversations.
func (s *Server) handleTelDialogs(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	connected := true
	if _, err := h.current(); err != nil {
		connected = false
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()

	q := r.URL.Query()
	filter := tgbridge.DialogFilter{
		Limit:      telAtoi(q.Get("limit"), 40),
		OffsetDate: telAtoi(q.Get("offsetDate"), 0),
		OffsetID:   telAtoi(q.Get("offsetId"), 0),
	}
	if p, ok := telPeerFromQuery(q, "offset"); ok {
		filter.OffsetPeer = p
	}
	// A warm cache answers at once, and the refresh runs behind the answer.
	// Fetching the list costs about a minute on a degraded link, and almost
	// all of that is spent re-reading conversations that have not changed.
	//
	// refresh=1 is the user asking Telegram, not us. Without it the cache
	// answered every first page for ever, so a conversation started elsewhere
	// -- a channel joined on the phone, a chat opened from another client --
	// never appeared no matter how many times the list was reloaded.
	firstPage := filter.OffsetID == 0 && filter.OffsetDate == 0
	forced := q.Get("refresh") == "1"
	// cacheOnly never touches the bridge, so it can be served while a connect
	// or another operation holds the tunnel. That is what lets a freshly
	// loaded page show the conversations it already has instead of an empty
	// section, without competing for the one link.
	if q.Get("cacheOnly") == "1" {
		cached := h.store().Dialogs()
		if len(cached) == 0 {
			writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_no_cache"})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "cached": true,
			"page": cachedDialogPage(cached, filter.Limit, h.store().DialogTotal(), h.store().DialogsComplete())})
		return
	}
	if firstPage && !forced {
		if cached := h.store().Dialogs(); len(cached) > 0 {
			// Only refresh when there is a link to refresh over; the answer
			// itself does not depend on one. Conversations do not disappear
			// because the tunnel did.
			if connected {
				h.refreshDialogs()
			}
			writeJSON(w, map[string]any{"ok": true, "cached": true,
				"page": cachedDialogPage(cached, filter.Limit, h.store().DialogTotal(), h.store().DialogsComplete())})
			return
		}
	}
	if !connected {
		// A forced refresh with no link still wants the list it has: the
		// conversations did not go anywhere, the tunnel did.
		if cached := h.store().Dialogs(); firstPage && len(cached) > 0 {
			writeJSON(w, map[string]any{"ok": true, "cached": true,
				"page": cachedDialogPage(cached, filter.Limit, h.store().DialogTotal(), h.store().DialogsComplete())})
			return
		}
		// Nothing cached and nowhere to ask.
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}

	var page *tgbridge.DialogPage
	err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		page, e = b.Dialogs(ctx, filter)
		return e
	})
	if err != nil {
		// A list we already have beats an error screen: the conversations did
		// not go anywhere, the link did. Served with cached:true so the UI can
		// say the counts may be stale instead of claiming they are current.
		if cached := h.store().Dialogs(); len(cached) > 0 {
			h.setErr(err)
			writeJSON(w, map[string]any{"ok": true, "cached": true,
				"page": cachedDialogPage(cached, filter.Limit, h.store().DialogTotal(), h.store().DialogsComplete())})
			return
		}
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	// Only a first page replaces the list; a later one would truncate what it
	// was extending.
	if page != nil {
		// Telegram said whether another page follows. A page that does not
		// fill the limit IS the end of the list, and that is the only end
		// marker worth keeping: the dialog COUNT includes rows this client
		// drops, so it can never be reached. A first page that ends the list
		// also completes it -- an account with fewer conversations than one
		// page has no second page to ask for.
		h.store().SetDialogsComplete(!page.More)
	}
	if page != nil && firstPage {
		// Merged for the same reason as the resync: this is the FIRST page of
		// a longer list, not the list. Replacing with it dropped every row
		// past the fortieth each time the list was refreshed -- the pages the
		// user had asked for, gone. Conversations that really are over are
		// removed by the Gone path instead, which Telegram reports outright.
		h.store().MergeDialogs(page.Dialogs)
		// Telegram's count of the whole list, kept so the cached answer can
		// still tell a partial list from a complete one on the next start.
		h.store().SetDialogTotal(page.TotalKnown)
		// The list we just read IS a sync point, so record where the update
		// stream had got to. Without this the cursor stayed at zero, every
		// later poll had to resync from the whole list, and the cheap
		// difference that keeps the list current was never reachable.
		if !h.store().Cursor().Known() {
			var st tgbridge.UpdateState
			if e := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
				var err error
				st, err = b.State(ctx)
				return err
			}); e == nil {
				h.store().SetCursor(st)
			}
		}
	} else if page != nil {
		// A later page, which the user asked for with "load more chats". It
		// cost a page of DNS queries, so it is kept: without this the next
		// start was back to the first forty rows and the press was wasted.
		h.store().MergeDialogs(page.Dialogs)
	}
	writeJSON(w, map[string]any{"ok": true, "page": page})
}

// handleTelHistory fetches messages for one conversation.
func (s *Server) handleTelHistory(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	q := r.URL.Query()
	peer, ok := telPeerFromQuery(q, "")
	if !ok {
		http.Error(w, "bad peer", 400)
		return
	}
	// cacheOnly touches no tunnel, so an open conversation can show the
	// messages it already has at once instead of a blank pane for the half
	// minute a page of history costs over DNS. The live fetch follows.
	if q.Get("cacheOnly") == "1" {
		if cached := h.store().Thread(telCacheKey(peer)); len(cached) > 0 {
			writeJSON(w, map[string]any{"ok": true, "cached": true, "messages": cached})
			return
		}
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_no_cache"})
		return
	}
	if _, err := h.current(); err != nil {
		// No link. The messages we already hold are still the messages: answer
		// from the cache rather than with an error, which is what made a
		// restart -- or a page reload before the tunnel was back up -- look
		// like every conversation had been wiped. Newest page only: paging
		// back is a request for messages we are saying we do not have.
		if telAtoi(q.Get("offsetId"), 0) == 0 {
			if cached := h.store().Thread(telCacheKey(peer)); len(cached) > 0 {
				writeJSON(w, map[string]any{"ok": true, "cached": true, "messages": cached})
				return
			}
		}
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	// A poll of an open conversation also asks Telegram what changed, which
	// is the only way edits to messages already held arrive: history only
	// returns ids above the cursor. Deferred before endOp so it runs after
	// it -- the refresh stands aside while any op is in flight.
	if telAtoi(q.Get("offsetId"), 0) == 0 && telAtoi(q.Get("minId"), 0) > 0 {
		defer h.refreshDialogs()
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	var msgs []tgbridge.Message
	err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		msgs, e = b.History(ctx, peer, telAtoi(q.Get("limit"), 30),
			telAtoi(q.Get("offsetId"), 0), telAtoi(q.Get("minId"), 0))
		return e
	})
	if err != nil {
		// Same rule as the list: show what we have rather than an empty thread,
		// which is what made a dropped link look like a channel with no posts.
		// Only for the newest page -- paging back is a request for messages we
		// are saying we do not have.
		if telAtoi(q.Get("offsetId"), 0) == 0 {
			if cached := h.store().Thread(telCacheKey(peer)); len(cached) > 0 {
				h.setErr(err)
				writeJSON(w, map[string]any{"ok": true, "cached": true, "messages": cached})
				return
			}
		}
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	h.store().MergeThread(telCacheKey(peer), msgs)
	writeJSON(w, map[string]any{"ok": true, "messages": msgs})
}

// handleTelSend sends a text message.
func (s *Server) handleTelSend(w http.ResponseWriter, r *http.Request) {
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
		Text string        `json:"text"`
		// ReplyTo answers an earlier message; 0 is an ordinary send.
		ReplyTo int `json:"replyTo"`
	}
	if err := s.telDecode(r, &req); err != nil {
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
	var sentID int
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		sentID, e = b.SendText(ctx, req.Peer, text, randomID(), req.ReplyTo)
		return e
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	// The id Telegram assigned, so the UI can settle the message it already
	// drew instead of spending a second round trip reading it back.
	if sentID > 0 {
		h.store().MergeThread(telCacheKey(req.Peer), []tgbridge.Message{{
			ID: sentID, Text: text, Out: true, Date: int(time.Now().Unix()),
			ReplyTo: req.ReplyTo,
		}})
	}
	writeJSON(w, map[string]any{"ok": true, "msgId": sentID})
}

// handleTelDelete removes a conversation: leaving a channel, or clearing a
// private/group history. Destructive, so the UI confirms first.
func (s *Server) handleTelDelete(w http.ResponseWriter, r *http.Request) {
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
		// Revoke also deletes the history for the other side, where Telegram
		// permits it. Off unless the user asked.
		Revoke bool `json:"revoke"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 {
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
		return b.DeleteChat(ctx, req.Peer, req.Revoke)
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	writeJSON(w, map[string]any{"ok": true})
}

// handleTelResolve finds a peer by @username, so a conversation can be started
// with someone who is not in the dialog list yet.
func (s *Server) handleTelResolve(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := s.telDecode(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		http.Error(w, "username required", 400)
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
	var peer tgbridge.Peer
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		peer, e = b.ResolveUsername(ctx, req.Username)
		return e
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	writeJSON(w, map[string]any{"ok": true, "peer": peer})
}

// handleTelWatch turns background checking on or off.
func (s *Server) handleTelWatch(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := s.telDecode(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.setTelWatch(req.On); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// A fresh opt-in starts from today: messages already sitting unread are
	// not an arrival.
	h.mu.Lock()
	h.unread, h.unreadSeen = 0, false
	h.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "watch": req.On})
}

// handleTelPeer re-reads one conversation's row for the read mark behind the
// second tick. One row instead of a page.
func (s *Server) handleTelPeer(w http.ResponseWriter, r *http.Request) {
	h := s.telHubOrFail(w)
	if h == nil {
		return
	}
	peer, ok := telPeerFromQuery(r.URL.Query(), "")
	if !ok {
		http.Error(w, "bad peer", 400)
		return
	}
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	// Marked as an operation like every other call that takes the tunnel.
	// Without it the keepalive ping can fire into this RPC -- the very thing
	// beginOp exists to prevent, and what broke send-code.
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	var d tgbridge.Dialog
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		d, e = b.PeerState(ctx, peer)
		return e
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	writeJSON(w, map[string]any{"ok": true, "dialog": d})
}

// handleTelRead clears a conversation's unread count. Separate from history so
// reading older messages never marks anything read by accident.
func (s *Server) handleTelRead(w http.ResponseWriter, r *http.Request) {
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
		MaxID int           `json:"maxId"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	// Same as every other call that takes the one tunnel: the keepalive must
	// not fire into it.
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		return b.MarkRead(ctx, req.Peer, req.MaxID)
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	writeJSON(w, map[string]any{"ok": true})
}

// telFail answers with an i18n key rather than an English sentence; the raw
// text still rides along for the log.
// telFailAs is telFail with a better reason for a LOST REPLY on this specific
// step. Telegram hands back the code's hash in the reply we never received,
// and nothing can fetch it afterwards -- so a code that arrives is unusable
// and the generic "enter the one that arrived" advice cannot be followed.
func telFailAs(w http.ResponseWriter, h *telHub, err error, lostKey string) {
	if !isLostReply(err) {
		telFail(w, h, err)
		return
	}
	h.s.addLog("[tel] failed: " + err.Error())
	h.setErr(err)
	h.dropDeadBridge(err)
	writeJSON(w, map[string]any{"ok": false, "reason": lostKey, "error": err.Error()})
}

func telFail(w http.ResponseWriter, h *telHub, err error) {
	// Always log the real text. The UI shows a translated key, so without this
	// an error we have not mapped is invisible everywhere.
	h.s.addLog("[tel] failed: " + err.Error())
	h.setErr(err)
	// A connection that is gone must not look like one failing call: drop it
	// so the UI offers reconnecting instead of steps that can only fail.
	h.dropDeadBridge(err)
	writeJSON(w, map[string]any{"ok": false, "reason": telStatusKey(err), "error": err.Error()})
}

// telStatusKey maps a bridge error to the UI's i18n key, mirroring how chat
// does it (chatStatusKey). Unknown errors fall back to the generic key.
func telStatusKey(err error) string {
	switch {
	case err == nil:
		return ""
	case isBadAPICredentials(err):
		return "tel_err_bad_credentials"
	// Telegram names the exact login failure; a generic error hides whether
	// the code or the connection is at fault.
	case tgerr.Is(err, "PHONE_CODE_INVALID"):
		return "tel_err_code_invalid"
	case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
		return "tel_err_code_expired"
	case tgerr.Is(err, "PHONE_CODE_EMPTY"):
		return "tel_err_code_empty"
	case tgerr.Is(err, "PHONE_NUMBER_INVALID"):
		return "tel_err_phone_invalid"
	case tgerr.Is(err, "PHONE_NUMBER_BANNED"):
		return "tel_err_phone_banned"
	case tgerr.Is(err, "PHONE_PASSWORD_FLOOD", "FLOOD_WAIT"):
		return "tel_err_flood"
	// Same story: gotd returns a typed ErrPasswordInvalid rather than the
	// Telegram error name.
	case errors.Is(err, auth.ErrPasswordInvalid), tgerr.Is(err, "PASSWORD_HASH_INVALID"):
		return "tel_err_password_invalid"
	case tgerr.Is(err, "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID"):
		return "tel_err_username"
	case tgerr.Is(err, "PEER_FLOOD"):
		return "tel_err_peer_flood"
	case tgerr.Is(err, "AUTH_RESTART"):
		return "tel_err_auth_restart"
	case tgerr.Is(err, "SEND_CODE_UNAVAILABLE"):
		return "tel_err_send_code_unavailable"
	// Last of the named causes: the call never got its answer back. Telegram
	// may have carried it out anyway, so the user has to be told to look before
	// asking again. Placed after every Telegram error name so a real refusal is
	// always reported as itself.
	case isLostReply(err):
		return "tel_err_lost_reply"
	case tgerr.Is(err, "API_ID_INVALID", "API_ID_PUBLISHED_FLOOD"):
		return "tel_err_bad_credentials"
	case errors.Is(err, client.ErrMTProtoNotOffered):
		return "tel_err_not_offered"
	case errors.Is(err, client.ErrMTProtoServerDisabled):
		return "tel_err_server_disabled"
	case errors.Is(err, client.ErrMTProtoNoServerKey):
		return "tel_err_no_key"
	case errors.Is(err, client.ErrMTProtoUnverified):
		return "tel_err_unverified"
	case errors.Is(err, client.ErrMTProtoUnreachable):
		return "tel_err_unreachable"
	case errors.Is(err, tgbridge.ErrNotConnected):
		return "tel_err_not_connected"
	case isDeadConnection(err):
		return "tel_err_disconnected"
	}
	return "tel_err_generic"
}

// telPeerFromQuery rebuilds a peer from query parameters; prefix selects a
// second one ("offset" → offsetKind, offsetId, offsetAccessHash).
func telPeerFromQuery(q url.Values, prefix string) (tgbridge.Peer, bool) {
	field := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + strings.ToUpper(name[:1]) + name[1:]
	}
	id := telAtoi64(q.Get(field("id")))
	if id == 0 {
		return tgbridge.Peer{}, false
	}
	kind := tgbridge.PeerKind(q.Get(field("kind")))
	switch kind {
	case tgbridge.PeerUser, tgbridge.PeerBot, tgbridge.PeerGroup,
		tgbridge.PeerChannel, tgbridge.PeerForum:
	default:
		return tgbridge.Peer{}, false
	}
	return tgbridge.Peer{Kind: kind, ID: id, AccessHash: telAtoi64(q.Get(field("accessHash")))}, true
}

// isBadAPICredentials reports whether Telegram rejected the app's credentials.
func isBadAPICredentials(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "API_ID_INVALID") ||
		strings.Contains(msg, "API_ID_PUBLISHED_FLOOD") ||
		strings.Contains(msg, "API_KEY_INVALID")
}

// isPasswordNeeded reports whether sign-in stopped on two-step verification.
// gotd replaces SESSION_PASSWORD_NEEDED with a typed error reading "2FA
// required", so the typed check is the one that fires.
func isPasswordNeeded(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		return true
	}
	return tgerr.Is(err, "SESSION_PASSWORD_NEEDED") ||
		strings.Contains(strings.ToUpper(err.Error()), "SESSION_PASSWORD_NEEDED")
}

// handleTelMute turns notifications for one conversation on or off.
func (s *Server) handleTelMute(w http.ResponseWriter, r *http.Request) {
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
		Muted bool          `json:"muted"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()

	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		return b.SetMuted(ctx, req.Peer, req.Muted)
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	// Reflected locally at once: the setting is Telegram's, but waiting for a
	// round trip to redraw a switch the user just flipped would feel broken on
	// a link this slow.
	h.store().SetMuted(telCacheKey(req.Peer), req.Muted)
	writeJSON(w, map[string]any{"ok": true, "muted": req.Muted})
}

// handleTelStats re-reads view and reaction counts for messages already on
// screen. Counters only: no message bodies cross the link, so a whole screen
// costs a fraction of re-reading the page.
func (s *Server) handleTelStats(w http.ResponseWriter, r *http.Request) {
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
		IDs  []int         `json:"ids"`
		// "views" or "reactions": two Telegram methods, so two requests, so
		// two progress bars that can each name what they are fetching.
		What string `json:"what"`
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
	var stats []tgbridge.MessageStats
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		stats, e = b.MessageStatsFor(ctx, req.Peer, req.IDs, req.What)
		return e
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	h.store().ApplyStats(telCacheKey(req.Peer), stats)
	writeJSON(w, map[string]any{"ok": true, "stats": stats})
}

// handleTelReact sets or clears this account's reaction on one message. An
// empty emoji removes it, which is how tapping your own choice again works.
func (s *Server) handleTelReact(w http.ResponseWriter, r *http.Request) {
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
		Emoji string        `json:"emoji"`
	}
	if err := s.telDecode(r, &req); err != nil || req.Peer.ID == 0 || req.MsgID == 0 {
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
	var rs []tgbridge.Reaction
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		rs, e = b.SendReaction(ctx, req.Peer, req.MsgID, req.Emoji)
		return e
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	// Telegram's own count is the truth, so the cache takes it rather than the
	// optimistic one the UI drew.
	h.store().ApplyStats(telCacheKey(req.Peer),
		[]tgbridge.MessageStats{{ID: req.MsgID, Reactions: rs}})
	writeJSON(w, map[string]any{"ok": true, "reactions": rs, "msgId": req.MsgID})
}

// handleTelJoin subscribes to a channel the user is currently only reading.
func (s *Server) handleTelJoin(w http.ResponseWriter, r *http.Request) {
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
	if _, err := h.current(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "reason": "tel_err_not_connected"})
		return
	}
	h.beginOp()
	defer h.endOp()
	ctx, cancel := context.WithTimeout(r.Context(), telCallTimeout)
	defer cancel()
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		return b.JoinChannel(ctx, req.Peer)
	}); err != nil {
		telFail(w, h, err)
		return
	}
	h.setErr(nil)
	writeJSON(w, map[string]any{"ok": true})
}
