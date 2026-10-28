package web

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Chat web layer. One identity (seed) per data dir, shared across profiles;
// one thread per contact with messages tagged by the server they travelled
// through. Contact names never leave this machine.

const (
	chatDirName = "chat"
	// chatPollInterval is the idle background poll cadence (no UI watching).
	chatPollInterval = 90 * time.Second
	// chatPollActiveInterval is the cadence used while a UI is open: the backend
	// is the SOLE timer-based poller (the frontend never polls on a timer — only
	// when the user taps refresh), so query volume is independent of how many
	// tabs/clients are connected.
	chatPollActiveInterval = 20 * time.Second
	// chatPollFastInterval tightens the cadence to 10s right after activity (a
	// message sent or received within chatActivityWindow), for a snappy
	// back-and-forth, then relaxes back to chatPollActiveInterval.
	chatPollFastInterval = 10 * time.Second
	// chatActivityWindow: a send/receive within this window counts as "active"
	// and selects chatPollFastInterval.
	chatActivityWindow = 60 * time.Second
	chatPollFirstWait  = 45 * time.Second
	// chatEnableRegisterAttempts: register+poll retries on enable (background loop).
	chatEnableRegisterAttempts = 20
	// chatAvailCacheTTL: report a confirmed-reachable server available without
	// re-probing for this long.
	chatAvailCacheTTL = 5 * time.Minute
	// chatEnableProbeTimeout bounds the synchronous register on enable.
	chatEnableProbeTimeout = 30 * time.Second
	// chatAvailFirstPaint is how long GET /api/chat/availability waits before
	// answering. It only buys a nicer first paint: probes keep running in the
	// background and each pushes its own result over SSE as it lands. MUST stay
	// well under the frontend's CHAT_AVAIL_TIMEOUT_MS abort, or the UI throws
	// the response away and reports "no servers" while a working one exists.
	chatAvailFirstPaint = 2 * time.Second
	// chatAvailProbeTimeout is one background probe's budget. Deliberately
	// generous: on a bad network a working config can take tens of seconds, and
	// giving up on it would report a live server as dead.
	chatAvailProbeTimeout = 45 * time.Second
	// chatPollServerTimeout bounds ONE server's inbox poll in the BACKGROUND
	// loop. Draining an inbox is tens to hundreds of sequential DNS round
	// trips, and FetchInbox discards everything it decoded if the deadline
	// fires, so a tight budget means a backlog can never drain at all. Servers
	// are polled in parallel and a server already being polled is skipped, so
	// a long budget costs nothing. The manual refresh passes its own.
	chatPollServerTimeout = 5 * time.Minute
	// chatPollConcurrency: servers polled at once. Independent inboxes, so they
	// need not wait on each other; capped to stay fair to the resolver pool.
	chatPollConcurrency = 4
	// chatBackoffMax caps the escalating backoff for a VERIFIED negative (this
	// server really has no messenger).
	chatBackoffMax = 30 * time.Minute
	// chatTransientBackoffMax caps escalation for everything else. A plain loss
	// of connectivity must not park a healthy server for half an hour: at the
	// 20s active cadence that would be ~100 seconds of bad network buying 30
	// minutes of silence, with nothing polling to discover the recovery.
	chatTransientBackoffMax = 5 * time.Minute
	// chatEnableSyncAttempts: register tries within the synchronous window before
	// reporting failure (background then continues to chatEnableRegisterAttempts).
	chatEnableSyncAttempts = 5
)

// chatIdentityFile is chat/identity.json.
type chatIdentityFile struct {
	Seed     string `json:"seed"` // base32, no padding
	BackedUp bool   `json:"backedUp,omitempty"`
}

// chatStoredMsg is one thread message on disk.
type chatStoredMsg struct {
	Dir       string `json:"dir"` // "in" | "out"
	Seq       uint32 `json:"seq"`
	Text      string `json:"text"`
	TS        int64  `json:"ts"`
	Server    string `json:"server,omitempty"`    // server key (main feed domain)
	LocalAddr string `json:"localAddr,omitempty"` // our address when this msg was stored
}

// chatThreadFile is one peer's conversation on disk.
type chatThreadFile struct {
	Msgs       []chatStoredMsg   `json:"msgs"`
	LastOutSeq map[string]uint32 `json:"lastOutSeq,omitempty"` // per server key
	Unread     int               `json:"unread,omitempty"`
	Pinned     bool              `json:"pinned,omitempty"`
	// Server is the chat server (profile main domain) this conversation is
	// bound to — chosen when the chat was started, used for sending.
	Server string `json:"server,omitempty"`
	// Accepted/Delivered cache the server's ✓/✓✓ high-water seq PER server key.
	// Seq numbering is per server (see LastOutSeq), so a single counter can't
	// represent two servers — switching the send server would otherwise blank
	// the old server's delivered ticks. LastAccepted/LastDelivered are the
	// legacy single-server fields, migrated into the maps on load.
	Accepted      map[string]uint32 `json:"accBy,omitempty"`
	Delivered     map[string]uint32 `json:"delBy,omitempty"`
	LastAccepted  uint32            `json:"acc,omitempty"`
	LastDelivered uint32            `json:"del,omitempty"`
	// ServerSetAt: when the user last switched send server — the UI shows the
	// "peer switched" banner only for peer messages newer than this.
	ServerSetAt int64 `json:"serverSetAt,omitempty"`
	// AckedIn is the highest INCOMING seq we have acked from this peer, per
	// server. A malicious server can re-serve an old, authentic envelope; any
	// incoming seq ≤ this watermark was already delivered+acked, so we drop it
	// instead of rendering it twice — even after local history is cleared (the
	// message-list scan can't catch that). Acks are contiguous (FetchInbox holds
	// at a gap), so nothing legitimate is ever below the mark.
	AckedIn map[string]uint32 `json:"ackedIn,omitempty"`
	// Emojis caches the safety-code emojis so the messages endpoint can
	// return them without a DNS round trip.
	Emojis []string `json:"emojis,omitempty"`
}

// chatThreadsFile is chat/threads.json.
type chatThreadsFile struct {
	Threads map[string]*chatThreadFile `json:"threads"`
}

// bumpStatus raises the per-server ✓/✓✓ high-water seq for server (never lowers
// it — ticks don't regress). Returns whether anything changed.
func (th *chatThreadFile) bumpStatus(server string, accepted, delivered uint32) bool {
	if th.Accepted == nil {
		th.Accepted = map[string]uint32{}
	}
	if th.Delivered == nil {
		th.Delivered = map[string]uint32{}
	}
	changed := false
	if accepted > th.Accepted[server] {
		th.Accepted[server] = accepted
		changed = true
	}
	if delivered > th.Delivered[server] {
		th.Delivered[server] = delivered
		changed = true
	}
	return changed
}

// chatStatusMap returns a snapshot copy of m (never nil), so the JSON response
// marshals {} rather than null and isn't raced by concurrent counter bumps.
func chatStatusMap(m map[string]uint32) map[string]uint32 {
	out := make(map[string]uint32, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// chatPostSendServer decides a conversation's bound server after a send.
// current is the binding now, boundAtStart the binding when the send began,
// sentVia the server the message actually went out on. It follows a reroute off
// a disabled bound server, but preserves a deliberate switch the user made
// mid-send (current != boundAtStart). Returns the new binding and whether it
// changed.
func chatPostSendServer(current, boundAtStart, sentVia string) (string, bool) {
	if current == "" {
		return sentVia, true
	}
	if current == boundAtStart && boundAtStart != sentVia {
		return sentVia, true
	}
	return current, false
}

// replayedIn reports whether an incoming seq from server was already
// delivered+acked, so a re-served copy must be dropped (not re-rendered). Acks
// are contiguous, so anything at/below the watermark was definitely seen.
func (th *chatThreadFile) replayedIn(server string, seq uint32) bool {
	return seq <= th.AckedIn[server]
}

// resetAckedIn clears the incoming-replay watermark for server, used when a
// server-side seq regression is detected (server reset its store).
func (th *chatThreadFile) resetAckedIn(server string) {
	if th.AckedIn != nil {
		delete(th.AckedIn, server)
	}
}

// markAckedIn raises the incoming-replay watermark for server; reports whether
// it moved (so the caller knows to persist).
func (th *chatThreadFile) markAckedIn(server string, seq uint32) bool {
	if th.AckedIn == nil {
		th.AckedIn = map[string]uint32{}
	}
	if seq > th.AckedIn[server] {
		th.AckedIn[server] = seq
		return true
	}
	return false
}

// migrateStatus folds legacy single-server counters into the per-server maps
// under the thread's bound server, so ticks survive the upgrade.
func (th *chatThreadFile) migrateStatus() {
	if th.Accepted != nil || th.Delivered != nil {
		return
	}
	if th.Server == "" || (th.LastAccepted == 0 && th.LastDelivered == 0) {
		return
	}
	th.bumpStatus(th.Server, th.LastAccepted, th.LastDelivered)
	th.LastAccepted, th.LastDelivered = 0, 0
}

// perServerChat is one chat-enabled profile's client. The same identity is
// reused across every server, so the user has one address everywhere.
// backoffUntil is mutated from concurrent poll/availability goroutines, so it
// is guarded by chatHub.mu (see backedOffLocked / setBackoff).
type perServerChat struct {
	serverKey    string // lowercased main domain (provenance tag)
	name         string // profile nickname, renamed in place (guarded by chatHub.mu)
	domain       string
	client       *client.ChatClient
	f            *client.Fetcher    // its fetcher, so resolvers can be re-synced live
	backoffUntil time.Time          // skip polling until this time (guarded by chatHub.mu)
	lastReason   string             // i18n key of the last probe failure (guarded by chatHub.mu)
	lastOKAt     time.Time          // last confirmed-reachable time; positive availability cache (guarded by chatHub.mu)
	lastLimits   map[string]any     // advertised limits from the last good probe, so the cached path can still report them (guarded by chatHub.mu)
	failures     int                // consecutive failures, for escalating backoff (guarded by chatHub.mu)
	pollMu       sync.Mutex         // one poll at a time for THIS server
	scorer       *chatBudgetScorer  // adaptive cell-budget selection (guarded by chatHub.mu)
	cfg          Config             // the config it was built from, to detect edits
	cancel       context.CancelFunc // stops the fetcher's goroutines when dropped/replaced
}

func (ps *perServerChat) stop() {
	if ps.cancel != nil {
		ps.cancel()
	}
}

// chatHub owns the web-layer chat state. Chat is multi-server: it polls and
// sends across EVERY profile that pins a server key, so switching the active
// feed profile never cuts off conversations on another server.
type chatHub struct {
	s   *Server
	dir string

	mu        sync.Mutex
	identity  *client.ChatIdentity
	backedUp  bool
	ctx       context.Context
	servers   map[string]*perServerChat // by serverKey
	activeKey string                    // the active profile's server key
	contacts  map[string]string
	threads   map[string]*chatThreadFile
	// enabled is the set of server keys the user has explicitly opted in to.
	// Only these are polled and registered on (publishing the chat pubkey), so
	// an untrusted feed server never silently receives the user's identity.
	enabled map[string]bool
	// lastResolvers is the resolver list last pushed to the chat fetchers; chat
	// resolvers track the feed's live healthy pool, re-synced only on change.
	lastResolvers []string
	// lastActivity stamps the most recent send or received message. The poll loop
	// tightens to chatPollFastInterval while this is within chatActivityWindow.
	lastActivity time.Time
	// pollKick wakes runPollLoop out of its sleep: send true to poll immediately
	// (a UI client just connected), false to only reschedule + resync the
	// frontend countdown (a manual refresh already polled).
	pollKick chan bool
	// budget is the per-cell op-plaintext budget B applied to every server's
	// client (RFC §8.2): the size/count trade-off the user picks via a preset.
	budget int
	// budgetMode is "compact"/"standard"/"wide" (fixed, using budget) or "auto",
	// in which each server's scorer picks a preset per send by recent success.
	budgetMode string

	// probing is the set of servers with a background availability probe in
	// flight, so repeated list opens don't stack duplicate probes.
	probing map[string]bool

	sendMu sync.Mutex // one upload at a time (uplink is scarce)
}

func newChatHub(s *Server, dataDir string) *chatHub {
	h := &chatHub{
		s:          s,
		dir:        filepath.Join(dataDir, chatDirName),
		servers:    make(map[string]*perServerChat),
		contacts:   make(map[string]string),
		threads:    make(map[string]*chatThreadFile),
		enabled:    make(map[string]bool),
		probing:    make(map[string]bool),
		pollKick:   make(chan bool, 1),
		budget:     protocol.ChatCellPlainSize, // Standard preset (fixed-mode fallback)
		budgetMode: chatBudgetModeAuto,         // default: cost-scored adaptive pick
	}
	h.loadState()
	return h
}

func chatServerKey(domain string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
}

var chatSeedEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

func (h *chatHub) loadState() {
	if raw, err := os.ReadFile(filepath.Join(h.dir, "identity.json")); err == nil {
		var f chatIdentityFile
		if json.Unmarshal(raw, &f) == nil {
			if seed, err := chatSeedEnc.DecodeString(strings.ToUpper(f.Seed)); err == nil && len(seed) == protocol.SeedSize {
				if id, err := client.NewChatIdentity(seed); err == nil {
					h.identity = id
					h.backedUp = f.BackedUp
				}
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(h.dir, "contacts.json")); err == nil {
		_ = json.Unmarshal(raw, &h.contacts)
	}
	if raw, err := os.ReadFile(filepath.Join(h.dir, "threads.json")); err == nil {
		var f chatThreadsFile
		if json.Unmarshal(raw, &f) == nil && f.Threads != nil {
			h.threads = f.Threads
			for _, th := range h.threads {
				th.migrateStatus()
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(h.dir, "servers.json")); err == nil {
		var en map[string]bool
		if json.Unmarshal(raw, &en) == nil && en != nil {
			h.enabled = en
		}
	}
	if raw, err := os.ReadFile(filepath.Join(h.dir, "settings.json")); err == nil {
		var st struct {
			Budget int    `json:"budget"`
			Mode   string `json:"mode"`
		}
		if json.Unmarshal(raw, &st) == nil {
			if st.Budget != 0 {
				h.budget = clampChatBudget(st.Budget)
			}
			if st.Mode != "" {
				h.budgetMode = st.Mode
			}
		}
	}
}

// applyBudget chooses and applies the cell budget for a send on ps. In auto
// mode the server's scorer picks a preset and the chosen arm is returned (to be
// scored after the send); otherwise the fixed preset is applied and arm = -1.
func (h *chatHub) applyBudget(ps *perServerChat) int {
	h.mu.Lock()
	arm := -1
	budget := h.budget
	if h.budgetMode == chatBudgetModeAuto && ps.scorer != nil {
		arm = ps.scorer.pick(time.Now(), rand.Float64())
		budget = ps.scorer.arms[arm].Budget
	}
	h.mu.Unlock()
	ps.client.SetBudget(budget)
	return arm
}

// chatBudgetSuccess reports whether a send's cells reached the server: nil error
// or any server-status reply (the request got through and was answered). Only a
// transport error (unreachable/timeout) counts against the budget.
func chatBudgetSuccess(err error) bool {
	if err == nil {
		return true
	}
	var serr *client.ChatStatusError
	return errors.As(err, &serr)
}

// recordBudget folds a send's query/error counts into the server's scorer (auto
// mode only).
func (h *chatHub) recordBudget(ps *perServerChat, arm, queries, errs int, success bool) {
	if arm < 0 {
		return
	}
	h.mu.Lock()
	if ps.scorer != nil {
		ps.scorer.record(arm, queries, errs, success, time.Now())
	}
	h.mu.Unlock()
}

// clampChatBudget bounds a budget to the valid range.
func clampChatBudget(b int) int {
	if b < protocol.ChatCellPlainMin {
		return protocol.ChatCellPlainMin
	}
	if b > protocol.ChatCellPlainMax {
		return protocol.ChatCellPlainMax
	}
	return b
}

// saveBudgetLocked persists settings.json. Caller holds mu.
func (h *chatHub) saveBudgetLocked() {
	_ = h.writeJSONAtomic("settings.json", map[string]any{"budget": h.budget, "mode": h.budgetMode})
}

// writeJSONAtomic persists v at path via tmp+rename.
func (h *chatHub) writeJSONAtomic(name string, v any) error {
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(h.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveIdentityLocked persists identity.json. Caller holds mu.
func (h *chatHub) saveIdentityLocked() {
	if h.identity == nil {
		return
	}
	_ = h.writeJSONAtomic("identity.json", chatIdentityFile{
		Seed:     strings.ToLower(chatSeedEnc.EncodeToString(h.identity.Seed)),
		BackedUp: h.backedUp,
	})
}

func (h *chatHub) saveContactsLocked() {
	_ = h.writeJSONAtomic("contacts.json", h.contacts)
}

func (h *chatHub) saveThreadsLocked() {
	_ = h.writeJSONAtomic("threads.json", chatThreadsFile{Threads: h.threads})
}

func (h *chatHub) saveEnabledLocked() {
	_ = h.writeJSONAtomic("servers.json", h.enabled)
}

// reset rebuilds the per-server chat clients from the current profiles and
// (re)starts the poll loop on the new lifetime context. Called from
// initFetcher on every config/profile change.
func (h *chatHub) reset(ctx context.Context) {
	// Run on a separate goroutine: reset is called from initFetcher while it
	// holds Server.mu (write), and rebuildServersLocked → chatResolvers takes
	// Server.mu (read). A sync.RWMutex is NOT reentrant, so doing that on the
	// same goroutine would deadlock and hang the client at startup.
	go func() {
		h.mu.Lock()
		h.ctx = ctx
		h.rebuildServersLocked()
		h.mu.Unlock()
		h.runPollLoop(ctx)
	}()
}

// rebuildServersLocked builds one ChatClient per profile that pins a server
// key (chat is fail-closed: no sk → no chat). The shared identity is reused.
// Caller holds mu.
func (h *chatHub) rebuildServersLocked() {
	for _, ps := range h.servers {
		ps.stop()
	}
	h.servers = make(map[string]*perServerChat)
	h.syncServersLocked()
}

// syncServersLocked reconciles h.servers with the profiles that pin a server
// key: new ones are added, removed or changed ones dropped/rebuilt, and the
// rest left alone so their backoff, limits and scorer survive. Adding or
// editing a non-active config reaches chat only through here: it doesn't
// re-init the fetcher, so reset never runs. Caller holds mu.
func (h *chatHub) syncServersLocked() {
	h.activeKey = ""
	if h.identity == nil || h.ctx == nil {
		return
	}
	pl, err := h.s.loadProfiles()
	if err != nil {
		return
	}
	want := make(map[string]Profile)
	var order []string
	for _, p := range pl.Profiles {
		if strings.TrimSpace(p.Config.ServerKey) == "" {
			continue
		}
		key := chatServerKey(p.Config.Domain)
		if _, dup := want[key]; key == "" || dup {
			continue // the first profile for a server wins
		}
		want[key] = p
		order = append(order, key)
		if p.ID == pl.Active {
			h.activeKey = key
		}
	}
	for key, ps := range h.servers {
		if p, ok := want[key]; !ok || !sameChatConfig(ps.cfg, p.Config) {
			ps.stop()
			delete(h.servers, key)
		}
	}
	var shared []string
	gotShared := false
	for _, key := range order {
		p := want[key]
		if ps := h.servers[key]; ps != nil {
			ps.name = p.Nickname // a rename shows up in the server picker
			continue
		}
		if !gotShared {
			shared, gotShared = h.s.chatResolvers(), true
		}
		// Use shared resolvers first; fall back to this profile's own
		// resolvers so chat works even if the active config has none.
		resolvers := shared
		if len(resolvers) == 0 {
			resolvers = p.Config.Resolvers
		}
		ctx, cancel := context.WithCancel(h.ctx)
		f, ferr := h.s.buildChatFetcher(p.Config, resolvers, ctx)
		if ferr != nil {
			cancel()
			h.s.addLog("[chat] server " + key + ": " + ferr.Error())
			continue
		}
		cc := client.NewChatClient(f, h.identity)
		cc.SetBudget(h.budget) // fixed preset; auto mode overrides per-send
		h.servers[key] = &perServerChat{
			serverKey: key,
			name:      p.Nickname,
			domain:    p.Config.Domain,
			client:    cc,
			f:         f,
			scorer:    newChatBudgetScorer(),
			cfg:       p.Config,
			cancel:    cancel,
		}
	}
}

// syncServers is syncServersLocked for callers that don't hold mu. Takes
// Server.mu (read) via chatResolvers, so never call it holding Server.mu.
func (h *chatHub) syncServers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.syncServersLocked()
}

// sameChatConfig reports whether a server's chat client can be kept: only
// these fields shape it (connection settings are global, resolvers shared).
func sameChatConfig(a, b Config) bool {
	return chatServerKey(a.Domain) == chatServerKey(b.Domain) && a.Key == b.Key &&
		strings.TrimSpace(a.ServerKey) == strings.TrimSpace(b.ServerKey) &&
		slices.Equal(a.ExtraDomains, b.ExtraDomains)
}

// ensureIdentityLocked creates the identity on first use, then builds the
// per-server clients. Caller holds mu.
func (h *chatHub) ensureIdentityLocked() error {
	if h.identity != nil {
		return nil
	}
	seed, err := protocol.GenerateSeed()
	if err != nil {
		return err
	}
	id, err := client.NewChatIdentity(seed)
	if err != nil {
		return err
	}
	h.identity = id
	h.backedUp = false
	h.saveIdentityLocked()
	h.rebuildServersLocked()
	return nil
}

// errChatPollBusy: this server is already being polled, so this cycle skips
// it. Not a failure -- it must never count toward the backoff.
var errChatPollBusy = errors.New("chat: poll already running for this server")

// serverName reads ps.name under the lock: a profile rename mutates it in
// place while background probes are still reading it.
func (h *chatHub) serverName(ps *perServerChat) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return ps.name
}

// snapshotServers returns the current per-server clients.
func (h *chatHub) snapshotServers() []*perServerChat {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*perServerChat, 0, len(h.servers))
	for _, ps := range h.servers {
		out = append(out, ps)
	}
	return out
}

// resolveServer picks the chat server for a conversation. An explicitly
// requested server wins (the picker enables it on send). Otherwise it routes
// only through ENABLED servers — the thread's bound server, else the active
// profile's, else any enabled one — so disabling a server stops its
// conversations from silently continuing on it. Returns nil when none fit.
func (h *chatHub) resolveServer(addr, requested string) *perServerChat {
	h.mu.Lock()
	defer h.mu.Unlock()
	if key := chatServerKey(requested); key != "" {
		if ps := h.servers[key]; ps != nil {
			return ps
		}
	}
	if th := h.threads[addr]; th != nil && th.Server != "" && h.enabled[th.Server] {
		if ps := h.servers[th.Server]; ps != nil {
			return ps
		}
	}
	if h.enabled[h.activeKey] {
		if ps := h.servers[h.activeKey]; ps != nil {
			return ps
		}
	}
	for k, ps := range h.servers {
		if h.enabled[k] {
			return ps
		}
	}
	return nil
}

// chatServerBackoff: a server with no chat shouldn't be re-queried often; a
// transient failure is retried sooner.
func chatServerBackoff(err error) time.Duration {
	if errors.Is(err, client.ErrChatDisabled) ||
		errors.Is(err, client.ErrChatNoServerKey) ||
		errors.Is(err, client.ErrChatUnverified) ||
		errors.Is(err, client.ErrChatVersion) {
		return 30 * time.Minute
	}
	return 2 * time.Minute
}

// runPollLoop polls every chat server in the background while an identity
// exists (a user who never opened chat is never auto-registered).
func (h *chatHub) runPollLoop(ctx context.Context) {
	timer := time.NewTimer(chatPollFirstWait)
	defer timer.Stop()
	for {
		// poll governs whether this wake actually queries the servers. A timer
		// tick always polls; a kick polls only when it asked to (UI connect).
		poll := true
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case poll = <-h.pollKick:
			// Drain the fired timer so the Reset below is the only pending wake.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if poll {
			if n := h.pollAllServers(ctx, nil, chatPollServerTimeout); n > 0 {
				h.markActivity()
				h.s.broadcastChat(map[string]any{"type": "inbox", "got": n})
				// Native notifier (mobile): the web UI handles foreground alerts,
				// but a backgrounded app has its WebView suspended — let the
				// platform post a system notification. The handler gates foreground.
				h.s.notifyNewMessages(n)
			}
		}
		interval := h.pollInterval()
		h.broadcastNextPoll(interval) // keep the frontend countdown in sync
		timer.Reset(interval)
	}
}

// markActivity stamps a send or received message, tightening the poll loop to
// chatPollFastInterval for chatActivityWindow.
func (h *chatHub) markActivity() {
	h.mu.Lock()
	h.lastActivity = time.Now()
	h.mu.Unlock()
}

// pollInterval picks the loop's next sleep: the slow background cadence with no
// UI watching, else the fast cadence right after activity, else the active
// cadence. The backend is the only timer-based poller, so this is the single
// knob governing chat-server query volume.
func (h *chatHub) pollInterval() time.Duration {
	if !h.s.hasUIClients() {
		return chatPollInterval
	}
	h.mu.Lock()
	active := !h.lastActivity.IsZero() && time.Since(h.lastActivity) < chatActivityWindow
	h.mu.Unlock()
	if active {
		return chatPollFastInterval
	}
	return chatPollActiveInterval
}

// kickPoll wakes runPollLoop. pollNow=true polls immediately (a UI just
// connected); pollNow=false only reschedules + resyncs the countdown (a manual
// refresh already polled). Non-blocking: a pending kick is enough.
func (h *chatHub) kickPoll(pollNow bool) {
	select {
	case h.pollKick <- pollNow:
	default:
	}
}

// broadcastNextPoll tells connected UIs when the next background poll lands, so
// the frontend countdown ring reflects the backend's actual schedule.
func (h *chatHub) broadcastNextPoll(d time.Duration) {
	h.s.broadcastChat(map[string]any{"type": "nextpoll", "ms": d.Milliseconds()})
}

// backedOff reports whether ps is currently in its no-chat/transient backoff
// window. setBackoff updates the window. Both guard the shared backoffUntil
// under h.mu, since polls and availability probes run concurrently.
func (h *chatHub) backedOff(ps *perServerChat, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return now.Before(ps.backoffUntil)
}

func (h *chatHub) setBackoff(ps *perServerChat, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ps.backoffUntil = until
	if until.IsZero() {
		ps.lastReason = "" // reachable again — drop the stale failure reason
		ps.failures = 0
	}
}

// setProbeFailure records a failed availability probe: the backoff window plus
// the i18n reason, so a backed-off server can be listed from cache (with its
// reason) instead of being re-probed every call.
func (h *chatHub) setProbeFailure(ps *perServerChat, until time.Time, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ps.backoffUntil = until
	ps.lastReason = reason
}

// noteProbeFailure is setProbeFailure with the escalating backoff, so a dead
// server stops being re-probed on every availability call.
func (h *chatHub) noteProbeFailure(ps *perServerChat, err error, reason string) {
	h.noteFailure(ps, err)
	h.mu.Lock()
	ps.lastReason = reason
	h.mu.Unlock()
}

// probeBackoff reports whether ps is in a failure backoff window, and its last
// reason. Used by the availability handler to skip re-probing dead servers.
func (h *chatHub) probeBackoff(ps *perServerChat, now time.Time) (bool, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return now.Before(ps.backoffUntil), ps.lastReason
}

// markReachable records a confirmed-reachable probe/register: clears the
// failure backoff and stamps the positive availability cache.
func (h *chatHub) markReachable(ps *perServerChat) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ps.backoffUntil = time.Time{}
	ps.lastReason = ""
	ps.lastOKAt = time.Now()
	ps.failures = 0
}

// noteFailure records a failure and returns the new backoff deadline, doubling
// per consecutive failure up to chatBackoffMax: one flaky round trip still
// retries soon, a server that is gone fades out.
func (h *chatHub) noteFailure(ps *perServerChat, err error) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	ps.failures++
	d := chatServerBackoff(err)
	// Only a verified "no messenger here" may escalate to the long ceiling.
	ceiling := chatTransientBackoffMax
	if d >= chatBackoffMax {
		ceiling = chatBackoffMax
	}
	for i := 1; i < ps.failures && d < ceiling; i++ {
		d *= 2
	}
	if d > ceiling {
		d = ceiling
	}
	ps.backoffUntil = time.Now().Add(d)
	return ps.backoffUntil
}

// cachedAvailable reports whether ps was confirmed reachable within the cache
// TTL, so it can be reported available without a fresh (slow) probe.
func (h *chatHub) cachedAvailable(ps *perServerChat, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !ps.lastOKAt.IsZero() && now.Sub(ps.lastOKAt) < chatAvailCacheTTL
}

// chatAdvEntry caches a config's chat-advertised bit read from VERIFIED feed
// metadata, with the time it was read. See Server.chatAdv.
type chatAdvEntry struct {
	advertised bool
	at         time.Time
}

// chatAdvTTL: how long a verified metadata chat-bit is trusted before a re-read
// is wanted. Matches the verified-disabled re-check cadence.
const chatAdvTTL = 30 * time.Minute

// recordChatAdv stores a config's chat-advertised bit, but ONLY when it came
// from signature-verified metadata — an unverified bit is never authoritative
// and must never drive a "no chat" verdict. Called from the feed AND the chat
// fetchers' metadata callbacks, so the feed populates it for free.
func (s *Server) recordChatAdv(serverKey string, advertised, verified bool) {
	if serverKey == "" || !verified {
		return
	}
	s.chatAdvMu.Lock()
	s.chatAdv[serverKey] = chatAdvEntry{advertised: advertised, at: time.Now()}
	s.chatAdvMu.Unlock()
}

// chatAdvVerified returns the last verified chat-advertised bit for serverKey and
// whether it is still fresh (within chatAdvTTL).
func (s *Server) chatAdvVerified(serverKey string) (advertised, fresh bool) {
	s.chatAdvMu.Lock()
	defer s.chatAdvMu.Unlock()
	e, ok := s.chatAdv[serverKey]
	if !ok || time.Since(e.at) >= chatAdvTTL {
		return false, false
	}
	return e.advertised, true
}

// everConfirmed reports whether ps was EVER verified to have chat this process
// (lastOKAt is set by markReachable and never cleared). Once true, the cheap
// UNVERIFIED block-0 pre-check must not be trusted to disable it — only the
// authoritative, signature-verified EnsureInfo may. See probeOneServer.
func (h *chatHub) everConfirmed(ps *perServerChat) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !ps.lastOKAt.IsZero()
}

// shouldSkipProbe reports whether the availability handler should skip a live
// probe of ps and report it from cache (with its last reason). Only DISABLED
// servers in a failure backoff are skipped — so a couple of dead/old extra
// configs can't keep flooding the shared resolver pool and starving sends.
// Enabled servers are always probed, for prompt recovery once they come back.
func (h *chatHub) shouldSkipProbe(ps *perServerChat, enabled bool, now time.Time) (bool, string) {
	if enabled {
		return false, ""
	}
	return h.probeBackoff(ps, now)
}

// clearBackoffs drops every server's backoff window so the next availability
// probe is a full, fresh sweep — wired to the UI's manual "retry" button.
func (h *chatHub) clearBackoffs() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ps := range h.servers {
		ps.backoffUntil = time.Time{}
		ps.lastReason = ""
		ps.failures = 0
	}
}

// serverEnabled reports whether the user has opted in to publish/poll on this
// server key. h.enabled is the single guarded source of truth (read from the
// unlocked poll path and the availability goroutines).
func (h *chatHub) serverEnabled(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.enabled[key]
}

// syncResolvers pushes the feed's current healthy resolver pool into every chat
// fetcher so chat rides the same live, scored resolver list as the feed instead
// of a stale snapshot taken when the fetcher was built. It only re-pushes (and
// logs) when the list actually changes. Called before each poll cycle and
// availability probe.
func (h *chatHub) syncResolvers() {
	rs := h.s.chatResolvers()
	if len(rs) == 0 {
		return
	}
	h.mu.Lock()
	changed := !equalStrings(h.lastResolvers, rs)
	if changed {
		h.lastResolvers = append([]string(nil), rs...)
	}
	servers := make([]*perServerChat, 0, len(h.servers))
	for _, ps := range h.servers {
		servers = append(servers, ps)
	}
	h.mu.Unlock()
	if !changed {
		return
	}
	for _, ps := range servers {
		if ps.f != nil {
			ps.f.SetActiveResolvers(rs)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pollAllServers polls each non-backed-off server; a server that reports no
// chat is backed off so we stop hammering it. Returns total new messages.
// budget caps each server's poll; 0 inherits ctx.
func (h *chatHub) pollAllServers(ctx context.Context, progress client.ChatProgress, budget time.Duration) int {
	h.mu.Lock()
	idOK := h.identity != nil
	var localAddr string
	if idOK {
		localAddr = client.ChatAddressString(h.identity.Addr)
	}
	h.mu.Unlock()
	if !idOK {
		return 0
	}
	h.syncResolvers() // ride the feed's current healthy resolver pool

	agg := newChatPollProgress(progress)
	return h.pollFanOut(ctx, budget, h.pollOrder(time.Now()),
		func(pctx context.Context, ps *perServerChat) (int, error) {
			return h.pollServer(pctx, ps, agg.forServer(ps.serverKey), localAddr)
		})
}

// pollFanOut polls up to chatPollConcurrency servers at once, each under its
// own chatPollServerTimeout, and sums the new messages. Inboxes are
// independent, so a slow server only costs its own slot.
// budget caps each server's poll; 0 means "inherit ctx" (the manual refresh
// already gives itself a long deadline and must be allowed to use it).
func (h *chatHub) pollFanOut(ctx context.Context, budget time.Duration, order []*perServerChat,
	poll func(context.Context, *perServerChat) (int, error)) int {

	sema := make(chan struct{}, chatPollConcurrency)
	var wg sync.WaitGroup
	var total atomic.Int64

	for _, ps := range order {
		select {
		case sema <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return int(total.Load())
		}
		wg.Add(1)
		go func(ps *perServerChat) {
			defer wg.Done()
			defer func() { <-sema }()

			pctx, cancel := ctx, context.CancelFunc(func() {})
			if budget > 0 {
				pctx, cancel = context.WithTimeout(ctx, budget)
			}
			n, err := poll(pctx, ps)
			cancel()
			if err != nil {
				// Our own exit (shutdown, user navigated away) says nothing
				// about the server -- penalising it here is how a short network
				// outage bought 30 minutes of silence.
				if ctx.Err() == nil && !errors.Is(err, errChatPollBusy) {
					h.noteFailure(ps, err)
				}
				return
			}
			h.setBackoff(ps, time.Time{}) // clear backoff on success
			total.Add(int64(n))
		}(ps)
	}
	wg.Wait()
	return int(total.Load())
}

// chatPollProgress sums the per-server poll progress into one figure, so
// parallel polls do not make the UI bar jump between servers.
type chatPollProgress struct {
	out client.ChatProgress
	mu  sync.Mutex
	st  map[string][2]int // serverKey -> {done, total}
}

func newChatPollProgress(out client.ChatProgress) *chatPollProgress {
	return &chatPollProgress{out: out, st: map[string][2]int{}}
}

// forServer returns a progress callback tagged with one server's key.
func (p *chatPollProgress) forServer(key string) client.ChatProgress {
	if p.out == nil {
		return nil
	}
	return func(done, total int) {
		p.mu.Lock()
		p.st[key] = [2]int{done, total}
		var d, t int
		for _, v := range p.st {
			d, t = d+v[0], t+v[1]
		}
		p.mu.Unlock()
		p.out(d, t)
	}
}

// pollOrder returns the servers worth polling now, healthiest first, so a
// flaky config never takes a poll slot ahead of the one the user chats on.
func (h *chatHub) pollOrder(now time.Time) []*perServerChat {
	var out []*perServerChat
	for _, ps := range h.snapshotServers() {
		// Only poll (which opens a read session and auto-registers) servers the
		// user has explicitly enabled — never publish the identity otherwise.
		if !h.serverEnabled(ps.serverKey) || h.backedOff(ps, now) {
			continue
		}
		out = append(out, ps)
	}
	h.mu.Lock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].failures < out[j].failures })
	h.mu.Unlock()
	return out
}

// pollServer fetches one server's inbox, stores new messages (tagged with the
// server), and acks them.
func (h *chatHub) pollServer(ctx context.Context, ps *perServerChat, progress client.ChatProgress, localAddr string) (int, error) {
	// Skip, never queue: with a multi-minute budget, waiting here would stack a
	// goroutine per cycle behind one stuck server.
	if !ps.pollMu.TryLock() {
		return 0, errChatPollBusy
	}
	defer ps.pollMu.Unlock()

	msgs, err := ps.client.FetchInbox(ctx, progress)
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	h.mu.Lock()
	added := 0
	maxSeq := map[string]uint32{}

	// Detect server-side seq regression: if every incoming seq from a peer
	// is at/below our acked watermark, the server likely reset its store.
	// Collect the highest incoming seq per peer first, then reset the
	// watermark for any peer whose max incoming seq regressed.
	peerMax := map[string]uint32{}
	for _, m := range msgs {
		addr := client.ChatAddressString(m.From)
		if m.Seq > peerMax[addr] {
			peerMax[addr] = m.Seq
		}
	}
	for addr, mx := range peerMax {
		th := h.threads[addr]
		if th != nil && th.AckedIn[ps.serverKey] > 0 && mx <= th.AckedIn[ps.serverKey] {
			th.resetAckedIn(ps.serverKey)
		}
	}

	for _, m := range msgs {
		addr := client.ChatAddressString(m.From)
		if m.Seq > maxSeq[addr] {
			maxSeq[addr] = m.Seq
		}
		th := h.threads[addr]
		if th == nil {
			th = &chatThreadFile{}
			h.threads[addr] = th
		}
		if th.Server == "" {
			th.Server = ps.serverKey
		}
		// Replay guard: a seq at/below our acked watermark was already delivered,
		// so a (re-)served copy is a duplicate — drop it without re-rendering,
		// even if local history was cleared.
		if th.replayedIn(ps.serverKey, m.Seq) {
			continue
		}
		dup := false
		for _, ex := range th.Msgs {
			if ex.Dir == "in" && ex.Seq == m.Seq && ex.Server == ps.serverKey && ex.LocalAddr == localAddr {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		th.Msgs = append(th.Msgs, chatStoredMsg{
			Dir: "in", Seq: m.Seq, Text: m.Text,
			TS: time.Now().Unix(), Server: ps.serverKey, LocalAddr: localAddr,
		})
		th.Unread++
		added++
	}
	if added > 0 {
		h.saveThreadsLocked()
	}
	h.mu.Unlock()

	if added > 0 {
		// Render the new messages immediately. The per-peer ACK below is a DNS
		// round trip (seconds on a flaky resolver) that only frees the sender's
		// quota — it has nothing to do with display and must not delay the UI.
		// Notification fires separately (the 'inbox' event), so this render-only
		// signal doesn't double-notify.
		h.s.broadcastChat(map[string]any{"type": "inboxstored", "got": added})
	}

	for addr, seq := range maxSeq {
		peer, perr := client.ParseChatAddress(addr)
		if perr != nil {
			continue
		}
		if err := ps.client.Ack(ctx, peer, seq); err != nil {
			h.s.addLog("[chat] ack " + addr[:6] + ": " + err.Error())
			continue
		}
		// Ack landed → raise the replay watermark for this peer+server, so a
		// later re-serve of seq ≤ this is dropped (see the replay guard above).
		h.mu.Lock()
		if th := h.threads[addr]; th != nil && th.markAckedIn(ps.serverKey, seq) {
			h.saveThreadsLocked()
		}
		h.mu.Unlock()
	}
	return added, nil
}

// broadcastChat pushes a chat SSE event to all connected UIs.
func (s *Server) broadcastChat(payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.broadcast("event: chat\ndata: " + string(raw) + "\n\n")
}

// chatStatusKey maps driver errors to i18n keys for the UI.
func chatStatusKey(err error) string {
	var serr *client.ChatStatusError
	switch {
	case errors.Is(err, client.ErrChatNoServerKey):
		return "chat_err_no_key"
	case errors.Is(err, client.ErrChatServerDisabled):
		return "chat_err_server_disabled"
	// No resolver to send over and running out of time both mean the server
	// could not be reached -- the one thing the user can act on.
	case errors.Is(err, client.ErrChatUnreachable),
		errors.Is(err, client.ErrChatNoResolvers),
		errors.Is(err, context.DeadlineExceeded):
		return "chat_err_unreachable"
	case errors.Is(err, client.ErrChatDisabled):
		return "chat_err_disabled"
	case errors.Is(err, client.ErrChatUnverified):
		return "chat_err_unverified"
	case errors.Is(err, client.ErrChatVersion):
		return "chat_err_version"
	case errors.As(err, &serr):
		switch serr.Status {
		case protocol.ChatStatusRateLimited:
			return "chat_err_rate_limited"
		case protocol.ChatStatusInboxFull:
			return "chat_err_inbox_full"
		case protocol.ChatStatusPairQuota:
			return "chat_err_pair_quota"
		case protocol.ChatStatusUnknownRecipient, protocol.ChatStatusNotFound:
			return "chat_err_unknown_recipient"
		case protocol.ChatStatusBusy:
			return "chat_err_busy"
		}
	}
	return "chat_err_generic"
}

// chatSafetyEmojiTable is the 64-emoji alphabet for the conversation safety
// code (both clients derive the same 5 emojis from the pair's identity keys).
var chatSafetyEmojiTable = []string{
	"🐶", "🐱", "🦊", "🐻", "🐼", "🐨", "🐯", "🦁",
	"🐮", "🐷", "🐸", "🐵", "🐔", "🐧", "🐦", "🦆",
	"🦉", "🐴", "🦄", "🐝", "🐢", "🐙", "🦋", "🐬",
	"🐳", "🐊", "🦒", "🐘", "🦔", "🐿", "🦜", "🦢",
	"🍎", "🍊", "🍋", "🍉", "🍇", "🍓", "🍒", "🍑",
	"🥝", "🍍", "🥥", "🌽", "🥕", "🍄", "🌻", "🌹",
	"🌵", "🍀", "🌲", "⭐", "🌙", "☀", "⛅", "🌈",
	"⚡", "❄", "🔥", "🎲", "🎈", "🎵", "⚽", "🔑",
}

// chatSafetyEmojis derives the shared safety code for two identity keys.
func chatSafetyEmojis(pubA, pubB []byte) []string {
	a, b := pubA, pubB
	if string(a) > string(b) {
		a, b = b, a
	}
	sum := sha256.Sum256(append(append([]byte("thefeed-chat-safety-v1"), a...), b...))
	out := make([]string, 5)
	for i := 0; i < 5; i++ {
		out[i] = chatSafetyEmojiTable[int(sum[i])%len(chatSafetyEmojiTable)]
	}
	return out
}

// ---- HTTP handlers ----

// handleChatInfo reports the local identity WITHOUT creating one — creating
// keys (and later publishing them) is an explicit opt-in via handleChatEnable.
// When no identity exists yet it returns exists:false so the UI shows the
// "enable messenger" prompt instead of silently registering an account.
func (s *Server) handleChatInfo(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.identity == nil {
		writeJSON(w, map[string]any{"exists": false})
		return
	}
	enabledCount := 0
	for key := range h.servers {
		if h.enabled[key] {
			enabledCount++
		}
	}
	// Disk-backed server list (no DNS probe): lets the UI render the server
	// picker + the add-contact field instantly on open — with each row's live
	// reachability filled in later by /api/chat/availability — instead of a
	// blanket "checking…" until the slow probe returns.
	srvList := make([]map[string]any, 0, len(h.servers))
	for key, ps := range h.servers {
		srvList = append(srvList, map[string]any{
			"key":     key,
			"name":    ps.name,
			"domain":  ps.domain,
			"enabled": h.enabled[key],
		})
	}
	sort.Slice(srvList, func(i, j int) bool {
		return srvList[i]["key"].(string) < srvList[j]["key"].(string)
	})
	writeJSON(w, map[string]any{
		"exists":      true,
		"address":     client.ChatAddressString(h.identity.Addr),
		"backedUp":    h.backedUp,
		"serverCount": len(h.servers),
		"servers":     srvList,
		// anyEnabled / enabledCount are the server-side source of truth for "has
		// the user set up a chat server" — the client uses them to drive first-run
		// guidance and to render the header instantly on open (no "checking…"
		// flash) before the slow DNS availability probe returns. They live on disk
		// (h.enabled), surviving the Android loopback-port change that wipes
		// localStorage.
		"anyEnabled":   enabledCount > 0,
		"enabledCount": enabledCount,
	})
}

// chatServerResult is one server's availability row, shared by the aggregate,
// list, and per-server probe handlers.
type chatServerResult struct {
	Key       string `json:"key"`
	Name      string `json:"name,omitempty"`
	Domain    string `json:"domain"`
	Available bool   `json:"available"`
	Enabled   bool   `json:"enabled"`
	Cached    bool   `json:"cached,omitempty"` // reported from the positive cache, not a fresh probe
	Reason    string `json:"reason,omitempty"`
	// Checking means the sweep ran out of time before this server answered.
	// No verdict, no penalty: the UI finishes it via /api/chat/probe.
	Checking bool `json:"checking,omitempty"`
}

// chatLimitsMap renders a server's advertised limits for the UI.
func chatLimitsMap(l protocol.ChatLimits) map[string]any {
	return map[string]any{
		"maxMsgBytes": l.MaxMsgBytes,
		"sendPerHour": l.SendPerHour,
		"inboxCap":    l.InboxCap,
		"ttlHours":    l.TTLHours,
	}
}

// probeOneServer does a fresh DNS capability probe of one server, updating its
// failure backoff or positive cache. Returns the row plus the server's
// advertised limits when reachable (nil otherwise).
// budget is this probe's own time; spending it all is a real "unreachable"
// verdict. ctx expiring instead means the CALLER walked away, which tells us
// nothing about the server.
func (h *chatHub) probeOneServer(ctx context.Context, ps *perServerChat, enabled bool, budget time.Duration) (chatServerResult, map[string]any) {
	res := chatServerResult{Key: ps.serverKey, Name: h.serverName(ps), Domain: ps.domain, Enabled: enabled}
	cctx, c := context.WithTimeout(ctx, budget)
	defer c()
	// Chat-availability gate, drawn from VERIFIED metadata only. The feed's
	// regular metadata fetches cache each config's signed ChatAvailable bit
	// (s.chatAdv); a "no chat" verdict is therefore always signature-backed —
	// never the old unverified raw block-0 read that let fake/transient data
	// silence a working server.
	// cutShort: the CALLER's context ended, so we never learned anything about
	// this server. Report "still checking" and record no failure -- only a
	// verdict we actually got may disable a server. Our own budget running out
	// is NOT this case: that is a genuine unreachable.
	cutShort := func() bool { return ctx.Err() != nil }
	advertised, fresh := h.s.chatAdvVerified(ps.serverKey)
	if !fresh && !h.everConfirmed(ps) {
		// Never confirmed and no fresh verified bit: fetch+verify the FULL metadata
		// now so we can authoritatively fast-fail a chatless server (skipping
		// EnsureInfo's slow ChatInfo-absence retries). The fetch caches the bit via
		// the metadata callback. A confirmed server skips this — EnsureInfo's cache
		// already confirms it, and a verified "off" still arrives via the feed.
		if _, err := ps.f.FetchMetadata(cctx); err == nil {
			advertised, fresh = h.s.chatAdvVerified(ps.serverKey)
		} else if cutShort() {
			res.Checking = true
			return res, nil
		}
	}
	if fresh && !advertised {
		// VERIFIED: this server advertises no messenger → disabled; re-check after
		// the 30-min backoff (or sooner when the feed re-reads its metadata).
		res.Reason = "chat_err_disabled"
		h.noteProbeFailure(ps, client.ErrChatDisabled, res.Reason)
		return res, nil
	}
	// Advertised, or we couldn't get a verified bit (old/unsigned server) → the
	// authoritative, signature-verified ChatInfo probe decides (keys + limits).
	info, err := ps.client.EnsureInfo(cctx)
	if err != nil {
		if cutShort() {
			res.Checking = true
			return res, nil
		}
		// Belt and braces for the same rule the client now follows: a server
		// whose signed metadata says it HAS a messenger may never be reported
		// as having none, whatever a failed ChatInfo fetch suggests. The
		// verified negative is handled above and returns before this.
		if advertised && fresh && errors.Is(err, client.ErrChatDisabled) {
			err = client.ErrChatUnreachable
		}
		res.Reason = chatStatusKey(err)
		h.noteProbeFailure(ps, err, res.Reason)
		return res, nil
	}
	res.Available = true
	h.markReachable(ps)
	lim := chatLimitsMap(info.Limits)
	// Cache the advertised limits so the positive-cache path (which skips the
	// probe) can still report them — otherwise the send-quota ring vanishes
	// whenever availability is served from cache.
	h.mu.Lock()
	ps.lastLimits = lim
	h.mu.Unlock()
	return res, lim
}

// handleChatAvailability probes EVERY chat server (each profile that pins a
// server key) in parallel and reports per-server availability plus the
// aggregate. The frontend uses the list to pick a server when starting a new
// chat and to gate composing. Servers confirmed reachable within the cache TTL
// are reported available without re-probing (unless ?retry=1 forces a sweep).
func (s *Server) handleChatAvailability(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	hasID := h.identity != nil
	h.mu.Unlock()

	resp := map[string]any{"available": false}
	if !hasID {
		// No identity yet → the UI shows the "enable messenger" prompt; don't
		// create keys or probe here.
		resp["needsCreate"] = true
		writeJSON(w, resp)
		return
	}
	if r.URL.Query().Get("retry") == "1" {
		h.clearBackoffs() // manual retry → fresh full sweep, ignore prior backoffs
	}
	h.syncResolvers() // probe over the feed's current healthy resolver pool
	servers := h.snapshotServers()
	if len(servers) == 0 {
		// No profile pins a server key → chat is fail-closed everywhere.
		resp["reason"] = "chat_err_no_key"
		writeJSON(w, resp)
		return
	}

	// Probes run in the BACKGROUND, detached from this request: waiting for the
	// slowest holds the response past the frontend's abort, and the UI then
	// reports no servers while a working one exists. Each probe broadcasts its
	// own result, so one that answers in 1s shows at once and one that needs
	// 40s is still not abandoned.
	bg := h.bgContext()
	forced := r.URL.Query().Get("retry") == "1"
	out := make([]chatServerResult, len(servers))
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		limits map[string]any
		landed = map[string]chatServerResult{} // probes that answered in time
	)
	now := time.Now()
	for i, ps := range servers {
		enabled := h.serverEnabled(ps.serverKey)
		// Positive cache: skip the probe for a recently-reachable server (unless forced).
		if !forced && h.cachedAvailable(ps, now) {
			out[i] = chatServerResult{Key: ps.serverKey, Name: h.serverName(ps), Domain: ps.domain, Enabled: enabled, Available: true, Cached: true}
			h.mu.Lock()
			cachedLim := ps.lastLimits
			h.mu.Unlock()
			// `limits` is shared with the probe goroutines below — guard it with
			// the SAME local mutex they use (not h.mu) to avoid a data race.
			mu.Lock()
			if limits == nil && cachedLim != nil {
				limits = cachedLim
			}
			mu.Unlock()
			continue
		}
		// A DISABLED server that recently failed (old/incompatible/dead) is
		// reported from cache instead of re-probed. Re-probing such servers on
		// every availability call — and this handler runs on every foreground
		// poll while chat looks unavailable — floods the shared resolver pool
		// with retried DNS fetches and starves the real send, which is exactly
		// how a couple of stale extra configs broke chat. Enabled servers are
		// always probed: the user opted in and wants prompt recovery.
		if skip, reason := h.shouldSkipProbe(ps, enabled, now); skip {
			out[i] = chatServerResult{Key: ps.serverKey, Name: h.serverName(ps), Domain: ps.domain, Enabled: enabled, Reason: reason}
			continue
		}
		// Already being probed from an earlier list open: leave the row as
		// "checking" and let that probe's broadcast finish it.
		if !h.beginProbe(ps.serverKey) {
			out[i] = chatServerResult{Key: ps.serverKey, Name: h.serverName(ps), Domain: ps.domain, Enabled: enabled, Checking: true}
			continue
		}
		out[i] = chatServerResult{Key: ps.serverKey, Name: h.serverName(ps), Domain: ps.domain, Enabled: enabled, Checking: true}
		wg.Add(1)
		go func(ps *perServerChat, enabled bool) {
			defer wg.Done()
			res, lim := h.probeOneServer(bg, ps, enabled, chatAvailProbeTimeout)

			mu.Lock()
			landed[res.Key] = res
			if lim != nil && limits == nil {
				limits = lim
			}
			mu.Unlock()

			// Drop the claim BEFORE broadcasting: a request that still sees it
			// held is then guaranteed the broadcast below, so no row is left
			// spinning on "checking" with no event coming.
			h.endProbe(ps.serverKey)
			// Tell the UI the moment THIS server is decided, whether that took
			// one second or forty.
			h.broadcastServerResult(res, lim)
		}(ps, enabled)
	}
	// Wait only for a nicer first paint; stragglers arrive over SSE.
	waitUpTo(&wg, chatAvailFirstPaint)

	mu.Lock()
	for i := range out {
		if res, ok := landed[out[i].Key]; ok {
			out[i] = res
		}
	}
	// Copy, never assign back: the probe goroutines outlive this handler and
	// keep writing `limits` under mu.
	shownLimits := limits
	mu.Unlock()

	anyUsable, firstReason := false, ""
	for _, rr := range out {
		if rr.Available && rr.Enabled {
			anyUsable = true // can actually chat now: enabled AND reachable
		}
		if firstReason == "" && rr.Reason != "" && !rr.Checking {
			firstReason = rr.Reason
		}
	}

	resp["servers"] = out
	resp["available"] = anyUsable
	if shownLimits != nil {
		resp["limits"] = shownLimits
	}
	if !anyUsable && firstReason != "" {
		resp["reason"] = firstReason
	}
	writeJSON(w, resp)
}

// bgContext returns the hub's lifetime context, so a probe outlives the HTTP
// request that started it.
func (h *chatHub) bgContext() context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx != nil {
		return h.ctx
	}
	return context.Background()
}

// beginProbe claims a server for an availability probe; false means one is
// already in flight.
func (h *chatHub) beginProbe(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.probing[key] {
		return false
	}
	if h.probing == nil {
		h.probing = make(map[string]bool)
	}
	h.probing[key] = true
	return true
}

func (h *chatHub) endProbe(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.probing, key)
}

// broadcastServerResult pushes one server's availability verdict to the UI so
// each row updates as soon as that server answers.
func (h *chatHub) broadcastServerResult(res chatServerResult, limits map[string]any) {
	msg := map[string]any{"type": "availserver", "server": res}
	if limits != nil {
		msg["limits"] = limits
	}
	h.s.broadcastChat(msg)
}

// waitUpTo waits for wg, giving up after d (the work continues).
func waitUpTo(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// handleChatProbeServer probes ONE server (?server=KEY). The UI fires these in
// parallel — one per row — can cancel any individually, and updates each row as
// it resolves, so a slow/dead server never blocks the others. Served from the
// positive cache unless ?retry=1 forces a fresh probe.
func (s *Server) handleChatProbeServer(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	hasID := h.identity != nil
	h.mu.Unlock()
	if !hasID {
		writeJSON(w, map[string]any{"needsCreate": true})
		return
	}
	key := chatServerKey(r.URL.Query().Get("server"))
	if key == "" {
		http.Error(w, "invalid server", 400)
		return
	}
	h.mu.Lock()
	ps := h.servers[key]
	h.mu.Unlock()
	if ps == nil {
		http.Error(w, "unknown server", 404)
		return
	}
	h.syncResolvers()
	enabled := h.serverEnabled(key)
	now := time.Now()
	if r.URL.Query().Get("retry") != "1" && h.cachedAvailable(ps, now) {
		resp := map[string]any{"server": chatServerResult{
			Key: key, Name: h.serverName(ps), Domain: ps.domain, Enabled: enabled, Available: true, Cached: true,
		}}
		h.mu.Lock()
		if ps.lastLimits != nil {
			resp["limits"] = ps.lastLimits
		}
		h.mu.Unlock()
		writeJSON(w, resp)
		return
	}
	res, lim := h.probeOneServer(r.Context(), ps, enabled, chatAvailProbeTimeout)
	resp := map[string]any{"server": res}
	if lim != nil {
		resp["limits"] = lim
	}
	writeJSON(w, resp)
}

// handleChatEnable is the explicit chat opt-in. action "create" generates the
// local identity (no key leaves the machine until a server is turned on);
// action "server" turns the messenger on/off for one server. Registration —
// publishing the chat pubkey — happens ONLY for servers turned on here.
func (s *Server) handleChatEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Action string `json:"action"` // "create" | "server"
		Server string `json:"server"`
		On     bool   `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	h := s.chat
	switch req.Action {
	case "create":
		h.mu.Lock()
		err := h.ensureIdentityLocked()
		addr := ""
		if h.identity != nil {
			addr = client.ChatAddressString(h.identity.Addr)
		}
		h.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "address": addr})

	case "server":
		key := chatServerKey(req.Server)
		if key == "" {
			http.Error(w, "invalid server", 400)
			return
		}
		h.mu.Lock()
		if h.identity == nil {
			h.mu.Unlock()
			http.Error(w, "no identity", 400)
			return
		}
		if req.On {
			h.enabled[key] = true
		} else {
			delete(h.enabled, key)
		}
		h.saveEnabledLocked()
		ps := h.servers[key]
		ctx := h.ctx
		h.mu.Unlock()

		// Register on the newly-enabled server (and pull mail). The first attempt is
		// synchronous so the toggle returns an authoritative verdict; on failure a
		// background loop keeps retrying.
		out := map[string]any{"ok": true}
		if req.On && ps != nil && ctx != nil {
			h.mu.Lock()
			la := ""
			if h.identity != nil {
				la = client.ChatAddressString(h.identity.Addr)
			}
			h.mu.Unlock()

			// Retry within the bounded window: one DNS round-trip is too flaky to
			// trust as the verdict.
			sctx, scancel := context.WithTimeout(r.Context(), chatEnableProbeTimeout)
			var n int
			var err error
			for attempt := 0; attempt < chatEnableSyncAttempts; attempt++ {
				if attempt > 0 {
					select {
					case <-sctx.Done():
					case <-time.After(1200 * time.Millisecond):
					}
					if sctx.Err() != nil {
						break
					}
				}
				if n, err = h.pollServer(sctx, ps, nil, la); err == nil {
					break
				}
				h.s.addLog("[chat] enable " + key + ": " + err.Error())
			}
			scancel()
			if err == nil {
				h.markReachable(ps)
				out["available"] = true
				if n > 0 {
					h.s.broadcastChat(map[string]any{"type": "inbox", "got": n})
				}
			} else {
				out["available"] = false
				// Out of time is not refused: the first registration is many
				// round trips from a cold cache and the retry below usually
				// lands seconds later. Report "checking", not an error.
				if sctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
					out["checking"] = true
				} else {
					out["reason"] = chatStatusKey(err)
				}
				// Keep retrying in the background so a flaky network still registers.
				go func() {
					lastErr := err
					for attempt := 1; attempt < chatEnableRegisterAttempts; attempt++ {
						backoff := time.Duration(attempt) * 1500 * time.Millisecond
						if backoff > 5*time.Second {
							backoff = 5 * time.Second
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(backoff):
						}
						// Stop if the user turned this server off meanwhile —
						// pollServer auto-registers and must never publish the
						// identity to a server the user opted out of.
						if !h.serverEnabled(key) {
							return
						}
						n, perr := h.pollServer(ctx, ps, nil, la)
						if perr == nil {
							h.markReachable(ps)
							if n > 0 {
								h.s.broadcastChat(map[string]any{"type": "inbox", "got": n})
							}
							return
						}
						lastErr = perr
						h.s.addLog("[chat] enable " + key + ": " + perr.Error())
					}
					// Eager attempts exhausted — hand off to the background poll loop.
					h.setBackoff(ps, time.Now().Add(chatServerBackoff(lastErr)))
				}()
			}
		}
		if req.On {
			// A server just came on: reschedule the loop so it announces the
			// next-poll time over SSE and the frontend countdown populates at once.
			h.kickPoll(false)
		}
		writeJSON(w, out)

	default:
		http.Error(w, "unknown action", 400)
	}
}

// chatBudgetPresets map a preset name to a per-cell budget B (RFC §8.2). Compact
// blends chat queries into the feed's length cloud (more, smaller queries);
// Wide is fewer, bigger queries (faster on clean resolvers).
var chatBudgetPresets = map[string]int{
	"compact":  8,
	"standard": protocol.ChatCellPlainSize, // 15
	"wide":     protocol.ChatCellPlainMax,  // 21
}

func chatBudgetPresetName(b int) string {
	for name, v := range chatBudgetPresets {
		if v == b {
			return name
		}
	}
	return "custom"
}

// handleChatSettings gets/sets the global cell-size mode: a fixed preset
// (compact/standard/wide) applied to every server's client, or "auto" — where
// each server's scorer picks a preset per send by recent success. GET in auto
// mode also returns the live per-server scores for display.
func (s *Server) handleChatSettings(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	switch r.Method {
	case http.MethodGet:
		h.mu.Lock()
		resp := map[string]any{
			"mode":    h.budgetMode,
			"budget":  h.budget,
			"preset":  chatBudgetPresetName(h.budget),
			"presets": chatBudgetPresets,
		}
		if h.budgetMode == chatBudgetModeAuto {
			scores := map[string][]chatBudgetArm{}
			for k, ps := range h.servers {
				if ps.scorer != nil {
					scores[k] = ps.scorer.snapshot()
				}
			}
			resp["scores"] = scores
		}
		h.mu.Unlock()
		writeJSON(w, resp)
	case http.MethodPost:
		var req struct {
			Mode   string `json:"mode"`
			Preset string `json:"preset"`
			Budget int    `json:"budget"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		mode := req.Mode
		if mode == "" {
			mode = req.Preset // a bare preset implies that fixed mode
		}
		h.mu.Lock()
		switch mode {
		case chatBudgetModeAuto:
			h.budgetMode = chatBudgetModeAuto
		case "compact", "standard", "wide":
			h.budgetMode = mode
			h.budget = chatBudgetPresets[mode]
			for _, ps := range h.servers {
				ps.client.SetBudget(h.budget)
			}
		default:
			if req.Budget == 0 { // no recognizable mode/preset/budget
				h.mu.Unlock()
				http.Error(w, "unknown mode", 400)
				return
			}
			h.budgetMode = "standard"
			h.budget = clampChatBudget(req.Budget)
			for _, ps := range h.servers {
				ps.client.SetBudget(h.budget)
			}
		}
		h.saveBudgetLocked()
		out := map[string]any{"ok": true, "mode": h.budgetMode, "budget": h.budget, "preset": chatBudgetPresetName(h.budget)}
		h.mu.Unlock()
		writeJSON(w, out)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleChatSeed serves and manages the recovery code.
func (s *Server) handleChatSeed(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if h.identity == nil {
			http.Error(w, "no identity", 404)
			return
		}
		code := strings.ToLower(chatSeedEnc.EncodeToString(h.identity.Seed))
		var grouped []string
		for len(code) > 4 {
			grouped = append(grouped, code[:4])
			code = code[4:]
		}
		grouped = append(grouped, code)
		writeJSON(w, map[string]any{"recovery": strings.Join(grouped, "-"), "backedUp": h.backedUp})
	case http.MethodPost:
		var req struct {
			Action   string `json:"action"`
			Recovery string `json:"recovery"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		switch req.Action {
		case "backedup":
			h.backedUp = true
			h.saveIdentityLocked()
			writeJSON(w, map[string]any{"ok": true})
		case "import":
			clean := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(req.Recovery))
			seed, err := chatSeedEnc.DecodeString(clean)
			if err != nil || len(seed) != protocol.SeedSize {
				http.Error(w, "invalid recovery code", 400)
				return
			}
			id, err := client.NewChatIdentity(seed)
			if err != nil {
				http.Error(w, "invalid recovery code", 400)
				return
			}
			changed := h.identity == nil || h.identity.Addr != id.Addr
			h.identity = id
			h.backedUp = true
			h.saveIdentityLocked()
			if changed {
				for _, th := range h.threads {
					th.AckedIn = nil
					th.Emojis = nil
				}
				h.saveThreadsLocked()
			}
			h.rebuildServersLocked() // rebind all per-server clients to the new identity
			writeJSON(w, map[string]any{"ok": true, "address": client.ChatAddressString(id.Addr)})
		default:
			http.Error(w, "unknown action", 400)
		}
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleChatContacts lists and edits the local contact book.
func (s *Server) handleChatContacts(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	switch r.Method {
	case http.MethodGet:
		h.mu.Lock()
		type entry struct {
			Addr string `json:"addr"`
			Name string `json:"name"`
		}
		out := make([]entry, 0, len(h.contacts))
		for a, n := range h.contacts {
			out = append(out, entry{a, n})
		}
		h.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, out)
	case http.MethodPost:
		var req struct {
			Addr string `json:"addr"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		addr, err := client.CanonicalChatAddress(req.Addr)
		if err != nil {
			http.Error(w, "invalid address", 400)
			return
		}
		h.mu.Lock()
		if strings.TrimSpace(req.Name) == "" {
			delete(h.contacts, addr)
		} else {
			h.contacts[addr] = strings.TrimSpace(req.Name)
		}
		h.saveContactsLocked()
		h.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleChatThreads lists conversations, newest first.
func (s *Server) handleChatThreads(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	h.mu.Lock()
	type entry struct {
		Addr     string `json:"addr"`
		Name     string `json:"name,omitempty"`
		LastText string `json:"lastText,omitempty"`
		LastTS   int64  `json:"lastTs,omitempty"`
		LastDir  string `json:"lastDir,omitempty"`
		Unread   int    `json:"unread,omitempty"`
		Pinned   bool   `json:"pinned,omitempty"`
	}
	out := make([]entry, 0, len(h.threads))
	for addr, th := range h.threads {
		e := entry{Addr: addr, Name: h.contacts[addr], Unread: th.Unread, Pinned: th.Pinned}
		if n := len(th.Msgs); n > 0 {
			last := th.Msgs[n-1]
			e.LastText, e.LastTS, e.LastDir = last.Text, last.TS, last.Dir
		}
		out = append(out, e)
	}
	// Contacts without a thread still appear (so you can start a chat).
	for addr, name := range h.contacts {
		if _, ok := h.threads[addr]; !ok {
			out = append(out, entry{Addr: addr, Name: name})
		}
	}
	h.mu.Unlock()
	// Pinned first, then most-recent first.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].LastTS > out[j].LastTS
	})
	writeJSON(w, out)
}

// handleChatThread mutates one conversation: delete it, or pin/unpin it.
func (s *Server) handleChatThread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer   string `json:"peer"`
		Action string `json:"action"` // delete | clear | pin | unpin
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	addr, err := client.CanonicalChatAddress(req.Peer)
	if err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	h := s.chat
	h.mu.Lock()
	defer h.mu.Unlock()
	switch req.Action {
	case "delete":
		// Drop local history AND the saved contact name: otherwise the contact
		// (named on rename) keeps the row alive in handleChatThreads, so a
		// "deleted" conversation reappears. The server-side inbox is E2E +
		// TTL'd and not ours to delete here.
		delete(h.threads, addr)
		delete(h.contacts, addr)
		h.saveThreadsLocked()
		h.saveContactsLocked()
	case "clear":
		// Wipe local message history but keep the conversation itself — the
		// contact name, server binding, seq counters and delivery ticks all
		// stay, so the chat stays open and the next send keeps numbering
		// correctly. Local-only: the server keeps no delivered messages.
		if th := h.threads[addr]; th != nil {
			th.Msgs = nil
			th.Unread = 0
			h.saveThreadsLocked()
		}
	case "pin", "unpin":
		th := h.threads[addr]
		if th == nil {
			th = &chatThreadFile{}
			h.threads[addr] = th
		}
		th.Pinned = req.Action == "pin"
		h.saveThreadsLocked()
	default:
		http.Error(w, "unknown action", 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleChatMessages returns one conversation (optionally clearing unread).
func (s *Server) handleChatMessages(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	addr, err := client.CanonicalChatAddress(r.URL.Query().Get("peer"))
	if err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	h.mu.Lock()
	th := h.threads[addr]
	if th == nil {
		th = &chatThreadFile{}
	}
	if r.URL.Query().Get("markRead") == "1" && th.Unread != 0 {
		th.Unread = 0
		h.saveThreadsLocked()
	}
	msgs := th.Msgs
	if msgs == nil {
		msgs = []chatStoredMsg{} // a typed-nil slice marshals to null; force []
	}
	resp := map[string]any{
		"peer":        addr,
		"name":        h.contacts[addr],
		"msgs":        msgs,
		"server":      th.Server,
		"accepted":    chatStatusMap(th.Accepted),
		"delivered":   chatStatusMap(th.Delivered),
		"serverSetAt": th.ServerSetAt,
	}
	if len(th.Emojis) > 0 {
		resp["emojis"] = th.Emojis
	}
	h.mu.Unlock()
	writeJSON(w, resp)
}

// handleChatSend uploads one message, streaming progress over SSE.
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer   string `json:"peer"`
		Text   string `json:"text"`
		Server string `json:"server"` // chat server (profile domain) to send via
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	peer, err := client.ParseChatAddress(req.Peer)
	if err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "empty message", 400)
		return
	}

	h := s.chat
	peerStr := client.ChatAddressString(peer)
	ps := h.resolveServer(peerStr, req.Server)
	if ps == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_disabled"})
		return
	}
	chatc, serverKey := ps.client, ps.serverKey

	// Sending requires an identity, and is itself consent to use this server —
	// enable it so the background loop keeps polling for replies.
	h.mu.Lock()
	if h.identity == nil {
		h.mu.Unlock()
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_disabled"})
		return
	}
	if !h.enabled[serverKey] {
		h.enabled[serverKey] = true
		h.saveEnabledLocked()
	}
	h.mu.Unlock()

	h.sendMu.Lock()
	defer h.sendMu.Unlock()

	h.mu.Lock()
	th := h.threads[peerStr]
	if th == nil {
		th = &chatThreadFile{}
		h.threads[peerStr] = th
	}
	boundAtStart := th.Server // to tell a mid-send switch from a disabled reroute
	if th.Server == "" {
		th.Server = serverKey // bind the conversation to this server
	}
	if th.LastOutSeq == nil {
		th.LastOutSeq = make(map[string]uint32)
	}
	lastSeq := th.LastOutSeq[serverKey]
	h.mu.Unlock()
	seq := lastSeq + 1

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	if lastSeq == 0 {
		// First send to this peer through this server (or local state was
		// lost): recover the seq from the server.
		if next, err := chatc.NextSeq(ctx, peer); err == nil && next > seq {
			seq = next
		}
	}

	var hsCount atomic.Int64
	chatc.SetOnHandshake(func(done, total int) {
		s.broadcastChat(map[string]any{
			"type": "progress", "op": "handshake",
			"done": hsCount.Add(1),
		})
	})
	progress := func(done, total int) {
		s.broadcastChat(map[string]any{
			"type": "progress", "op": "send", "peer": peerStr,
			"done": done, "total": total,
		})
	}

	// Auto mode: pick this send's cell budget, then score it by the queries it
	// spent and how many errored — measured as the fetcher's success/failure
	// delta across the send.
	budgetArm := h.applyBudget(ps)
	resp0, err0 := ps.f.QueryTotals()

	res, err := chatc.SendMessage(ctx, peer, seq, text, progress)
	if err != nil {
		var serr *client.ChatStatusError
		if errors.As(err, &serr) && serr.Status == protocol.ChatStatusReplay {
			// Seq raced (another device): retry once just above the
			// server's counter.
			seq = serr.LastAccepted + 1
			res, err = chatc.SendMessage(ctx, peer, seq, text, progress)
		}
	}
	resp1, err1 := ps.f.QueryTotals()
	// queries spent and lost this send; "success" (cells reached the server, incl.
	// a server-status reply) vs a transport failure tips the failure penalty.
	queries := int((resp1 - resp0) + (err1 - err0))
	errs := int(err1 - err0)
	h.recordBudget(ps, budgetArm, queries, errs, chatBudgetSuccess(err))
	if err != nil {
		key := chatStatusKey(err)
		out := map[string]any{"ok": false, "error": key}
		var serr *client.ChatStatusError
		if errors.As(err, &serr) && serr.ResetUnix != 0 {
			out["resetUnix"] = serr.ResetUnix
			out["remaining"] = serr.Remaining
		}
		s.addLog("[chat] send failed: " + err.Error())
		writeJSON(w, out)
		return
	}

	// Send succeeded → server is reachable; clear poll backoff so the next
	// background poll fetches replies immediately instead of waiting.
	h.setBackoff(ps, time.Time{})
	// A send is activity: tighten the poll loop to the fast cadence for snappy
	// replies, and resync the frontend countdown to the new (shorter) schedule.
	h.markActivity()
	h.kickPoll(false)

	// Re-fetch the thread under the lock: a delete may have removed the
	// pre-send pointer during the (multi-minute) upload, and reusing it would
	// persist the map without this conversation, dropping the sent message.
	h.mu.Lock()
	th = h.threads[peerStr]
	if th == nil {
		th = &chatThreadFile{Server: serverKey}
		h.threads[peerStr] = th
	}
	if ns, changed := chatPostSendServer(th.Server, boundAtStart, serverKey); changed {
		// Follow a reroute off a disabled bound server — but NOT a deliberate
		// switch the user made mid-send (that binding must stick).
		th.Server = ns
		th.ServerSetAt = time.Now().Unix()
	}
	if th.LastOutSeq == nil {
		th.LastOutSeq = make(map[string]uint32)
	}
	th.LastOutSeq[serverKey] = res.Seq
	th.bumpStatus(serverKey, res.Seq, 0) // a committed send IS accepted (✓)
	th.Msgs = append(th.Msgs, chatStoredMsg{
		Dir: "out", Seq: res.Seq, Text: text,
		TS: time.Now().Unix(), Server: serverKey,
	})
	h.saveThreadsLocked()
	h.mu.Unlock()

	writeJSON(w, map[string]any{
		"ok": true, "seq": res.Seq,
		"remaining": res.Remaining, "resetUnix": res.ResetUnix,
	})
}

// handleChatSetServer rebinds a conversation to another server, only after
// confirming the peer is registered there.
func (s *Server) handleChatSetServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Peer   string `json:"peer"`
		Server string `json:"server"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	peer, err := client.ParseChatAddress(req.Peer)
	if err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	addr := client.ChatAddressString(peer)
	h := s.chat
	h.mu.Lock()
	ps := h.servers[chatServerKey(req.Server)]
	enabled := ps != nil && h.enabled[ps.serverKey]
	h.mu.Unlock()
	if ps == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_disabled"})
		return
	}
	if !enabled {
		// Must be on first — switching would publish the identity there.
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_server_off"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	reg, err := ps.client.IsRegistered(ctx, peer)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": chatStatusKey(err)})
		return
	}
	if !reg {
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_peer_not_on_server"})
		return
	}

	h.mu.Lock()
	th := h.threads[addr]
	if th == nil {
		th = &chatThreadFile{}
		h.threads[addr] = th
	}
	th.Server = ps.serverKey
	th.ServerSetAt = time.Now().Unix()
	h.saveThreadsLocked()
	h.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "server": ps.serverKey})
}

// handleChatPoll fetches the inbox now, with download progress over SSE.
func (s *Server) handleChatPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	h := s.chat
	h.clearBackoffs() // explicit user action — bypass stale backoffs
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	// Atomic: servers are polled in parallel, so handshakes report concurrently.
	var hsCount atomic.Int64
	hsProgress := func(done, total int) {
		s.broadcastChat(map[string]any{
			"type": "progress", "op": "handshake",
			"done": hsCount.Add(1),
		})
	}
	for _, ps := range h.snapshotServers() {
		ps.client.SetOnHandshake(hsProgress)
	}
	progress := func(done, total int) {
		s.broadcastChat(map[string]any{
			"type": "progress", "op": "poll",
			"done": done, "total": total,
		})
	}
	// budget 0: the manual refresh already bounded itself above, and a big
	// backlog legitimately needs minutes to drain.
	n := h.pollAllServers(ctx, progress, 0)
	if n > 0 {
		h.markActivity()
	}
	// We just polled — reset the background loop's timer so it doesn't poll again
	// right behind us, and resync the frontend countdown to the fresh schedule.
	h.kickPoll(false)
	writeJSON(w, map[string]any{"ok": true, "got": n})
}

// handleChatPeerStatus returns ✓/✓✓ counters and the safety emojis for a
// conversation.
func (s *Server) handleChatPeerStatus(w http.ResponseWriter, r *http.Request) {
	h := s.chat
	peer, err := client.ParseChatAddress(r.URL.Query().Get("peer"))
	if err != nil {
		http.Error(w, "invalid peer", 400)
		return
	}
	addr := client.ChatAddressString(peer)
	ps := h.resolveServer(addr, r.URL.Query().Get("server"))
	if ps == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "chat_err_disabled"})
		return
	}
	chatc := ps.client
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	accepted, delivered, err := chatc.PeerStatus(ctx, peer)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": chatStatusKey(err)})
		return
	}
	// Cache the counters per server so ticks survive a restart and a later
	// server switch doesn't blank this server's delivered messages.
	h.mu.Lock()
	th := h.threads[addr]
	if th != nil && th.bumpStatus(ps.serverKey, accepted, delivered) {
		h.saveThreadsLocked()
	}
	out := map[string]any{"ok": true}
	if th != nil {
		out["accepted"] = chatStatusMap(th.Accepted)
		out["delivered"] = chatStatusMap(th.Delivered)
	} else {
		out["accepted"] = map[string]uint32{ps.serverKey: accepted}
		out["delivered"] = map[string]uint32{ps.serverKey: delivered}
	}
	h.mu.Unlock()
	if rem, reset, known := chatc.Quota(); known {
		out["quota"] = map[string]any{"remaining": rem, "resetUnix": reset}
	}
	if rec, err := chatc.FetchPeerKey(ctx, peer); err == nil {
		h.mu.Lock()
		myPub := h.identity.Identity.Public().(ed25519.PublicKey)
		emojis := chatSafetyEmojis(myPub, rec.IdentityPub)
		if th := h.threads[addr]; th != nil && len(th.Emojis) == 0 {
			th.Emojis = emojis
			h.saveThreadsLocked()
		}
		h.mu.Unlock()
		out["emojis"] = emojis
	}
	writeJSON(w, out)
}
