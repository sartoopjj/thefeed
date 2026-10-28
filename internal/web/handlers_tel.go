package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// The Telegram tab: real Telegram over the DNS tunnel. Separate from the
// project's own messenger (handlers_chat.go) in protocol, account and role.

const telDirName = "tel"

// telegramSessionPath is where the gotd session (the auth key) lives. Also
// used by the backup code, which is the only thing allowed to copy it out.
func telegramSessionPath(dataDir string) string {
	return filepath.Join(dataDir, telDirName, "session.json")
}

const (
	// telConnectTimeout bounds bringing the connection up. Long on purpose: at
	// one query per second the handshake alone is minutes.
	telConnectTimeout = 15 * time.Minute
	// telCallTimeout bounds one RPC, which can hide a reconnect and a data
	// centre migration (a second full handshake).
	telCallTimeout = 8 * time.Minute
	// telLogoutRemoteTimeout bounds the one courtesy call that tells Telegram
	// to revoke the key we are deleting. Short on purpose: the sign-out itself
	// is local and already decided, so this is the longest the user should
	// ever wait to be told it is done. A link too slow to carry it in this
	// much time would not have carried it at all.
	telLogoutRemoteTimeout = 6 * time.Second
	// telIdlePoll / telMaxIdlePoll: downstream polling, backing off since every
	// poll is a DNS query.
	telIdlePoll    = 700 * time.Millisecond
	telMaxIdlePoll = 5 * time.Second
	// telRecheckTimeout bounds ONE config's metadata fetch during a re-check.
	telRecheckTimeout = 45 * time.Second
	// telRecheckFirstPaint is how long a re-check waits before answering;
	// stragglers land in the cache for the next look.
	telRecheckFirstPaint = 3 * time.Second
	// telKeepAlive pings the live connection, comfortably under the minute at
	// which Telegram drops an idle one.
	telKeepAlive = 25 * time.Second
	// telPingTimeout is how long that ping may take. It must NOT be telKeepAlive:
	// gotd sets the socket write deadline from the call's context, so a ping
	// given 25s on a link whose round trip is slower fails the write and takes
	// the whole connection down -- the same way gotd's own ping loop did.
	telPingTimeout = 4 * time.Minute
)

// Telegram app credentials: they identify the app, not the user. Empty until
// whoever ships the build registers a pair at my.telegram.org; until then the
// tab asks the user for their own.
const (
	defaultTelAPIID   = 0
	defaultTelAPIHash = ""
)

// telHub owns the Telegram bridge for this backend.
type telHub struct {
	s   *Server
	dir string

	mu sync.Mutex
	// ctx is the hub's lifetime context, set by reset (mirrors chatHub): a
	// bridge must outlive the HTTP request that started it.
	ctx context.Context
	// bridge is non-nil once connected.
	bridge *tgbridge.Bridge
	// sess is the tunnel session under the bridge, kept so it can be closed.
	sessCancel context.CancelFunc
	// serverKey records which config's tunnel we are on, so the UI can show it
	// and a config switch can be detected; name is its nickname.
	serverKey string
	name      string
	// lastCfg is the config the live (or last) connection used, so a dropped
	// link can be rebuilt without asking the user to pick again.
	lastCfg  Config
	lastNick string
	// lastErr is the most recent connect/call failure, for the UI, with
	// lastErrKey its i18n key so a non-English UI shows its own language.
	lastErr    string
	lastErrKey string
	// connecting marks a connect running in the background; the request
	// returns at once, since the handshake outlives any sensible timeout.
	connecting bool
	// connectCancel aborts a connect in flight, so the user can give up
	// instead of watching a counter they cannot stop.
	connectCancel context.CancelFunc
	// unread is the watcher's last total; unreadSeen marks that a baseline
	// exists, so the first check reports nothing.
	unread     int
	unreadSeen bool
	// cache holds conversations and messages across restarts, sealed under the
	// same key as the session. Built once on first use.
	cacheOnce sync.Once
	cache     *telCache

	// resyncedChannels remembers which unknown channels have already cost a
	// dialog-list rebuild, so one can never cost two.
	resyncedChannels map[string]bool

	// refreshing is true while a background dialog refresh is in flight.
	refreshing atomic.Bool

	// cells counts outgoing tunnel cells. Atomic, not under mu: the connect
	// path bumps it and must never take the hub lock.
	cells atomic.Int64
	// ops counts user operations in flight. A keepalive exists to stop the
	// connection going idle; an operation already proves it is not, and pinging
	// across one costs queries it needs and can kill it outright.
	ops atomic.Int64
	// lastCell is when one last went out, so a keepalive is only spent on a
	// connection that is genuinely idle.
	lastCell atomic.Int64
	// Where the message being sent has got, and what the server still holds.
	// Atomic for the same reason as cells: the tunnel reports them from a
	// path that must never take the hub lock.
	sentCells  atomic.Int64
	totalCells atomic.Int64
	downLeft   atomic.Int64
	// phase names the step in progress. The early steps carry no message, so
	// there is no total to count against and the user would otherwise watch a
	// bare number with no idea which part was slow.
	phase atomic.Value
}

func newTelHub(s *Server, dataDir string) *telHub {
	return &telHub{s: s, dir: filepath.Join(dataDir, telDirName)}
}

// infoCachePath names the stored capability payload for one server. Per
// domain, since each advertises its own handshake key.
func (h *telHub) infoCachePath(cfg Config) string {
	if cfg.Domain == "" {
		return ""
	}
	// The domain comes from the user's own config, but it still reaches a file
	// name here, so anything that could climb out of the directory goes.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		}
		return '_'
	}, cfg.Domain)
	if safe == "" || safe == "." || safe == ".." {
		return ""
	}
	return filepath.Join(h.dir, "caps", safe+".json")
}

func (h *telHub) sessionFile() tgbridge.SessionFile {
	f := tgbridge.SessionFile{Path: filepath.Join(h.dir, "session.json")}
	// Sealed at rest: the session IS the Telegram auth key, so a copy of this
	// file is the account. Loaded per call rather than cached, so turning a
	// passcode on or off takes effect without a restart.
	tc, err := loadTelCrypto(h.dir)
	if err != nil {
		log.Printf("tel: at-rest key unavailable (%v) — session left as it is on disk", err)
		return f
	}
	f.Seal, f.Open = tc.seal, tc.open
	return f
}

// refreshDialogs brings the cached conversations up to date behind an answer
// already given. One at a time, so a user tapping about cannot stack fetches
// on a link with room for one.
func (h *telHub) refreshDialogs() {
	if !h.refreshing.CompareAndSwap(false, true) {
		return
	}
	h.telSafeGo("dialog refresh", func() {
		defer h.refreshing.Store(false)
		h.mu.Lock()
		base := h.ctx
		h.mu.Unlock()
		if base == nil {
			return
		}
		// Never in front of the user. There is one tunnel, and a refresh that
		// takes it while someone is sending a message or opening a chat makes
		// the app look dead on a link that reports itself connected -- the
		// user's own request simply waits behind ours. Nothing here is urgent:
		// it runs on the next poll instead.
		if h.ops.Load() > 0 {
			return
		}
		ctx, cancel := context.WithTimeout(base, telCallTimeout)
		defer cancel()
		// Ask for the difference, not the list: with a cursor this costs five
		// integers when nothing moved, against a minute of queries to re-read
		// conversations that have not changed. Sync falls back to the list by
		// itself when the cursor is missing or too far behind.
		if err := h.Sync(ctx); err != nil {
			return
		}
	})
}

// store returns the message cache, loading it the first time it is asked for.
func (h *telHub) store() *telCache {
	h.cacheOnce.Do(func() { h.cache = loadTelCache(h.dir) })
	return h.cache
}

// apiCredentials returns the user's own app id/hash, or the bundled default.
func (h *telHub) apiCredentials() (int, string) {
	pl, err := h.s.loadProfiles()
	if err == nil && pl.TelAPIID > 0 && strings.TrimSpace(pl.TelAPIHash) != "" {
		return pl.TelAPIID, strings.TrimSpace(pl.TelAPIHash)
	}
	return defaultTelAPIID, defaultTelAPIHash
}

// reset points the hub at a new lifetime context and drops any live bridge,
// because a config/fetcher rebuild invalidates the tunnel underneath it.
func (h *telHub) reset(ctx context.Context) {
	h.disconnect()
	h.mu.Lock()
	h.ctx = ctx
	// A rebuilt hub has no idea what was unread before; start a new baseline.
	h.unread, h.unreadSeen = 0, false
	h.mu.Unlock()
	if ctx != nil {
		h.telSafeGo("watcher", func() { h.startWatch(ctx) })
	}
}

// connect brings up the tunnel and the Telegram client. It runs without the
// hub lock, which would otherwise block every status call for minutes.
func (h *telHub) connect(ctx context.Context, cfg Config, nickname string) error {
	// The bridge outlives its request, so the fetcher hangs off the hub's
	// lifetime context, not the request's.
	h.mu.Lock()
	base := h.ctx
	h.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	runCtx, cancel := context.WithCancel(base)
	// Cancelling this stops the attempt: gotd's client runs on runCtx, which
	// the request's own context does not reach.
	h.mu.Lock()
	h.connectCancel = cancel
	h.mu.Unlock()

	fail := func(err error) error {
		cancel()
		return err
	}

	// The steps below stop on the request's deadline or on a cancel.
	stepCtx, stepCancel := context.WithCancel(ctx)
	defer stepCancel()
	go func() {
		select {
		case <-runCtx.Done():
			stepCancel()
		case <-stepCtx.Done():
		}
	}()

	h.setPhase(telPhaseDiscover)
	f, err := h.s.buildChatFetcher(cfg, h.s.chatResolvers(), runCtx)
	if err != nil {
		return fail(err)
	}
	// stepCtx bounds only the steps the user waits through, never anything
	// that must outlive the request.
	// Cached per server: this payload is the same every time, and fetching it
	// is a step the user watches on every connect -- including every automatic
	// reconnect, which is most of them. A stored copy goes through the same
	// signature check as a fetched one, so the file cannot stand in for the
	// server.
	infoPath := h.infoCachePath(cfg)
	info, err := client.LoadMTProtoInfo(stepCtx, f, infoPath)
	if err != nil {
		return fail(err)
	}
	// ONE sealed session for the whole connection, handshaked here.
	//
	// The session is our encryption context with the RELAY; a tunnel is one
	// TCP connection to Telegram. Telegram closing a connection says nothing
	// about the session, so re-handshaking on every re-dial spent two queries
	// rebuilding something that had not broken. The relay replaces a session's
	// tunnel when a new one is opened on it, and closes the old one itself, so
	// a reconnect is now a single MTPOpen.
	h.setPhase(telPhaseTunnel)
	sess, err := client.NewMTProtoSessionOn(f, info, protocol.ChatCellPlainMax)
	if err != nil {
		return fail(err)
	}
	// Count every cell, this handshake's included, so the UI can prove it is
	// still moving on a one-query-per-second link.
	sess.SetOnQuery(h.countCell)
	sess.SetOnStep(h.recordStep)
	if err := sess.Handshake(stepCtx); err != nil {
		// The handshake uses the key this payload advertised, so a cached copy
		// is the first suspect when it fails: a relay that rotated its key
		// would otherwise leave the client unable to connect until the cache
		// aged out. One wasted re-fetch is the whole cost of being wrong.
		client.ForgetMTProtoInfo(infoPath)
		return fail(err)
	}

	// gotd re-dials after the auth key and on every reconnect or migration.
	var dialMu sync.Mutex
	var prev *client.TunnelConn
	dial := func(dctx context.Context, dc int) (net.Conn, error) {
		dialMu.Lock()
		old := prev
		prev = nil
		// The relay can lose the session under us -- it restarts, or sweeps an
		// idle one -- and a lost session stays dead: every later exchange on it
		// fails at once. Re-handshake rather than dial on a corpse, which is
		// two queries now against a connection that never recovers.
		if !sess.Live() {
			ns, err := client.NewMTProtoSessionOn(f, info, protocol.ChatCellPlainMax)
			if err != nil {
				dialMu.Unlock()
				return nil, err
			}
			ns.SetOnQuery(h.countCell)
			ns.SetOnStep(h.recordStep)
			h.setPhase(telPhaseTunnel)
			if err := ns.Handshake(dctx); err != nil {
				dialMu.Unlock()
				return nil, err
			}
			sess = ns
		}
		cur := sess
		dialMu.Unlock()
		if old != nil {
			// Local only: the open below tells the relay to drop it, so a
			// CLOSE would spend a query saying the same thing twice.
			go old.CloseLocal()
		}

		h.setPhase(telPhaseTunnel)
		c, err := client.DialTunnel(dctx, cur.Exchange, dc, client.TunnelOptions{
			Chunk:       cur.Chunk(),
			IdlePoll:    telIdlePoll,
			MaxIdlePoll: telMaxIdlePoll,
			// Cells in flight at once, from the config's own query rate: the
			// setting that decides the speed also bounds the window.
			Window:     cur.InFlight(),
			OnProgress: h.recordProgress,
		})
		if err != nil {
			return nil, err
		}
		h.setPhase(telPhaseTelegram)
		dialMu.Lock()
		prev = c
		dialMu.Unlock()
		return c, nil
	}

	h.setPhase(telPhaseTelegram)
	apiID, apiHash := h.apiCredentials()
	b := tgbridge.NewBridge()
	if err := b.Start(runCtx, tgbridge.Options{
		APIID:     apiID,
		APIHash:   apiHash,
		Dial:      dial,
		Session:   h.sessionFile(),
		NoUpdates: true, // the tunnel is scarce: fetch explicitly
		// Must match the relay: a production client against a test DC dies
		// deep in the handshake with "wrong DC".
		TestDCs: info.TestDCs,
		// gotd's own messages, so an MTProto failure is visible instead of
		// disappearing into its reconnect backoff.
		Log: func(msg string) {
			msg = strings.TrimSpace(msg)
			// The phase is read from every line, including the routine ones.
			h.phaseFromGotd(msg)
			// But only trouble is published. gotd narrates each connection in
			// detail, and that detail -- conversation counts, timings, data
			// centre ids -- is broadcast to every browser attached to this
			// backend. Debug mode turns the full narration back on.
			if h.debugLogs() || telLogWorthShowing(msg) {
				h.s.addLog("[tel] " + msg)
			}
		},
	}); err != nil {
		return fail(err)
	}
	// Cancelled while we were connecting: drop it rather than publish a bridge
	// the user has already given up on.
	if runCtx.Err() != nil {
		b.Stop()
		return fail(runCtx.Err())
	}
	h.mu.Lock()
	h.bridge = b
	h.sessCancel = cancel
	h.connectCancel = nil
	h.serverKey = chatServerKey(cfg.Domain)
	h.name = nickname
	h.lastCfg, h.lastNick = cfg, nickname
	h.mu.Unlock()
	h.rememberResume(chatServerKey(cfg.Domain))
	h.telSafeGo("keepalive", func() { h.keepAlive(runCtx, b) })
	return nil
}

// debugLogs reports whether the user asked for verbose logging.
func (h *telHub) debugLogs() bool {
	pl, err := h.s.loadProfiles()
	return err == nil && pl.Debug
}

// telLogWorthShowing keeps the lines that explain a failure and drops the
// running commentary.
func telLogWorthShowing(msg string) bool {
	low := strings.ToLower(msg)
	// "close" not "closed": gotd writes "Close called" when a connection is
	// torn down, and that line is what identifies a drop.
	for _, mark := range []string{"error", "failed", "restarting", "close", "warn"} {
		if strings.Contains(low, mark) {
			return true
		}
	}
	return false
}

// telSafeGo runs a long-lived background job so a panic inside it is logged
// instead of taking the whole client down with it.
func (h *telHub) telSafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.s.addLog("[tel] " + name + " stopped: " + fmt.Sprint(r))
			}
		}()
		fn()
	}()
}

// Steps of a connect, in the order they happen.
const (
	telPhaseDiscover = "discover" // fetching the server's signed capability
	telPhaseTunnel   = "tunnel"   // opening the sealed session
	telPhaseTelegram = "telegram" // Telegram's own handshake and login
	// Sub-steps inside telPhaseTelegram. On a link this slow each is minutes
	// long, and reporting them as one "connecting to Telegram" left the user
	// watching a label that never changed while the query counter restarted.
	telPhaseKeyExchange = "keyexchange" // deriving the MTProto auth key
	telPhaseSession     = "session"     // initConnection, then the login state
)

// gotdPhaseMarks maps a line gotd logs to the step it has just reached. gotd
// does not report progress any other way, so these strings are the only signal;
// an unmatched line simply leaves the phase alone, and telPhaseLabel falls back
// to the generic text, so a gotd upgrade degrades instead of breaking.
var gotdPhaseMarks = []struct {
	mark  string
	phase string
}{
	{"Generating auth key", telPhaseKeyExchange},
	{"Auth key generated", telPhaseSession},
	{"Key already exists", telPhaseSession},
	{"Restored connection", telPhaseSession},
}

// phaseFromGotd advances the reported step when gotd says it has moved on.
func (h *telHub) phaseFromGotd(msg string) {
	// Only while connecting: once connected the label belongs to whatever call
	// the user started.
	if h.currentPhase() == "" {
		return
	}
	for _, m := range gotdPhaseMarks {
		if strings.Contains(msg, m.mark) {
			h.setPhase(m.phase)
			return
		}
	}
}

func (h *telHub) setPhase(p string) {
	if cur, ok := h.phase.Load().(string); ok && cur == p {
		return
	}
	h.phase.Store(p)
	// Each phase counts something different; carrying numbers across would
	// show one phase's progress against another's total.
	h.sentCells.Store(0)
	h.totalCells.Store(0)
}

func (h *telHub) currentPhase() string {
	if p, ok := h.phase.Load().(string); ok {
		return p
	}
	return ""
}

// recordStep stores progress through a step whose length is known up front.
func (h *telHub) recordStep(done, total int) {
	h.sentCells.Store(int64(done))
	h.totalCells.Store(int64(total))
}

// recordProgress stores how far the current message has got, for the UI.
func (h *telHub) recordProgress(p client.TunnelProgress) {
	h.downLeft.Store(int64(p.DownLeft))
	// A poll carries no message of its own and so reports no total. Storing
	// that would blank the meter between one message's cells, which read as the
	// step restarting when it was only waiting.
	if p.TotalCells == 0 {
		return
	}
	h.sentCells.Store(int64(p.SentCells))
	h.totalCells.Store(int64(p.TotalCells))
}

// countCell records one outgoing cell: the progress the UI shows, and when
// the tunnel last carried anything.
func (h *telHub) countCell() {
	h.cells.Add(1)
	h.lastCell.Store(time.Now().UnixNano())
}

// idleFor reports how long the tunnel has carried nothing.
func (h *telHub) idleFor() time.Duration {
	last := h.lastCell.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

// keepAlive pings until the connection is replaced or the context ends.
func (h *telHub) keepAlive(ctx context.Context, b *tgbridge.Bridge) {
	t := time.NewTicker(telKeepAlive)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Stop as soon as this bridge is no longer the live one.
		h.mu.Lock()
		current := h.bridge == b
		h.mu.Unlock()
		if !current {
			return
		}
		// Any traffic already reset Telegram's idle timer, so a ping now would
		// only spend queries the user's own work needs. An operation in flight
		// counts as traffic even between its cells: a login step waiting on
		// Telegram's reply looks idle, and a ping there is what turned a
		// successful send-code into "engine forcibly closed".
		if h.ops.Load() > 0 || h.idleFor() < telKeepAlive {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, telPingTimeout)
		err := b.Ping(pctx)
		cancel()
		if err != nil && isDeadConnection(err) {
			// Let the next user action rebuild it; nothing to do here.
			return
		}
	}
}

// withBridge runs one Telegram call, rebuilding a connection Telegram has
// dropped for idleness rather than failing the call.
func (h *telHub) withBridge(ctx context.Context, fn func(*tgbridge.Bridge) error) error {
	return h.callBridge(ctx, true, fn)
}

// withBridgeOnce runs a call that must NEVER be repeated on a rebuilt
// connection. Every login step is one of these: asking Telegram for a code a
// second time invalidates the first code, and a burst of such requests makes
// Telegram stop delivering any code at all. A dropped connection mid-login has
// to surface as a failure so the user retries the step deliberately.
func (h *telHub) withBridgeOnce(ctx context.Context, fn func(*tgbridge.Bridge) error) error {
	return h.callBridge(ctx, false, fn)
}

func (h *telHub) callBridge(ctx context.Context, retry bool, fn func(*tgbridge.Bridge) error) error {
	b, err := h.current()
	if err != nil {
		return err
	}
	err = fn(b)
	if err == nil || !isDeadConnection(err) || !retry {
		return err
	}

	// Claim the connect flag, exactly as /api/tel/connect does. Without it
	// the hub has no bridge AND is not "connecting" for the whole rebuild --
	// the two conditions /api/tel/connect checks -- so a reconnect tapped in
	// that window builds a second bridge, and the one that loses the
	// assignment polls DNS for ever on the user's query budget.
	h.mu.Lock()
	cfg, nick := h.lastCfg, h.lastNick
	busy := h.connecting
	if !busy && cfg.Domain != "" {
		h.connecting = true
	}
	h.mu.Unlock()
	if cfg.Domain == "" {
		return err // nothing to rebuild from
	}
	if busy {
		// Someone is already building one; ours would race it. The caller
		// gets the original failure and the next call finds the new bridge.
		return err
	}
	defer func() {
		h.mu.Lock()
		h.connecting = false
		h.mu.Unlock()
	}()
	h.disconnect()
	if rerr := h.connect(ctx, cfg, nick); rerr != nil {
		return rerr
	}
	b, err2 := h.current()
	if err2 != nil {
		return err2
	}
	return fn(b)
}

func (h *telHub) disconnect() {
	h.mu.Lock()
	b, cancel := h.bridge, h.sessCancel
	// Abort a connect that is still running, too.
	if h.connectCancel != nil {
		h.connectCancel()
		h.connectCancel = nil
	}
	h.bridge, h.sessCancel = nil, nil
	h.serverKey = ""
	h.mu.Unlock()
	if b != nil {
		b.Stop()
	}
	if cancel != nil {
		cancel()
	}
}

// beginOp starts a new user-visible operation.
//
// It clears the per-MESSAGE meter but NOT the running total: the label calls
// that total "in total", and zeroing it made a step read "query 10 of 11 (1
// query in total)" -- the two halves of one line disagreeing, because the
// message meter carried over while the count behind it restarted.
func (h *telHub) beginOp() {
	h.sentCells.Store(0)
	h.totalCells.Store(0)
	h.downLeft.Store(0)
	h.ops.Add(1)
	// The last failure stops being current the moment a new operation starts,
	// or a wrong password stays on screen through the login that succeeds.
	h.setErr(nil)
}

// endOp marks a user operation finished; pair it with beginOp via defer.
func (h *telHub) endOp() {
	if h.ops.Add(-1) < 0 {
		h.ops.Store(0)
	}
}

// dropDeadBridge tears down a bridge whose transport is gone; gotd closes its
// engine and every later call returns the same error for ever.
func (h *telHub) dropDeadBridge(err error) {
	if err == nil || !isDeadConnection(err) {
		return
	}
	h.disconnect()
}

// isDeadConnection reports whether an error means the Telegram connection is
// gone for good rather than one call failing.
func isDeadConnection(err error) bool {
	if err == nil {
		return false
	}
	// The bridge itself saying it has no client. gotd's Run loop clears the
	// API handle when it exits, so from that moment EVERY call returns this
	// and nothing else: the string matching below never sees gotd's reason
	// again. Without this the hub kept a bridge that could not carry a single
	// request while still reporting itself connected -- a link that says it is
	// up while nothing, not even sending a message, gets through.
	if errors.Is(err, tgbridge.ErrNotConnected) {
		return true
	}
	msg := strings.ToLower(err.Error())
	// Only "engine was closed" is terminal: gotd's Run has exited and every
	// later call returns it for ever. "engine forcibly closed" is NOT -- it
	// means one connection generation died under a call in flight, and gotd
	// logs "Restarting connection" and builds another. Treating it as dead
	// threw away a bridge that was about to work and sent the user back to
	// picking a server mid-login. It is a lost reply instead; see isLostReply.
	return strings.Contains(msg, "engine was closed") ||
		strings.Contains(msg, "connection dead") ||
		strings.Contains(msg, "use of closed") ||
		errors.Is(err, io.EOF)
}

// isUnregisteredKey reports Telegram refusing an auth key it does not know.
// The session file is then worthless and has to go, or every start repeats it.
func isUnregisteredKey(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "AUTH_KEY_UNREGISTERED") ||
		strings.Contains(err.Error(), "SESSION_REVOKED") ||
		strings.Contains(err.Error(), "AUTH_KEY_INVALID")
}

// isLostReply reports whether a call died waiting for its answer, rather than
// being refused. Telegram may well have carried it out -- a login code that
// arrives while the UI reports a failure is exactly this -- so asking again is
// not free: a second request invalidates the code the first one delivered.
func isLostReply(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "engine forcibly closed") ||
		strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "i/o timeout")
}

// resumeServer names the config a dropped connection can be rebuilt on.
//
// Being logged in is a property of the stored session, not of a live
// connection: a transient failure must not send the user back to choosing a
// server. Empty only when none has been chosen yet, or the user disconnected
// on purpose.
func (h *telHub) resumeServer() string {
	h.mu.Lock()
	last := h.lastCfg.Domain
	h.mu.Unlock()
	if last != "" {
		return chatServerKey(last)
	}
	// Nothing in memory: a fresh client. Go back to the config the session was
	// last on, but ONLY for an account Telegram has confirmed -- otherwise
	// this spends queries reconnecting with a key that belongs to nobody.
	if !h.wasAuthorized() {
		return ""
	}
	pl, err := h.s.loadProfiles()
	if err != nil {
		return ""
	}
	key := strings.TrimSpace(pl.TelLastServer)
	if key == "" {
		return ""
	}
	// Still a config we have: "deleted or changed" must drop the default
	// rather than leave the app retrying a server that is gone.
	for _, p := range pl.Profiles {
		if strings.TrimSpace(p.Config.ServerKey) == "" {
			continue
		}
		if chatServerKey(p.Config.Domain) == key {
			return key
		}
	}
	return ""
}

// rememberAuthorized records whether Telegram has confirmed the stored session
// belongs to an account, so a dropped connection does not look like a logout
// and a dead key does not look like a login.
func (h *telHub) rememberAuthorized(ok bool) {
	h.s.profilesMu.Lock()
	defer h.s.profilesMu.Unlock()
	pl, err := h.s.loadProfiles()
	if err != nil || pl.TelAuthorized == ok {
		return
	}
	pl.TelAuthorized = ok
	_ = h.s.saveProfiles(pl)
}

// wasAuthorized reports the last confirmed answer.
func (h *telHub) wasAuthorized() bool {
	pl, err := h.s.loadProfiles()
	return err == nil && pl.TelAuthorized
}

// rememberResume stores the config across restarts.
func (h *telHub) rememberResume(key string) {
	h.s.profilesMu.Lock()
	defer h.s.profilesMu.Unlock()
	pl, err := h.s.loadProfiles()
	if err != nil || pl.TelLastServer == key {
		return
	}
	pl.TelLastServer = key
	_ = h.s.saveProfiles(pl)
}

// forgetResume drops the remembered config, so the picker comes back. Only an
// explicit disconnect does this.
func (h *telHub) forgetResume() {
	h.mu.Lock()
	h.lastCfg, h.lastNick = Config{}, ""
	h.mu.Unlock()
	h.rememberResume("")
}

// current returns the live bridge, or an error the UI can show.
func (h *telHub) current() (*tgbridge.Bridge, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bridge == nil {
		return nil, tgbridge.ErrNotConnected
	}
	return h.bridge, nil
}

func (h *telHub) setErr(err error) {
	key := telStatusKey(err)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		h.lastErr, h.lastErrKey = "", ""
		return
	}
	h.lastErr, h.lastErrKey = err.Error(), key
}

// randomID is Telegram's message dedup id: a retry carrying the same id is
// dropped rather than delivered twice, which matters on a lossy tunnel.
func randomID() int64 { return rand.Int63() }

// telWipeSession deletes the stored auth key.
func (h *telHub) wipeSession() error {
	// The conversations belong to the account being signed out of. Leaving
	// them would show one person's chats to whoever signs in next.
	h.store().Forget()
	h.forgetResume()
	h.rememberAuthorized(false)
	err := os.Remove(h.sessionFile().Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Server) telDecode(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}

func telAtoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

// recordMTPAdv stores a config's MTProto-advertised bit, but only from
// signature-verified metadata: an unverified bit must never claim a tunnel.
func (s *Server) recordMTPAdv(serverKey string, advertised, verified bool) {
	if serverKey == "" || !verified {
		return
	}
	s.mtpAdvMu.Lock()
	if s.mtpAdv == nil {
		s.mtpAdv = make(map[string]chatAdvEntry)
	}
	s.mtpAdv[serverKey] = chatAdvEntry{advertised: advertised, at: time.Now()}
	s.mtpAdvMu.Unlock()
}

// mtpAdvVerified returns the last verified tunnel bit for serverKey and
// whether it is still fresh.
func (s *Server) mtpAdvVerified(serverKey string) (advertised, fresh bool) {
	s.mtpAdvMu.Lock()
	defer s.mtpAdvMu.Unlock()
	e, ok := s.mtpAdv[serverKey]
	if !ok {
		// Never asked. Not a negative either: the caller shows "checking".
		return false, false
	}
	// A stale bit is NOT a negative. Only a VERIFIED answer may say a server
	// carries no tunnel: going quiet is what a slow link does, and expiring a
	// confirmed yes into a no condemns the config that just worked.
	return e.advertised, time.Since(e.at) < chatAdvTTL
}

// configForServerKey finds the imported config pinned to a server key.
func (s *Server) configForServerKey(key string) (Config, string, bool) {
	if key == "" {
		return Config{}, "", false
	}
	pl, err := s.loadProfiles()
	if err != nil {
		return Config{}, "", false
	}
	for _, p := range pl.Profiles {
		if strings.TrimSpace(p.Config.ServerKey) == "" {
			continue
		}
		if chatServerKey(p.Config.Domain) == key {
			return p.Config, p.Nickname, true
		}
	}
	return Config{}, "", false
}

// setAPICredentials stores the user's own Telegram app credentials.
func (s *Server) setTelAPICredentials(id int, hash string) error {
	s.profilesMu.Lock()
	defer s.profilesMu.Unlock()
	pl, err := s.loadProfiles()
	if err != nil {
		return err
	}
	pl.TelAPIID = id
	pl.TelAPIHash = strings.TrimSpace(hash)
	return s.saveProfiles(pl)
}

func telAtoi64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ---------------------------------------------------------------- watcher ---
// A backgrounded WebView is suspended, so the frontend's poll stops. This
// polls in the backend and hands the count to the platform notifier, like the
// messenger does. Off by default: every check spends queries.

const (
	// telWatchInterval is the gap between background checks.
	telWatchInterval = 5 * time.Minute
	// telWatchTimeout bounds one check, well under the interval so a stalled
	// link cannot pile checks on top of each other.
	telWatchTimeout = 4 * time.Minute
	// telWatchDialogs is how many conversations a check looks at: enough to
	// cover anything recently active, far short of a full page.
	telWatchDialogs = 20
)

// telWatchEnabled reports whether background checking is on: the user's
// choice when they have made one, otherwise the default (on).
func (h *telHub) telWatchEnabled() bool {
	pl, err := h.s.loadProfiles()
	if err != nil {
		return false
	}
	return pl.TelWatch == nil || *pl.TelWatch
}

// setTelWatch stores the user's choice. Writing it explicitly is what stops
// the default from reasserting itself on the next load.
func (s *Server) setTelWatch(on bool) error {
	s.profilesMu.Lock()
	defer s.profilesMu.Unlock()
	pl, err := s.loadProfiles()
	if err != nil {
		return err
	}
	pl.TelWatch = &on
	return s.saveProfiles(pl)
}

// startWatch runs the background check until ctx ends; it does nothing while
// the setting is off.
func (h *telHub) startWatch(ctx context.Context) {
	t := time.NewTicker(telWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.watchOnce(ctx)
	}
}

// watchOnce counts unread messages and reports any growth to the platform.
func (h *telHub) watchOnce(parent context.Context) {
	if !h.telWatchEnabled() {
		return
	}
	// Never brings the tunnel up on its own.
	if _, err := h.current(); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, telWatchTimeout)
	defer cancel()

	// The cheap question first: five integers, no messages. Most of the time
	// the answer is "nothing moved" and the poll ends here, which is the whole
	// reason this is not a dialog fetch.
	var st tgbridge.UpdateState
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		st, e = b.State(ctx)
		return e
	}); err != nil {
		return
	}

	// And that is ALL the background does. Fetching the difference here would
	// pull the account's entire message traffic through a sixteen-byte-a-query
	// link to raise one notification, and a notification needs only the count.
	// WHICH conversations changed is worth asking when someone is looking --
	// that is Sync's job.
	//
	// The cursor is deliberately left where it is: it marks how far the
	// messages have been read, and nothing was read here.
	h.noteUnread(st.Unread)
}

// Sync brings the conversations up to date, for when the user is actually
// looking. Unlike the background poll this does fetch messages -- but only the
// ones that changed, never the last message of every conversation.
func (h *telHub) Sync(ctx context.Context) error {
	var st tgbridge.UpdateState
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		st, e = b.State(ctx)
		return e
	}); err != nil {
		return err
	}
	cur := h.store().Cursor()
	if cur.Known() && st.Pts == cur.Pts && st.Qts == cur.Qts {
		h.recordUnread(st.Unread)
		return nil // nothing moved
	}

	var ch *tgbridge.Changes
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		ch, e = b.Changes(ctx, cur)
		return e
	}); err != nil {
		return err
	}
	if ch == nil {
		return nil
	}
	if ch.Resync {
		// Too far behind to describe as a difference (or no cursor yet): the
		// only way back is the expensive list, once.
		h.resyncFromDialogs(ctx, st)
		return nil
	}
	// An update naming a channel we have never heard of means the account
	// joined it elsewhere. A difference carries messages, not memberships, so
	// this is the one thing worth paying for the whole list over.
	//
	// Strictly once per channel: the list is the most expensive call there is
	// and it runs on the one tunnel, so a channel that stayed unknown would
	// re-trigger it every poll and starve everything the user is doing.
	if fresh := h.unresyncedChannels(ch.Channels); len(fresh) > 0 {
		h.markChannelsResynced(fresh)
		h.resyncFromDialogs(ctx, st)
		return nil
	}
	if len(ch.Gone) > 0 {
		keys := make([]string, 0, len(ch.Gone))
		for _, p := range ch.Gone {
			keys = append(keys, telCacheKey(p))
		}
		h.store().ForgetPeer(keys)
	}
	// Muting FIRST, before the arrivals are counted: ApplyNew asks whether a
	// conversation is muted to decide whether its new messages may announce
	// themselves, so a channel silenced elsewhere must already be silent here
	// -- otherwise it rings once more on the very poll that learns it is muted.
	h.store().ApplyMutes(ch.Mutes)
	n := h.store().ApplyNew(ch.New)
	h.store().ApplyReads(ch.Reads)
	// Reads made on another device, which is the only way an unread badge here
	// can come down without the user opening the chat here too.
	h.store().ApplyInReads(ch.InReads)
	h.store().SetCursor(ch.State)
	h.repairNotify(ctx)
	unread := ch.State.Unread
	if unread == 0 {
		unread = st.Unread
	}
	// Recorded, not announced: the difference counted the arrivals exactly,
	// and letting the unread total announce them too would show them twice.
	h.recordUnread(unread)
	if n > 0 {
		h.s.notifyNewMessages(n)
	}
	return nil
}

// repairNotify asks Telegram about the conversations the cache had to invent.
//
// A difference names a conversation and carries its message, but never says
// whether the user silenced it -- so a row built from one starts with no
// answer, and "no answer" must not be shown as "not muted": that makes a
// channel the user deliberately silenced look like one that may interrupt.
// Most of a busy account's rows arrive this way, which is how most of the
// list ended up looking audible.
//
// Telegram takes a VECTOR of peers here, so repairing a hundred rows is one
// round trip, not a hundred -- the only reason this is affordable at all on a
// link measured in queries per second.
func (h *telHub) repairNotify(ctx context.Context) {
	peers := h.store().UnknownNotify(tgbridge.MaxPeerStates)
	if len(peers) == 0 {
		return
	}
	var rows []tgbridge.Dialog
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		rows, e = b.PeerStates(ctx, peers)
		return e
	}); err != nil {
		// Left marked unknown on purpose: the next poll tries again, and
		// until then the row says "not asked" rather than "not muted".
		return
	}
	h.store().MergeDialogs(rows)
	// Asked is asked. A merged row carries the real setting and is no longer
	// unknown; one Telegram did not describe never will be, and must stop
	// costing a call every poll.
	asked := make([]string, 0, len(peers))
	for _, p := range peers {
		asked = append(asked, telCacheKey(p))
	}
	h.store().ClearNotifyUnknown(asked)
	h.s.broadcast("event: update\ndata: \"tel\"\n\n")
}

// unresyncedChannels returns the channels the cached list has never seen AND
// that have not already cost us a rebuild. Both halves matter: the first finds
// a channel joined elsewhere, the second makes sure it can only ever do so
// once.
func (h *telHub) unresyncedChannels(chans []tgbridge.Peer) []string {
	if len(chans) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, d := range h.store().Dialogs() {
		have[telCacheKey(d.Peer)] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var fresh []string
	for _, p := range chans {
		key := telCacheKey(p)
		if have[key] || h.resyncedChannels[key] {
			continue
		}
		fresh = append(fresh, key)
	}
	return fresh
}

func (h *telHub) markChannelsResynced(keys []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.resyncedChannels == nil {
		h.resyncedChannels = map[string]bool{}
	}
	// Bounded: this only ever grows by channels joined on another device, but
	// a long-running client should not hold an unbounded set either.
	if len(h.resyncedChannels) > 256 {
		h.resyncedChannels = map[string]bool{}
	}
	for _, k := range keys {
		h.resyncedChannels[k] = true
	}
}

// resyncFromDialogs rebuilds the list the expensive way and plants a cursor so
// the next poll can go back to asking for differences.
func (h *telHub) resyncFromDialogs(ctx context.Context, st tgbridge.UpdateState) {
	var page *tgbridge.DialogPage
	if err := h.withBridge(ctx, func(b *tgbridge.Bridge) error {
		var e error
		page, e = b.Dialogs(ctx, tgbridge.DialogFilter{Limit: telWatchDialogs})
		return e
	}); err != nil || page == nil {
		return
	}
	// MERGED, not replaced. This page is twenty rows fetched to re-establish
	// the cursor -- it is not the conversation list. Replacing with it threw
	// away everything past the twentieth, including every page the user had
	// paid DNS queries for with "load more chats", and did so on any resync:
	// a cursor gap, or a single post from a channel the cache had not seen.
	h.store().MergeDialogs(page.Dialogs)
	h.store().SetDialogTotal(page.TotalKnown)
	// Deliberately NOT SetDialogsComplete: this fetches a short page on
	// purpose, to re-establish the cursor. It says nothing about whether the
	// list has an end past it.
	// Only now: a cursor stored before the list would claim the conversations
	// are current when they had not been fetched.
	h.store().SetCursor(st)
	total := 0
	for _, d := range page.Dialogs {
		total += d.Unread
	}
	h.noteUnread(total)
}

// noteUnread records the account's unread total and announces a genuine rise.
// Used where the total is the ONLY evidence of new messages.
func (h *telHub) noteUnread(total int) {
	if n := h.recordUnread(total); n > 0 {
		h.s.notifyNewMessages(n)
	}
}

// recordUnread stores the total and reports the rise without announcing it.
func (h *telHub) recordUnread(total int) int {
	h.mu.Lock()
	prev, seen := h.unread, h.unreadSeen
	h.unread, h.unreadSeen = total, true
	h.mu.Unlock()
	return unreadDelta(prev, seen, total)
}

// unreadDelta is how many messages arrived since the last check. The first
// check only sets a baseline, and a falling count (the user read elsewhere) is
// not an arrival.
func unreadDelta(prev int, seen bool, total int) int {
	if !seen || total <= prev {
		return 0
	}
	return total - prev
}
