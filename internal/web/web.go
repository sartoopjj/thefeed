package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/version"
)

//go:embed static
var staticFS embed.FS

// Config holds the client configuration saved in the data directory.
type Config struct {
	Domain string `json:"domain"`
	// ExtraDomains are additional sub-domains the server also answers feed
	// queries on; the client spreads block fetches across the main domain +
	// these (main stays canonical for relay paths). From the d= URI field.
	ExtraDomains []string `json:"extraDomains,omitempty"`
	Key          string   `json:"key"`
	Resolvers    []string `json:"resolvers"`
	// ServerKey is the pinned server signing public key (base64url, the sk=
	// field of a thefeed:// URI). When set, the client verifies feed content
	// against the server's signed ExtraBlocks. Empty = unverified (old config).
	ServerKey string  `json:"serverKey,omitempty"`
	QueryMode string  `json:"queryMode"`
	RateLimit float64 `json:"rateLimit"`
	// Timeout is the per-query DNS timeout in seconds (0 = default 15 s).
	// Also used as the resolver health-check probe timeout.
	Timeout float64 `json:"timeout,omitempty"`
	// Scatter is the number of resolvers queried simultaneously per DNS block request
	// (0 or 1 = sequential, 4 = default parallel pair).
	Scatter int `json:"scatter,omitempty"`
	// AutoScan enables hourly automatic resolver health-check scans.
	// nil means enabled (default); set to a false pointer to disable.
	AutoScan *bool `json:"autoScan,omitempty"`
}

// Profile wraps a Config with a user-chosen nickname and a unique ID.
type Profile struct {
	ID                 string   `json:"id"`
	Nickname           string   `json:"nickname"`
	Config             Config   `json:"config"`
	AutoUpdate         []string `json:"autoUpdate,omitempty"`
	AutoUpdateInterval int      `json:"autoUpdateInterval,omitempty"`
	// PinnedChannels stores the user's pinned channel names (stripped of @).
	// Pinned channels render at the top of each section in the sidebar.
	PinnedChannels []string `json:"pinnedChannels,omitempty"`
}

const (
	// minAutoUpdateInterval is the floor — never tick faster than once per
	// minute, even if the user sets something silly. The DNS path is
	// expensive and the server's own fetch cycle is much longer.
	minAutoUpdateInterval = 60 * time.Second
	// serverFetchSettleDelay is how long after nextFetch we wait before
	// asking the server for fresh data — gives it time to process the
	// upstream Telegram fetch and have a coherent metadata snapshot.
	serverFetchSettleDelay = 30 * time.Second
	// autoUpdateStartupDelay defers the first tick so the initial metadata
	// + resolver checks have a chance to land before we start polling.
	autoUpdateStartupDelay = 30 * time.Second
)

// SavedResolverScore stores persistent resolver performance data.
type SavedResolverScore struct {
	Success int64 `json:"success"`
	Failure int64 `json:"failure"`
	TotalMs int64 `json:"totalMs"`
}

// ActiveList is a named subset of the resolver bank that the user can
// switch between — e.g., "Home", "Office", "Mobile". The currently
// selected list is what the fetcher uses; switching to a different list
// hot-swaps the active resolvers without rescanning.
type ActiveList struct {
	Name      string   `json:"name"`
	Resolvers []string `json:"resolvers"`
	LastUsed  int64    `json:"lastUsed,omitempty"`
}

// ProfileList is the on-disk structure for profiles.json.
type ProfileList struct {
	Active   string    `json:"active"` // ID of active profile
	Profiles []Profile `json:"profiles"`
	// FontSize stores user's preferred font size (0 = default 14).
	FontSize int    `json:"fontSize,omitempty"`
	Debug    bool   `json:"debug,omitempty"`
	Theme    string `json:"theme,omitempty"`
	Lang     string `json:"lang,omitempty"`
	// SkipUpdateVersion is the latest release the user dismissed.
	SkipUpdateVersion string `json:"skipUpdateVersion,omitempty"`
	// ResolverBank is the shared pool of DNS resolvers used by all profiles.
	ResolverBank []string `json:"resolverBank,omitempty"`
	// ResolverScores stores accumulated performance data for bank resolvers.
	ResolverScores map[string]*SavedResolverScore `json:"resolverScores,omitempty"`
	// ActiveLists holds named resolver subsets so the user can keep one
	// list per situation (home, office, mobile data) and switch by name
	// instead of rescanning. The currently-selected list is named in
	// SelectedList; its resolvers are what the fetcher uses.
	//
	// Migration: legacy installs without ActiveLists are upgraded on
	// first load — their last_scan.json (or current bank) becomes a
	// single list named "Default".
	ActiveLists  []ActiveList `json:"activeLists,omitempty"`
	SelectedList string       `json:"selectedList,omitempty"`
	// ScanPromptOff suppresses the startup "scan resolvers?" prompt.
	// Persisted server-side so it survives Android's per-launch port
	// changes (each launch picks a fresh port → different localStorage
	// origin → flag was lost on every restart).
	ScanPromptOff bool `json:"scanPromptOff,omitempty"`

	// MirrorNoteOff suppresses the Mirror (Telemirror) disclaimer note once the
	// user dismisses it. Persisted server-side for the same reason as
	// ScanPromptOff: a fresh client port has empty localStorage and would
	// otherwise re-show the note on every launch.
	MirrorNoteOff bool `json:"mirrorNoteOff,omitempty"`

	// ProfilePicsEnabled enables fetching avatars over DNS when the
	// GitHub relay can't serve them. Off by default.
	ProfilePicsEnabled bool `json:"profilePicsEnabled,omitempty"`

	// Connection settings (formerly per-profile). 0 = use default.
	QueryMode string  `json:"queryMode,omitempty"` // "single" or "double"
	RateLimit float64 `json:"rateLimit,omitempty"` // parallel block fetches
	Scatter   int     `json:"scatter,omitempty"`   // resolvers per block
	Timeout   float64 `json:"timeout,omitempty"`   // DNS query timeout (s)

	// CacheBudgetMB is the disk-cache byte budget in megabytes (0 = default 1024).
	// Applies to media-cache and telemirror images combined. saved-media is exempt.
	CacheBudgetMB int `json:"cacheBudgetMB,omitempty"`

	// ResolverCacheShare gates the shared-resolver-cache feature. Pointer
	// (not plain bool) so we can tell "user never set it" (nil → default
	// on) apart from "user explicitly disabled" (false). New installs and
	// upgrades from before this field existed get the feature on; an
	// explicit opt-out persists across restarts.
	ResolverCacheShare *bool `json:"resolverCacheShare,omitempty"`

	// SeenIDs maps channel name → last-seen message ID. It's the baseline
	// for the per-channel unread-count badge. Persisted server-side so the
	// counts survive the client's loopback port changing (which wipes the
	// WebView localStorage these markers used to live in).
	SeenIDs map[string]int64 `json:"seenIds,omitempty"`
	// SeenHashes maps channel name → last-seen content hash, used for the
	// X/Twitter "NEW" badge where message IDs aren't sequential.
	SeenHashes map[string]int64 `json:"seenHashes,omitempty"`
}

// ShareEnabled returns whether the shared-resolver-cache feature is on.
// Default is on; only an explicit *false disables it.
func (pl *ProfileList) ShareEnabled() bool {
	if pl == nil || pl.ResolverCacheShare == nil {
		return true
	}
	return *pl.ResolverCacheShare
}

// Defaults for the connection settings exposed on the settings page.
const (
	defaultQueryMode = "single"
	defaultRateLimit = 10.0
	defaultScatter   = 6
	defaultTimeout   = 10.0
)

// connectionSettings returns the effective values from pl, substituting
// defaults for any zero field. Centralises the fallback so every place
// that builds a fetcher reads the same numbers.
func connectionSettings(pl *ProfileList) (queryMode string, rateLimit float64, scatter int, timeout float64) {
	queryMode = defaultQueryMode
	rateLimit = defaultRateLimit
	scatter = defaultScatter
	timeout = defaultTimeout
	if pl == nil {
		return
	}
	if pl.QueryMode != "" {
		queryMode = pl.QueryMode
	}
	if pl.RateLimit > 0 {
		rateLimit = pl.RateLimit
	}
	if pl.Scatter > 0 {
		scatter = pl.Scatter
	}
	if pl.Timeout > 0 {
		timeout = pl.Timeout
	}
	return
}

// lastScanData is the on-disk structure for last_scan.json.
type lastScanData struct {
	Resolvers []string `json:"resolvers"`
	ScannedAt int64    `json:"scannedAt"`
}

// channelsCacheEntry is one profile's startup snapshot.
type channelsCacheEntry struct {
	Channels  []protocol.ChannelInfo `json:"channels"`
	NextFetch uint32                 `json:"nextFetch"`
	SavedAt   int64                  `json:"savedAt"`
}

// channelsCacheFile maps profile ID → snapshot.
type channelsCacheFile map[string]*channelsCacheEntry

// Server is the web UI server for thefeed client.
type Server struct {
	dataDir  string
	port     int
	host     string
	password string // admin password; empty means no auth
	// sharedBackend marks a multi-user deployment (e.g. --host 0.0.0.0 with
	// --shared). The UI then keeps unread/seen state per-browser in
	// localStorage instead of the shared server profiles.json, so connected
	// users don't clear each other's unread counts.
	sharedBackend bool

	mu               sync.RWMutex
	config           *Config
	fetcher          *client.Fetcher
	cache            *client.Cache
	channels         []protocol.ChannelInfo
	messages         map[int][]protocol.Message
	telegramLoggedIn bool
	chatAvailable    bool // server advertises the messenger (metadata flag bit)
	nextFetch        uint32
	latestVersion    string
	lastMsgIDs       map[int]uint32 // last seen message IDs per channel
	lastHashes       map[int]uint32 // last seen content hashes per channel

	// checker is the active resolver health-checker; set by initFetcher.
	checker *client.ResolverChecker

	// metaFetchedAt is when channels/nextFetch were last fetched from DNS.
	// refreshChannel reuses the in-memory metadata when it is younger than metaCacheTTL.
	metaFetchedAt time.Time
	metaCacheTTL  time.Duration

	// fetcherCtx/fetcherCancel control the lifetime of the active fetcher's
	// background goroutines (rate limiter, noise, resolver checker).
	// They are cancelled and recreated each time the config changes.
	fetcherCtx    context.Context
	fetcherCancel context.CancelFunc

	// refreshMu / refreshCancels allow a new refresh to cancel an in-progress one.
	// Each channel gets its own cancel func so concurrent channel refreshes are allowed.
	// Key 0 is reserved for metadata-only refreshes.
	refreshMu      sync.Mutex
	refreshCancels map[int]context.CancelFunc

	// chatAdv caches, per chat server key, the last VERIFIED ChatAvailable bit
	// read from that config's metadata — populated by BOTH the feed's regular
	// metadata fetches and the messenger's own, so chat availability rarely needs
	// a dedicated probe and a "no chat" verdict is always signature-backed.
	chatAdvMu sync.Mutex
	chatAdv   map[string]chatAdvEntry

	logMu    sync.RWMutex
	logLines []string

	sseMu   sync.Mutex
	clients map[chan string]struct{}

	stopRefresh chan struct{}

	scanner *client.ResolverScanner

	// titlesMu guards the background title-fetch state.
	// Only one goroutine fetches titles at a time; errors impose a 5-minute backoff.
	titlesMu           sync.Mutex
	titlesLoading      bool
	titlesBackoffUntil time.Time

	// dlMu guards dlProgress. Active media downloads register their block
	// counter here so the frontend can poll /api/media/progress and show
	// per-block updates instead of waiting for byte chunks.
	dlMu       sync.Mutex
	dlProgress map[string]*mediaDLProgress

	// relayInfo caches the latest answer from RelayInfoChannel so the fast
	// media path doesn't pay a DNS round trip per file.
	relayInfo *relayCache

	// mediaCache is a disk-backed store for downloaded media bytes so that
	// multiple devices on the same network share a single DNS-tunnelled
	// fetch. Entries expire after 7 days.
	mediaCache *mediaDiskCache

	// savedMedia is a never-reaped disk store (ttl=0) for media bytes that
	// belong to saved messages, so they survive mediaCache TTL reaping.
	savedMedia *mediaDiskCache

	// savedCrypto seals the Saved store (saved.json) and saved-media blobs at
	// rest. nil only in legacy unit tests that construct Server{} directly.
	savedCrypto *savedCrypto

	// rescanReplaceList: set by handleRescan, consumed (and cleared)
	// by the next SetOnScanDone callback so an explicit user rescan
	// overwrites the selected list instead of just topping it up.
	rescanFlagMu      sync.Mutex
	rescanReplaceList bool

	// profilesMu serialises read-modify-write cycles on profiles.json.
	profilesMu sync.Mutex
	// savedMu serialises read-modify-write cycles on the Saved store and guards
	// the s.savedCrypto pointer + its mutable fields. RWMutex so the lock-state
	// reads on the hot path (handleSavedList etc.) don't serialise behind a slow
	// Argon2 setPassphrase. NEVER take the read lock while already holding the
	// write lock (Go RWMutex isn't reentrant) — the store helpers below hold the
	// write lock, so guard at handler entry, before calling them.
	savedMu sync.RWMutex

	// Optional, removable backup feed (Telegram-via-Translate proxy).
	telemirror *telemirrorHub

	// Optional per-channel profile pictures cache.
	profilePics *profilePicsHub

	// chat is the standalone messenger hub (identity, threads, poll loop).
	chat *chatHub

	// newMsgMu guards newMsgHandler, an optional callback the background chat
	// poll loop invokes when it finds new messages. The mobile bind layer wires
	// it to a native notifier so backgrounded apps can post a system
	// notification (the in-app, foreground case is handled by the web UI).
	newMsgMu      sync.Mutex
	newMsgHandler func(count int)

	// httpSrv is the running *http.Server, captured in serve() so Shutdown()
	// (mobile background) can force-close it. Guarded by srvMu.
	srvMu   sync.Mutex
	httpSrv *http.Server
}

// SetNewMessageHandler registers a callback invoked (off the UI path) whenever
// the background chat poll loop discovers new messages. Pass nil to clear.
func (s *Server) SetNewMessageHandler(fn func(count int)) {
	s.newMsgMu.Lock()
	s.newMsgHandler = fn
	s.newMsgMu.Unlock()
}

// notifyNewMessages fires the registered new-message handler, if any.
func (s *Server) notifyNewMessages(count int) {
	s.newMsgMu.Lock()
	fn := s.newMsgHandler
	s.newMsgMu.Unlock()
	if fn != nil {
		fn(count)
	}
}

// New creates a new web server.
func New(dataDir string, port int, host string, password string) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// Remove stale cache files on every startup, even before a config is loaded.
	go func() {
		if c, err := client.NewCache(filepath.Join(dataDir, "cache")); err == nil {
			_ = c.Cleanup()
		}
	}()

	scanner := client.NewResolverScanner()

	mediaCache, mcErr := newMediaDiskCache(filepath.Join(dataDir, "media-cache"), 7*24*time.Hour)
	if mcErr != nil {
		log.Printf("Warning: media disk cache disabled: %v", mcErr)
	}

	s := &Server{
		dataDir:        dataDir,
		port:           port,
		host:           host,
		password:       password,
		messages:       make(map[int][]protocol.Message),
		clients:        make(map[chan string]struct{}),
		chatAdv:        make(map[string]chatAdvEntry),
		refreshCancels: make(map[int]context.CancelFunc),
		lastMsgIDs:     make(map[int]uint32),
		lastHashes:     make(map[int]uint32),
		scanner:        scanner,
		mediaCache:     mediaCache,
		dlProgress:     make(map[string]*mediaDLProgress),
		relayInfo:      newRelayCache(),
		profilePics:    newProfilePicsHub(dataDir),
	}
	// Set up telemirror with an onUpdate hook so background refreshes
	// push an SSE event; the frontend re-fetches the active channel
	// when it sees the matching event.
	s.telemirror = newTelemirrorHub(dataDir, func(username string) {
		s.broadcast("event: update\ndata: \"telemirror:" + username + "\"\n\n")
	})
	s.chat = newChatHub(s, dataDir)

	if mediaCache != nil {
		go mediaCache.Cleanup()
		go s.runMediaCacheSweep()
	}

	// Saved store at-rest encryption: load or initialise the keyring (device
	// mode on first run). If this fails the Saved feature degrades to plaintext
	// rather than blocking startup.
	if sc, scErr := loadSavedCrypto(dataDir); scErr == nil {
		s.savedCrypto = sc
	}

	// saved-media never expires (ttl=0): saved messages keep their bytes.
	if savedMedia, smErr := newMediaDiskCache(filepath.Join(dataDir, "saved-media"), 0); smErr == nil {
		savedMedia.crypto = s.savedCrypto
		s.savedMedia = savedMedia
	}

	// Apply cache budget from saved preferences.
	if pl, plErr := s.loadProfiles(); plErr == nil && pl != nil {
		s.applyCacheBudget(pl.CacheBudgetMB)
	}

	// Migrate per-profile resolvers into the shared bank on first run.
	s.migrateResolverBank()

	cfg, err := s.loadConfig()
	if err == nil {
		s.config = cfg
		if err := s.initFetcher(); err != nil {
			log.Printf("Warning: could not initialize fetcher: %v", err)
		}
	} else {
		// config.json missing — try to bootstrap from the active profile
		if pl, plErr := s.loadProfiles(); plErr == nil && pl.Active != "" {
			for _, p := range pl.Profiles {
				if p.ID == pl.Active {
					_ = s.saveConfig(&p.Config)
					s.config = &p.Config
					if err := s.initFetcher(); err != nil {
						log.Printf("Warning: could not initialize fetcher from profile: %v", err)
					}
					break
				}
			}
		}
	}

	if cc := s.loadChannelsCache(); cc != nil {
		s.channels = cc.Channels
		s.nextFetch = cc.NextFetch
	}

	return s, nil
}

// Run starts the web server, binding to s.host:s.port.
func (s *Server) Run() error { return s.serve(nil) }

// Serve runs the web server on an already-bound listener. Used by the
// mobile entry where the listener is opened first to discover the
// kernel-assigned port.
func (s *Server) Serve(ln net.Listener) error { return s.serve(ln) }

func (s *Server) serve(ln net.Listener) error {
	mux := http.NewServeMux()

	// no-cache (not no-store): the browser still keeps a copy but must
	// revalidate with the server every time via If-Modified-Since. Without
	// this, embedded assets served under a fixed URL (/static/js/core.js
	// etc.) can be served stale from heuristic cache after a rebuild —
	// e.g. an old core.js paired with a newer nav.js — breaking the page
	// in ways that don't reproduce on a fresh load.
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(staticMinFS)))
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		staticHandler.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/channels", s.handleChannels)
	mux.HandleFunc("/api/messages/", s.handleMessages)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/rescan", s.handleRescan)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/admin", s.handleAdmin)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/api/profiles/switch", s.handleProfileSwitch)
	mux.HandleFunc("/api/profiles/defaults", s.handleProfileDefaults)
	mux.HandleFunc("/api/auto-update", s.handleAutoUpdate)
	mux.HandleFunc("/api/auto-update/toggle", s.handleAutoUpdateToggle)
	mux.HandleFunc("/api/pinned-channels/toggle", s.handlePinnedChannelToggle)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/seen", s.handleSeen)
	mux.HandleFunc("/api/version-check", s.handleVersionCheck)
	mux.HandleFunc("/api/update/github", s.handleGitHubUpdateCheck)
	mux.HandleFunc("/api/update/download", s.handleUpdateDownload)
	mux.HandleFunc("/api/cache/clear", s.handleClearCache)
	mux.HandleFunc("/api/cache/stats", s.handleCacheStats)
	mux.HandleFunc("/api/cache/clear-one", s.handleCacheClearOne)
	mux.HandleFunc("/api/cache/budget", s.handleCacheBudget)
	mux.HandleFunc("/api/bg-image", s.handleBgImage)
	mux.HandleFunc("/api/resolvers/active", s.handleActiveResolvers)
	mux.HandleFunc("/api/resolvers/remove", s.handleRemoveResolver)
	mux.HandleFunc("/api/resolvers/reset-stats", s.handleResetResolverStats)
	mux.HandleFunc("/api/resolvers/bank", s.handleResolverBank)
	mux.HandleFunc("/api/resolvers/bank/cleanup", s.handleResolverBankCleanup)
	mux.HandleFunc("/api/scanner/start", s.handleScannerStart)
	mux.HandleFunc("/api/scanner/stop", s.handleScannerStop)
	mux.HandleFunc("/api/scanner/pause", s.handleScannerPause)
	mux.HandleFunc("/api/scanner/resume", s.handleScannerResume)
	mux.HandleFunc("/api/scanner/progress", s.handleScannerProgress)
	mux.HandleFunc("/api/scanner/apply", s.handleScannerApply)
	mux.HandleFunc("/api/scanner/presets", s.handleScannerPresets)
	mux.HandleFunc("/api/resolvers/lists", s.handleResolverLists)
	mux.HandleFunc("/api/resolvers/lists/select", s.handleResolverListSelect)
	mux.HandleFunc("/api/resolvers/lists/save", s.handleResolverListSave)
	mux.HandleFunc("/api/resolvers/lists/rename", s.handleResolverListRename)
	mux.HandleFunc("/api/resolvers/lists/add", s.handleResolverListAdd)
	mux.HandleFunc("/api/resolvers/lists/remove", s.handleResolverListRemove)
	// Media (image/file) downloader: assembles a binary blob from a media
	// channel and streams it back. See internal/web/media.go for the param
	// contract.
	mux.HandleFunc("/api/media/get", s.handleMediaGet)
	mux.HandleFunc("/api/media/progress", s.handleMediaProgress)
	mux.HandleFunc("/api/saved", s.handleSaved)
	mux.HandleFunc("/api/saved/note", s.handleSavedNote)
	mux.HandleFunc("/api/saved/upload", s.handleSavedUpload)
	mux.HandleFunc("/api/saved/from-chat", s.handleSavedFromChat)
	mux.HandleFunc("/api/saved/lock", s.handleSavedLock)
	mux.HandleFunc("/api/saved/lock/remove", s.handleSavedLockRemove)
	mux.HandleFunc("/api/saved/lock/reset", s.handleSavedLockReset)
	mux.HandleFunc("/api/saved/unlock", s.handleSavedUnlock)
	mux.HandleFunc("/api/saved/pin", s.handleSavedPin)
	mux.HandleFunc("/api/saved/count", s.handleSavedCount)
	mux.HandleFunc("/api/saved/seen", s.handleSavedSeen)
	mux.HandleFunc("/api/saved/media", s.handleSavedMedia)
	mux.HandleFunc("/api/saved/media/persist", s.handleSavedMediaPersist)
	mux.HandleFunc("/api/saved/media/persist-tm", s.handleSavedMediaPersistTm)
	mux.HandleFunc("/api/saved/media/upload-blob", s.handleSavedMediaUploadBlob)
	// Optional telemirror feature — see internal/telemirror/.
	mux.HandleFunc("/api/telemirror/channels", s.telemirror.handleChannels)
	mux.HandleFunc("/api/telemirror/channel/", s.telemirror.handleChannel)
	mux.HandleFunc("/api/telemirror/img", s.telemirror.handleImg)
	mux.HandleFunc("/api/telemirror/avatar/", s.telemirror.handleAvatar)
	mux.HandleFunc("/api/telemirror/older/", s.telemirror.handleOlder)
	// Chat messenger endpoints — see handlers_chat.go.
	mux.HandleFunc("/api/chat/info", s.handleChatInfo)
	mux.HandleFunc("/api/chat/availability", s.handleChatAvailability)
	mux.HandleFunc("/api/chat/probe", s.handleChatProbeServer)
	mux.HandleFunc("/api/chat/enable", s.handleChatEnable)
	mux.HandleFunc("/api/chat/seed", s.handleChatSeed)
	mux.HandleFunc("/api/chat/contacts", s.handleChatContacts)
	mux.HandleFunc("/api/chat/threads", s.handleChatThreads)
	mux.HandleFunc("/api/chat/thread", s.handleChatThread)
	mux.HandleFunc("/api/chat/messages", s.handleChatMessages)
	mux.HandleFunc("/api/chat/setserver", s.handleChatSetServer)
	mux.HandleFunc("/api/chat/send", s.handleChatSend)
	mux.HandleFunc("/api/chat/poll", s.handleChatPoll)
	mux.HandleFunc("/api/chat/peer-status", s.handleChatPeerStatus)
	mux.HandleFunc("/api/chat/settings", s.handleChatSettings)
	// Backup / restore endpoints.
	mux.HandleFunc("/api/backup/export", s.handleBackupExport)
	mux.HandleFunc("/api/backup/preview", s.handleBackupPreview)
	mux.HandleFunc("/api/backup/restore", s.handleBackupRestore)
	// Profile-pics cache + control endpoints.
	mux.HandleFunc("/api/profile-pics/", s.profilePics.handleProfilePic)
	mux.HandleFunc("/api/profile-pics", s.handleProfilePicsList)
	mux.HandleFunc("/api/profile-pics/refresh", s.handleProfilePicsRefresh)
	mux.HandleFunc("/api/profile-pics/progress", s.handleProfilePicsProgress)
	mux.HandleFunc("/", s.handleIndex)

	// Listen on the specified host (default 127.0.0.1)
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	log.Printf("thefeed client %s", version.Version)
	fmt.Printf("\n  Open in browser: http://%s:%d\n\n", s.host, s.port)

	if s.fetcher != nil {
		// Boot-time fast path for the resolver list:
		//   1. The user's currently-selected named list (instant, no
		//      probing) — populated by the migration on first load.
		//   2. last_scan.json (legacy fast path for old installs).
		//   3. Full health-check scan.
		if applied := s.applySelectedList(); applied {
			s.checker.StartPeriodic(s.fetcherCtx)
			go s.refreshMetadataOnly()
		} else if s.reuseKnownResolvers(nil) {
			// Reused last_scan → resolver bank (best-scored, known-dead dropped)
			// with no scan — the same path a config switch takes, so boot and
			// switch behave identically.
		} else {
			s.startCheckerThenRefresh()
		}
	}

	var handler http.Handler = mux
	if s.password != "" {
		pw := s.password
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, pass, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(pw)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="thefeed"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	handler = sameOriginGuard(handler)

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// ReadHeaderTimeout protects against slow-loris on the request
		// header. The body itself can be large (Telegram send-message
		// uploads), and the response can be slow (DNS-tunneled media
		// streams take many minutes for multi-block files in censored
		// networks). So zero out ReadTimeout/WriteTimeout and bound the
		// idle period on the connection itself.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       30 * time.Minute,
	}
	s.srvMu.Lock()
	s.httpSrv = srv
	s.srvMu.Unlock()
	if ln != nil {
		return srv.Serve(ln)
	}
	return srv.ListenAndServe()
}

// Shutdown stops the server's background goroutines and closes the HTTP
// listener. It is the clean counterpart to serve() for embedders that
// reuse the process — notably the iOS/Android bind layer, which calls
// Shutdown when the app is backgrounded so the fetcher/checker/chat
// goroutines stop touching profiles.json while suspended. Without this
// the goroutines leak across every background→foreground cycle and a
// scan suspended mid-flight can corrupt shared state.
func (s *Server) Shutdown() {
	if s == nil {
		return
	}
	// Cancel the active fetcher's goroutine tree (rate limiter, noise,
	// resolver checker, chat poll loop — they all derive from fetcherCtx).
	s.mu.Lock()
	cancel := s.fetcherCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.srvMu.Lock()
	srv := s.httpSrv
	s.srvMu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// sameOriginGuard blocks cross-site STATE-CHANGING requests, the CSRF/DNS-
// rebinding vector against this localhost UI: a hostile web page the user is
// also visiting could otherwise POST to 127.0.0.1 and drive chat/admin actions
// (e.g. enabling a chat server, sending a message). It keys off the browser-set
// Fetch-Metadata header, which is sent on every request and cannot be forged by
// the calling page:
//   - "cross-site" on an unsafe method → reject.
//   - same-origin / same-site / none (direct navigation), or no header at all
//     (curl, native/mobile clients, the in-app webview's own calls) → allow.
//
// Reads (GET/HEAD) are never blocked, so no legitimate same-origin flow breaks.
func sameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-site request blocked", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(staticMinFS, "index.html")
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func (s *Server) initFetcher() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel goroutines from the previous fetcher configuration.
	// This also cancels any in-progress manual rescan (via the context chain).
	// Preserve resolver stats across fetcher re-creation (e.g. profile switch).
	var prevStats map[string][3]int64
	if s.fetcher != nil {
		prevStats = s.fetcher.ExportStats()
		// Persist accumulated stats before destroying the old fetcher.
		s.persistResolverScores(prevStats)
	}
	if s.fetcherCancel != nil {
		s.fetcherCancel()
	}

	cfg := s.config
	if cfg == nil {
		return fmt.Errorf("no config")
	}

	// Load the shared resolver bank and preferences from profiles.json.
	var bankResolvers []string
	var debug bool
	var savedScores map[string]*SavedResolverScore
	if pl, plErr := s.loadProfiles(); plErr == nil {
		debug = pl.Debug
		if len(pl.ResolverBank) > 0 {
			bankResolvers = pl.ResolverBank
		}
		savedScores = pl.ResolverScores
	}

	// Use resolver bank; fall back to per-profile resolvers for backward compat.
	resolvers := cfg.Resolvers
	if len(bankResolvers) > 0 {
		resolvers = bankResolvers
	}

	cacheDir := filepath.Join(s.dataDir, "cache")
	cache, err := client.NewCache(cacheDir)
	if err != nil {
		return fmt.Errorf("create cache: %w", err)
	}

	fetcher, err := client.NewFetcher(cfg.Domain, cfg.Key, resolvers)
	if err != nil {
		return fmt.Errorf("create fetcher: %w", err)
	}
	if len(cfg.ExtraDomains) > 0 {
		fetcher.SetDomains(cfg.ExtraDomains)
	}
	if cfg.ServerKey != "" {
		if err := fetcher.SetServerPublicKey(cfg.ServerKey); err != nil {
			s.addLog("[verify] invalid server key in config: " + err.Error())
		}
	}

	// Restore resolver stats: prefer in-memory stats from the previous fetcher,
	// fall back to persisted scores for fresh starts.
	if prevStats != nil {
		fetcher.ImportStats(prevStats)
	} else if len(savedScores) > 0 {
		m := make(map[string][3]int64)
		for addr, ss := range savedScores {
			key := addr
			if !strings.Contains(key, ":") {
				key += ":53"
			}
			m[key] = [3]int64{ss.Success, ss.Failure, ss.TotalMs}
		}
		fetcher.ImportStats(m)
	}

	// Connection settings are global (live on ProfileList). Defaults
	// apply when fields are zero/empty.
	pl, _ := s.loadProfiles()
	qm, rl, sc, to := connectionSettings(pl)
	if qm == "double" {
		fetcher.SetQueryMode(protocol.QueryMultiLabel)
	}
	fetcher.SetDebug(debug)
	s.scanner.SetDebug(debug)
	if rl > 0 {
		fetcher.SetRateLimit(rl)
	}
	if sc > 1 {
		fetcher.SetScatter(sc)
	}
	timeout := time.Duration(to * float64(time.Second))
	fetcher.SetTimeout(timeout)
	fetcher.SetCacheShare(pl.ShareEnabled())

	fetcher.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})

	// Cache this config's VERIFIED chat-advertised bit from the feed's regular
	// metadata fetches, so the messenger needn't re-probe to learn it.
	if feedKey := chatServerKey(cfg.Domain); feedKey != "" {
		fetcher.SetOnMetaChatAvail(func(advertised, verified bool) {
			s.recordChatAdv(feedKey, advertised, verified)
		})
	}

	// Create a shared context for this fetcher's lifetime.
	ctx, cancel := context.WithCancel(context.Background())
	s.fetcherCtx = ctx
	s.fetcherCancel = cancel

	// Start rate limiter and noise goroutines.
	fetcher.Start(ctx)

	// Initialise resolver health-checker; start it (with initial scan → then refresh)
	// via startCheckerThenRefresh, called by every initFetcher call site.
	checker := client.NewResolverChecker(fetcher, timeout)
	checker.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})
	checker.SetOnScanDone(func(healthy []string) {
		if len(healthy) > 0 {
			s.saveLastScan(healthy)
		}
		s.persistScanResultsToList(healthy)
	})
	// nil means enabled (the default); only an explicit false pointer disables it.
	autoScan := cfg.AutoScan == nil || *cfg.AutoScan
	checker.SetAutoScan(autoScan)
	s.checker = checker

	s.fetcher = fetcher
	s.cache = cache
	go cache.Cleanup() // remove channel files not updated in 7 days

	// Goroutine dies with fetcherCtx, so a profile switch / config change
	// stops it cleanly.
	go s.runAutoUpdateLoop(ctx)

	// Rebuild the chat hub's per-server clients from the current profiles; its
	// poll loop also dies with fetcherCtx.
	if s.chat != nil {
		s.chat.reset(ctx)
	}
	return nil
}

// chatResolvers returns a resolver list for building chat fetchers: the active
// fetcher's healthy resolvers, falling back to the shared bank.
func (s *Server) chatResolvers() []string {
	s.mu.RLock()
	f := s.fetcher
	s.mu.RUnlock()
	if f != nil {
		if rs := f.Resolvers(); len(rs) > 0 {
			return rs
		}
	}
	if pl, err := s.loadProfiles(); err == nil && len(pl.ResolverBank) > 0 {
		return pl.ResolverBank
	}
	return nil
}

// buildChatFetcher creates a lightweight Fetcher for a profile's server, used
// only for chat (no health scanner — it reuses the shared resolver list). Its
// background goroutines die with ctx.
func (s *Server) buildChatFetcher(cfg Config, resolvers []string, ctx context.Context) (*client.Fetcher, error) {
	f, err := client.NewFetcher(cfg.Domain, cfg.Key, resolvers)
	if err != nil {
		return nil, err
	}
	if len(cfg.ExtraDomains) > 0 {
		f.SetDomains(cfg.ExtraDomains)
	}
	if cfg.ServerKey != "" {
		if err := f.SetServerPublicKey(cfg.ServerKey); err != nil {
			return nil, fmt.Errorf("server key: %w", err)
		}
	}
	f.SetActiveResolvers(resolvers)
	f.SetNoiseDisabled(true) // the main feed fetcher already emits cover traffic
	pl, _ := s.loadProfiles()
	// Respect the global debug toggle so chat cell queries (qname + resolvers)
	// are logged just like the main feed's queries.
	f.SetDebug(pl.Debug)
	qm, rl, sc, to := connectionSettings(pl)
	if qm == "double" {
		f.SetQueryMode(protocol.QueryMultiLabel)
	}
	if rl > 0 {
		f.SetRateLimit(rl)
	}
	if sc > 1 {
		f.SetScatter(sc)
	}
	f.SetTimeout(time.Duration(to * float64(time.Second)))
	f.SetLogFunc(func(msg string) { s.addLog(msg) })
	// Cache this config's VERIFIED chat-advertised bit from the messenger's own
	// metadata fetches too (shared with the feed's, keyed by server).
	if chatKey := chatServerKey(cfg.Domain); chatKey != "" {
		f.SetOnMetaChatAvail(func(advertised, verified bool) {
			s.recordChatAdv(chatKey, advertised, verified)
		})
	}
	if main := s.fetcher; main != nil {
		f.SetStatsForward(func(resolver string, ok bool, latency time.Duration) {
			if ok {
				main.RecordSuccess(resolver, latency)
			} else {
				main.RecordFailure(resolver)
			}
		})
	}
	f.Start(ctx)
	return f, nil
}
