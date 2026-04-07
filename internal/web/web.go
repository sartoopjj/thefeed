package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	Domain    string   `json:"domain"`
	Key       string   `json:"key"`
	Resolvers []string `json:"resolvers"`
	QueryMode string   `json:"queryMode"`
	RateLimit float64  `json:"rateLimit"`
	// Timeout is the per-query DNS timeout in seconds (0 = default 15 s).
	// Also used as the resolver health-check probe timeout.
	Timeout float64 `json:"timeout,omitempty"`
	// Scatter is the number of resolvers queried simultaneously per DNS block request
	// (0 or 1 = sequential, 2 = default parallel pair).
	Scatter int `json:"scatter,omitempty"`
}

// Profile wraps a Config with a user-chosen nickname and a unique ID.
type Profile struct {
	ID       string `json:"id"`
	Nickname string `json:"nickname"`
	Config   Config `json:"config"`
}

// ProfileList is the on-disk structure for profiles.json.
type ProfileList struct {
	Active   string    `json:"active"` // ID of active profile
	Profiles []Profile `json:"profiles"`
	// FontSize stores user's preferred font size (0 = default 14).
	FontSize int  `json:"fontSize,omitempty"`
	Debug    bool `json:"debug,omitempty"`
}

// lastScanData is the on-disk structure for last_scan.json.
type lastScanData struct {
	Resolvers []string `json:"resolvers"`
	ScannedAt int64    `json:"scannedAt"`
}

// Server is the web UI server for thefeed client.
type Server struct {
	dataDir  string
	port     int
	password string // admin password; empty means no auth

	mu               sync.RWMutex
	config           *Config
	fetcher          *client.Fetcher
	cache            *client.Cache
	channels         []protocol.ChannelInfo
	messages         map[int][]protocol.Message
	telegramLoggedIn bool
	nextFetch        uint32
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

	// refreshMu / refreshCancel allow a new refresh to cancel an in-progress one.
	// channelFetching tracks which channels are currently being fetched.
	refreshMu       sync.Mutex
	refreshCancel   context.CancelFunc
	channelFetching map[int]bool // prevents duplicate fetches for same channel

	logMu    sync.RWMutex
	logLines []string

	sseMu   sync.Mutex
	clients map[chan string]struct{}

	stopRefresh chan struct{}
}

// New creates a new web server.
func New(dataDir string, port int, password string) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// Remove stale cache files on every startup, even before a config is loaded.
	go func() {
		if c, err := client.NewCache(filepath.Join(dataDir, "cache")); err == nil {
			_ = c.Cleanup()
		}
	}()

	s := &Server{
		dataDir:         dataDir,
		port:            port,
		password:        password,
		messages:        make(map[int][]protocol.Message),
		clients:         make(map[chan string]struct{}),
		channelFetching: make(map[int]bool),
		lastMsgIDs:      make(map[int]uint32),
		lastHashes:      make(map[int]uint32),
	}

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

	return s, nil
}

// Run starts the web server.
func (s *Server) Run() error {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/channels", s.handleChannels)
	mux.HandleFunc("/api/messages/", s.handleMessages)
	mux.HandleFunc("/api/media", s.handleMedia)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/rescan", s.handleRescan)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/admin", s.handleAdmin)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/api/profiles/switch", s.handleProfileSwitch)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/cache/clear", s.handleClearCache)
	mux.HandleFunc("/api/resolvers/apply-saved", s.handleApplySavedResolvers)
	mux.HandleFunc("/", s.handleIndex)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	log.Printf("thefeed client %s", version.Version)
	fmt.Printf("\n  Open in browser: http://%s\n\n", addr)

	if s.fetcher != nil {
		if ls := s.loadLastScan(); ls != nil {
			// Fast path: apply saved healthy resolvers immediately and skip the
			// initial full scan. Only the periodic 30-min checker starts.
			// This gives the UI near-instant channel data on app open.
			s.fetcher.SetActiveResolvers(ls.Resolvers)
			s.checker.StartPeriodic(s.fetcherCtx)
			go s.refreshMetadataOnly()
		} else {
			s.startCheckerThenRefresh()
		}
	}

	var handler http.Handler = mux
	if s.password != "" {
		pw := s.password
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, pass, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(pw)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="thefeed"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	return srv.ListenAndServe()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	status := map[string]any{
		"configured":  s.config != nil,
		"version":     version.Version,
		"hasPassword": s.password != "",
	}
	if s.config != nil {
		status["domain"] = s.config.Domain
		status["channels"] = s.channels
		status["telegramLoggedIn"] = s.telegramLoggedIn
		status["nextFetch"] = s.nextFetch
		// Include last resolver scan if recent (<24 h) so the frontend can offer a quick-start.
		if ls := s.loadLastScan(); ls != nil {
			status["lastScan"] = map[string]any{
				"resolvers": ls.Resolvers,
				"scannedAt": ls.ScannedAt,
				"count":     len(ls.Resolvers),
			}
		}
	}
	writeJSON(w, status)
}

// handleConfig handles GET (read) and POST (write) of client configuration.
// POST is authenticated when a global password is set (via the middleware).
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.config == nil {
			writeJSON(w, map[string]any{"configured": false})
			return
		}
		writeJSON(w, s.config)

	case http.MethodPost:
		var cfg Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if cfg.Domain == "" || cfg.Key == "" || len(cfg.Resolvers) == 0 {
			http.Error(w, "domain, key, and resolvers are required", 400)
			return
		}
		if err := s.saveConfig(&cfg); err != nil {
			http.Error(w, fmt.Sprintf("save config: %v", err), 500)
			return
		}
		s.mu.Lock()
		s.config = &cfg
		s.mu.Unlock()

		if err := s.initFetcher(); err != nil {
			http.Error(w, fmt.Sprintf("init fetcher: %v", err), 500)
			return
		}
		s.startCheckerThenRefresh()
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, s.channels)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "missing channel number", 400)
		return
	}
	chNum, err := strconv.Atoi(parts[3])
	if err != nil || chNum < 1 {
		http.Error(w, "invalid channel number", 400)
		return
	}

	s.mu.RLock()
	msgs := s.messages[chNum]
	chs := s.channels
	cache := s.cache
	s.mu.RUnlock()

	// Serve the persistent on-disk cache when available —
	// it contains the full merged history (up to 200 messages) keyed by channel name.
	if cache != nil && chNum >= 1 && chNum <= len(chs) {
		if result := cache.GetMessages(chs[chNum-1].Name); result != nil {
			writeJSON(w, result)
			return
		}
	}

	// Fall back to the in-memory fresh fetch (no accumulated history).
	writeJSON(w, client.NewMessagesResult(msgs))
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	// Background (quiet) metadata-only refreshes skip silently if one is already running,
	// so the auto-refresh timer never cancels a slow in-progress fetch.
	// Channel refreshes are NOT skipped here — refreshChannel has its own duplicate guard.
	chParam := r.URL.Query().Get("channel")
	if r.URL.Query().Get("quiet") == "1" && chParam == "" {
		s.refreshMu.Lock()
		running := s.refreshCancel != nil
		s.refreshMu.Unlock()
		if running {
			writeJSON(w, map[string]any{"ok": true, "skipped": true})
			return
		}
	}
	if chParam != "" {
		chNum, err := strconv.Atoi(chParam)
		if err != nil || chNum < 1 {
			http.Error(w, "invalid channel", 400)
			return
		}
		go s.refreshChannel(chNum)
	} else {
		go s.refreshMetadataOnly()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		http.Error(w, "token is required", 400)
		return
	}
	s.addLog(fmt.Sprintf("Media download requested (token=%s)", token))

	s.mu.RLock()
	fetcher := s.fetcher
	basectx := s.fetcherCtx
	s.mu.RUnlock()
	if fetcher == nil || basectx == nil {
		http.Error(w, "not configured", 400)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-basectx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	data, name, mimeType, err := fetcher.FetchMedia(ctx, token)
	if err != nil {
		log.Printf("[web] media token=%s: %v", token, err)
		s.addLog(fmt.Sprintf("Media download failed (token=%s)", token))
		http.Error(w, "failed to fetch media", 500)
		return
	}

	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if name == "" {
		name = "media.bin"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	s.addLog(fmt.Sprintf("Media ready: %s (%d bytes)", name, len(data)))
	w.Write(data)
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.mu.RLock()
	checker := s.checker
	baseCtx := s.fetcherCtx
	s.mu.RUnlock()
	if checker == nil || baseCtx == nil {
		http.Error(w, "not configured", 400)
		return
	}
	go func() {
		// Cancel any in-progress metadata refresh so it doesn't race with the
		// scan — we want fresh resolver data before we hit DNS again.
		s.refreshMu.Lock()
		if s.refreshCancel != nil {
			s.refreshCancel()
			s.refreshCancel = nil
		}
		s.refreshMu.Unlock()

		if checker.CheckNow(baseCtx) {
			// Cool-down: give resolvers time to recover from the scan's DNS
			// queries before we immediately hit them again with a fetch.
			sleep := 3*time.Second + time.Duration(mrand.IntN(13))*time.Second // 3–15 s
			select {
			case <-baseCtx.Done():
				return
			case <-time.After(sleep):
			}
			s.refreshMetadataOnly()
		}
	}()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	var req struct {
		Channel int    `json:"channel"`
		Text    string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.Channel < 1 || req.Text == "" {
		http.Error(w, "channel and text are required", 400)
		return
	}
	if len(req.Text) > 4000 {
		http.Error(w, "message too long (max 4000 chars)", 400)
		return
	}

	s.mu.RLock()
	fetcher := s.fetcher
	basectx := s.fetcherCtx
	s.mu.RUnlock()

	if fetcher == nil || basectx == nil {
		http.Error(w, "not configured", 400)
		return
	}

	ctx, cancel := context.WithTimeout(basectx, 5*time.Minute)
	defer cancel()

	s.addLog(fmt.Sprintf("Sending message to channel %d (%d chars)...", req.Channel, len(req.Text)))

	if err := fetcher.SendMessage(ctx, req.Channel, req.Text); err != nil {
		log.Printf("[web] send error ch=%d: %v", req.Channel, err)
		s.addLog("Error: failed to send message")
		http.Error(w, "failed to send message", 500)
		return
	}

	s.addLog("Message sent successfully")
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	var req struct {
		Command string `json:"command"`
		Arg     string `json:"arg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.Command == "" {
		http.Error(w, "command is required", 400)
		return
	}

	s.mu.RLock()
	fetcher := s.fetcher
	basectx := s.fetcherCtx
	s.mu.RUnlock()

	if fetcher == nil || basectx == nil {
		http.Error(w, "not configured", 400)
		return
	}

	ctx, cancel := context.WithTimeout(basectx, 5*time.Minute)
	defer cancel()

	s.addLog(fmt.Sprintf("Admin command: %s %s", req.Command, req.Arg))

	var cmd protocol.AdminCmd
	switch req.Command {
	case "add_channel":
		cmd = protocol.AdminCmdAddChannel
	case "remove_channel":
		cmd = protocol.AdminCmdRemoveChannel
	case "list_channels":
		cmd = protocol.AdminCmdListChannels
	case "refresh":
		cmd = protocol.AdminCmdRefresh
	default:
		http.Error(w, "unknown command", 400)
		return
	}

	result, err := fetcher.SendAdminCommand(ctx, cmd, req.Arg)
	if err != nil {
		log.Printf("[web] admin error: %v", err)
		s.addLog(fmt.Sprintf("Admin error: %v", err))
		http.Error(w, "admin command failed", 500)
		return
	}

	s.addLog(fmt.Sprintf("Admin result: %s", result))
	writeJSON(w, map[string]any{"ok": true, "result": result})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	// Disable the server-wide WriteTimeout for this long-lived SSE connection.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 500)
	s.sseMu.Lock()
	s.clients[ch] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.clients, ch)
		s.sseMu.Unlock()
	}()

	s.logMu.RLock()
	for _, line := range s.logLines {
		data, _ := json.Marshal(line)
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
	}
	s.logMu.RUnlock()
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			// SSE comment line as heartbeat — keeps the connection alive and
			// lets us detect a dead client (write error).
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg := <-ch:
			if _, err := fmt.Fprint(w, msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) broadcast(event string) {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *Server) addLog(msg string) {
	ts := time.Now().Format("15:04:05")
	line := fmt.Sprintf("%s %s", ts, msg)

	s.logMu.Lock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	s.logMu.Unlock()

	data, _ := json.Marshal(line)
	s.broadcast(fmt.Sprintf("event: log\ndata: %s\n\n", data))
}

func (s *Server) initFetcher() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel goroutines from the previous fetcher configuration.
	// This also cancels any in-progress manual rescan (via the context chain).
	if s.fetcherCancel != nil {
		s.fetcherCancel()
	}

	cfg := s.config
	if cfg == nil {
		return fmt.Errorf("no config")
	}

	cacheDir := filepath.Join(s.dataDir, "cache")
	cache, err := client.NewCache(cacheDir)
	if err != nil {
		return fmt.Errorf("create cache: %w", err)
	}

	fetcher, err := client.NewFetcher(cfg.Domain, cfg.Key, cfg.Resolvers)
	if err != nil {
		return fmt.Errorf("create fetcher: %w", err)
	}

	if cfg.QueryMode == "double" {
		fetcher.SetQueryMode(protocol.QueryMultiLabel)
	}
	// Use global debug preference from profiles.json.
	var debug bool
	if pl, err := s.loadProfiles(); err == nil {
		debug = pl.Debug
	}
	fetcher.SetDebug(debug)
	if cfg.RateLimit > 0 {
		fetcher.SetRateLimit(cfg.RateLimit)
	}
	if cfg.Scatter > 1 {
		fetcher.SetScatter(cfg.Scatter)
	}

	timeout := 15 * time.Second
	if cfg.Timeout > 0 {
		timeout = time.Duration(cfg.Timeout * float64(time.Second))
	}
	fetcher.SetTimeout(timeout)

	fetcher.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})

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
	})
	s.checker = checker

	s.fetcher = fetcher
	s.cache = cache
	go cache.Cleanup() // remove channel files not updated in 7 days
	return nil
}

// startCheckerThenRefresh runs the resolver health-check pass synchronously
// (in a new goroutine), then starts the periodic checker and fetches metadata.
// This ensures fresh resolver data is used for the very first metadata query.
func (s *Server) startCheckerThenRefresh() {
	s.mu.RLock()
	checker := s.checker
	ctx := s.fetcherCtx
	s.mu.RUnlock()
	if checker == nil {
		return
	}

	checker.StartAndNotify(ctx, func() {
		s.refreshMetadataOnly()
	})
}

// nextFetchDeadline returns the Time when the server will next fetch from Telegram.
// Returns zero value if nextFetch is not set or has already passed.
func (s *Server) nextFetchDeadline() time.Time {
	s.mu.RLock()
	nf := s.nextFetch
	s.mu.RUnlock()
	if nf == 0 {
		return time.Time{}
	}
	t := time.Unix(int64(nf), 0)
	if time.Until(t) <= 0 {
		return time.Time{} // already passed
	}
	return t
}

// waitForServerFetch blocks until the server's Telegram fetch is likely complete
// (nextFetch + 45 s), emitting a countdown progress event each second so the UI
// can render a live progress bar. Returns true on completion, false if ctx cancelled.
func (s *Server) waitForServerFetch(ctx context.Context, nf uint32) bool {
	const serverFetchDuration = 45 * time.Second
	deadline := time.Unix(int64(nf), 0).Add(serverFetchDuration)
	totalWait := time.Until(deadline)
	if totalWait <= 0 {
		totalWait = serverFetchDuration
	}
	totalSec := int(totalWait.Seconds()) + 1

	s.addLog(fmt.Sprintf("SERVER_FETCH_WAIT start %d", totalSec))

	timer := time.NewTimer(totalWait)
	defer timer.Stop()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			s.addLog("SERVER_FETCH_WAIT done")
			return false
		case <-timer.C:
			s.addLog("SERVER_FETCH_WAIT done")
			return true
		case <-ticker.C:
			remaining := int((totalWait - time.Since(start)).Seconds())
			if remaining < 0 {
				remaining = 0
			}
			s.addLog(fmt.Sprintf("SERVER_FETCH_WAIT tick %d/%d", remaining, totalSec))
		}
	}
}

func (s *Server) refreshMetadataOnly() {
	// Don't fetch before resolver scanning has found at least one healthy resolver.
	// The onFirstDone callback in startCheckerThenRefresh is the canonical first trigger.
	s.mu.RLock()
	fetcherEarly := s.fetcher
	s.mu.RUnlock()
	if fetcherEarly != nil && len(fetcherEarly.Resolvers()) == 0 {
		s.addLog("Waiting for resolver scan to complete...")
		return
	}

	// Cancel any in-progress refresh and start a new cancellable one.
	s.refreshMu.Lock()
	if s.refreshCancel != nil {
		s.refreshCancel()
	}

	s.mu.RLock()
	basectx := s.fetcherCtx
	fetcher := s.fetcher
	cache := s.cache
	s.mu.RUnlock()

	if fetcher == nil || basectx == nil {
		s.refreshMu.Unlock()
		return
	}

	// Child context: cancelled either by the next refresh call or by a config change.
	ctx, cancel := context.WithCancel(basectx)
	s.refreshCancel = cancel
	s.refreshMu.Unlock()
	defer func() {
		cancel()
		s.refreshMu.Lock()
		s.refreshCancel = nil
		s.refreshMu.Unlock()
	}()

	s.addLog(fmt.Sprintf("Fetching metadata... (%d active resolvers)", len(fetcher.Resolvers())))

	// If the server's next Telegram fetch is imminent (within 5 s), wait for it first.
	if dl := s.nextFetchDeadline(); !dl.IsZero() && time.Until(dl) < 5*time.Second {
		s.mu.RLock()
		nf := s.nextFetch
		s.mu.RUnlock()
		if !s.waitForServerFetch(ctx, nf) {
			return
		}
	}

	meta, err := fetcher.FetchMetadata(ctx)
	if err != nil {
		if ctx.Err() != nil {
			s.addLog("Refresh cancelled")
			return
		}
		// Detect invalid passphrase from crypto errors
		errStr := err.Error()
		if strings.Contains(errStr, "integrity check failed") || strings.Contains(errStr, "message authentication failed") || strings.Contains(errStr, "cipher") {
			s.addLog("Error: Invalid passphrase — check your encryption key in Settings")
		} else {
			s.addLog(fmt.Sprintf("Error: %v", err))
		}
		return
	}

	s.mu.Lock()
	s.channels = meta.Channels
	s.telegramLoggedIn = meta.TelegramLoggedIn
	s.nextFetch = meta.NextFetch
	s.metaFetchedAt = time.Now()
	s.mu.Unlock()

	if cache != nil {
		_ = cache.PutMetadata(meta)
	}

	s.broadcast("event: update\ndata: \"channels\"\n\n")
}

func (s *Server) refreshChannel(channelNum int) {
	// Prevent duplicate fetches for the same channel
	s.refreshMu.Lock()
	if s.channelFetching[channelNum] {
		s.refreshMu.Unlock()
		return
	}
	if s.refreshCancel != nil {
		s.refreshCancel()
	}
	s.channelFetching[channelNum] = true

	s.mu.RLock()
	basectx := s.fetcherCtx
	fetcher := s.fetcher
	cache := s.cache
	s.mu.RUnlock()

	if fetcher == nil || basectx == nil {
		delete(s.channelFetching, channelNum)
		s.refreshMu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(basectx)
	s.refreshCancel = cancel
	s.refreshMu.Unlock()
	defer func() {
		cancel()
		s.refreshMu.Lock()
		s.refreshCancel = nil
		delete(s.channelFetching, channelNum)
		s.refreshMu.Unlock()
	}()

	// Use the cached in-memory metadata if it is fresh enough (< metaCacheTTL, default 3 min).
	// This avoids a redundant metadata DNS fetch for every channel refresh.
	// If the metadata is stale (or was never fetched), fetch it from DNS now.
	s.mu.RLock()
	ttl := s.metaCacheTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	// Cap TTL at the time remaining until the server's next Telegram fetch.
	// If nextFetch is sooner than our TTL the cached metadata may already be stale.
	if nf := s.nextFetch; nf > 0 {
		if rem := time.Until(time.Unix(int64(nf), 0)); rem > 0 && rem < ttl {
			ttl = rem
		}
	}
	cachedChannels := s.channels
	cachedAge := time.Since(s.metaFetchedAt)
	s.mu.RUnlock()

	var meta *protocol.Metadata
	if len(cachedChannels) > 0 && cachedAge < ttl {
		// Build a lightweight Metadata from the cached fields to keep the rest of the
		// function unchanged.
		s.mu.RLock()
		meta = &protocol.Metadata{
			Channels:         s.channels,
			TelegramLoggedIn: s.telegramLoggedIn,
			NextFetch:        s.nextFetch,
		}
		s.mu.RUnlock()
	} else {
		var err error
		meta, err = fetcher.FetchMetadata(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.addLog("Refresh cancelled")
				return
			}
			errStr := err.Error()
			if strings.Contains(errStr, "integrity check failed") || strings.Contains(errStr, "message authentication failed") || strings.Contains(errStr, "cipher") {
				s.addLog("Error: Invalid passphrase — check your encryption key in Settings")
			} else {
				s.addLog(fmt.Sprintf("Error: %v", err))
			}
			return
		}
		s.mu.Lock()
		s.channels = meta.Channels
		s.telegramLoggedIn = meta.TelegramLoggedIn
		s.nextFetch = meta.NextFetch
		s.metaFetchedAt = time.Now()
		s.mu.Unlock()
		if cache != nil {
			_ = cache.PutMetadata(meta)
		}
		s.broadcast("event: update\ndata: \"channels\"\n\n")
	}

	channels := meta.Channels
	if channelNum < 1 || channelNum > len(channels) {
		s.addLog(fmt.Sprintf("Warning: channel %d is not available", channelNum))
		return
	}

	ch := channels[channelNum-1]

	// Skip refresh if the last message ID and content hash haven't changed
	// AND we already have messages stored for this channel.
	s.mu.RLock()
	prevID := s.lastMsgIDs[channelNum]
	prevHash := s.lastHashes[channelNum]
	prevMsgs := s.messages[channelNum]
	s.mu.RUnlock()
	if prevID > 0 && ch.LastMsgID == prevID && ch.ContentHash == prevHash && len(prevMsgs) > 0 {
		s.addLog(fmt.Sprintf("Channel %s: no changes (last ID: %d)", ch.Name, prevID))
		s.broadcast(fmt.Sprintf("event: update\ndata: {\"type\":\"messages\",\"channel\":%d}\n\n", channelNum))
		return
	}

	blockCount := int(ch.Blocks)
	if blockCount <= 0 {
		s.mu.Lock()
		s.messages[channelNum] = nil
		s.lastMsgIDs[channelNum] = ch.LastMsgID
		s.lastHashes[channelNum] = ch.ContentHash
		s.mu.Unlock()
		s.addLog(fmt.Sprintf("Updated %s: 0 messages", ch.Name))
		s.broadcast(fmt.Sprintf("event: update\ndata: {\"type\":\"messages\",\"channel\":%d}\n\n", channelNum))
		return
	}

	// Wrap the context with a deadline at the server's next Telegram fetch.
	// If the server starts fetching during our block download we cancel early,
	// wait for the fresh data to land, then restart this channel fetch.
	fetchCtx := ctx
	var fetchCancel context.CancelFunc
	var fetchNF uint32
	if dl := s.nextFetchDeadline(); !dl.IsZero() {
		s.mu.RLock()
		fetchNF = s.nextFetch
		s.mu.RUnlock()
		fetchCtx, fetchCancel = context.WithDeadline(ctx, dl)
		defer fetchCancel()
	}

	var msgs []protocol.Message
	var err error
	msgs, err = fetcher.FetchChannel(fetchCtx, channelNum, blockCount)
	if err != nil {
		if fetchCancel != nil && fetchCtx.Err() == context.DeadlineExceeded {
			// nextFetch fired mid-download — wait for the server, then re-fetch.
			fetchCancel()
			if s.waitForServerFetch(ctx, fetchNF) {
				go s.refreshChannel(channelNum)
			}
			return
		}
		if ctx.Err() != nil {
			s.addLog("Refresh cancelled")
			return
		}
		s.addLog(fmt.Sprintf("Channel %s error: %v", ch.Name, err))
		return
	}

	s.mu.Lock()
	s.messages[channelNum] = msgs
	// Only store the metadata IDs when we actually received messages.
	// If the fetch returned 0 messages but the channel has content (LastMsgID > 0),
	// keep the old IDs so the next refresh will try a full fetch instead of skipping.
	if len(msgs) > 0 || ch.LastMsgID == 0 {
		s.lastMsgIDs[channelNum] = ch.LastMsgID
		s.lastHashes[channelNum] = ch.ContentHash
	}
	s.mu.Unlock()

	if cache != nil {
		if result, mergeErr := cache.MergeAndPut(ch.Name, msgs); mergeErr == nil {
			// Replace the in-memory store with the full merged history.
			s.mu.Lock()
			s.messages[channelNum] = result.Messages
			s.mu.Unlock()
		}
	}

	s.addLog(fmt.Sprintf("Updated %s: %d messages", ch.Name, len(msgs)))
	s.broadcast(fmt.Sprintf("event: update\ndata: {\"type\":\"messages\",\"channel\":%d}\n\n", channelNum))
}

func (s *Server) loadConfig() (*Config, error) {
	path := filepath.Join(s.dataDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// saveLastScan persists the healthy resolver list from the most recent scan.
func (s *Server) saveLastScan(resolvers []string) {
	d := lastScanData{Resolvers: resolvers, ScannedAt: time.Now().Unix()}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.dataDir, "last_scan.json"), b, 0600)
}

// loadLastScan reads the most recent resolver scan result.
// Returns nil when the file doesn't exist or is older than 24 hours.
func (s *Server) loadLastScan() *lastScanData {
	b, err := os.ReadFile(filepath.Join(s.dataDir, "last_scan.json"))
	if err != nil {
		return nil
	}
	var d lastScanData
	if err := json.Unmarshal(b, &d); err != nil {
		return nil
	}
	if len(d.Resolvers) == 0 || time.Since(time.Unix(d.ScannedAt, 0)) > 24*time.Hour {
		return nil
	}
	return &d
}

// handleApplySavedResolvers immediately activates the resolvers from the last
// scan file, letting the UI skip the current scan and start fetching channels.
func (s *Server) handleApplySavedResolvers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	ls := s.loadLastScan()
	if ls == nil {
		http.Error(w, "no saved scan", 400)
		return
	}
	s.mu.RLock()
	fetcher := s.fetcher
	s.mu.RUnlock()
	if fetcher == nil {
		http.Error(w, "not configured", 400)
		return
	}
	fetcher.SetActiveResolvers(ls.Resolvers)
	go s.refreshMetadataOnly()
	writeJSON(w, map[string]any{"ok": true, "count": len(ls.Resolvers)})
}

func (s *Server) saveConfig(cfg *Config) error {
	path := filepath.Join(s.dataDir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) loadProfiles() (*ProfileList, error) {
	path := filepath.Join(s.dataDir, "profiles.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pl ProfileList
	if err := json.Unmarshal(data, &pl); err != nil {
		return nil, err
	}
	return &pl, nil
}

func (s *Server) saveProfiles(pl *ProfileList) error {
	path := filepath.Join(s.dataDir, "profiles.json")
	data, err := json.MarshalIndent(pl, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// handleProfiles manages CRUD for config profiles.
// GET: returns profile list. POST: create/update/delete profiles.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pl, err := s.loadProfiles()
		if err != nil {
			// Migrate existing config.json into a profile
			pl = &ProfileList{}
			if s.config != nil {
				p := Profile{
					ID:       generateID(),
					Nickname: s.config.Domain,
					Config:   *s.config,
				}
				pl.Profiles = []Profile{p}
				pl.Active = p.ID
				_ = s.saveProfiles(pl)
			}
		}
		writeJSON(w, pl)

	case http.MethodPost:
		var req struct {
			Action  string   `json:"action"` // "create", "update", "delete", "reorder"
			Profile Profile  `json:"profile"`
			Order   []string `json:"order"` // for reorder
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		pl, _ := s.loadProfiles()
		if pl == nil {
			pl = &ProfileList{}
		}

		needsReinit := false

		switch req.Action {
		case "create":
			req.Profile.ID = generateID()
			if req.Profile.Nickname == "" {
				req.Profile.Nickname = req.Profile.Config.Domain
			}
			pl.Profiles = append(pl.Profiles, req.Profile)
			if len(pl.Profiles) == 1 {
				pl.Active = req.Profile.ID
				needsReinit = true
			}

		case "update":
			for i, p := range pl.Profiles {
				if p.ID == req.Profile.ID {
					pl.Profiles[i] = req.Profile
					if p.ID == pl.Active {
						needsReinit = true
					}
					break
				}
			}

		case "delete":
			for i, p := range pl.Profiles {
				if p.ID == req.Profile.ID {
					pl.Profiles = append(pl.Profiles[:i], pl.Profiles[i+1:]...)
					if pl.Active == req.Profile.ID {
						pl.Active = ""
						if len(pl.Profiles) > 0 {
							pl.Active = pl.Profiles[0].ID
							needsReinit = true
						}
					}
					break
				}
			}

		case "reorder":
			if len(req.Order) > 0 {
				ordered := make([]Profile, 0, len(pl.Profiles))
				byID := make(map[string]Profile)
				for _, p := range pl.Profiles {
					byID[p.ID] = p
				}
				for _, id := range req.Order {
					if p, ok := byID[id]; ok {
						ordered = append(ordered, p)
					}
				}
				pl.Profiles = ordered
			}

		default:
			http.Error(w, "unknown action", 400)
			return
		}

		if err := s.saveProfiles(pl); err != nil {
			http.Error(w, fmt.Sprintf("save profiles: %v", err), 500)
			return
		}

		// Only re-init the fetcher when the active profile's config was modified.
		if needsReinit && pl.Active != "" {
			for _, p := range pl.Profiles {
				if p.ID == pl.Active {
					_ = s.saveConfig(&p.Config)
					s.mu.Lock()
					s.config = &p.Config
					s.mu.Unlock()
					if err := s.initFetcher(); err != nil {
						log.Printf("[web] re-init fetcher after profile change: %v", err)
					} else {
						s.startCheckerThenRefresh()
					}
					break
				}
			}
		}

		writeJSON(w, map[string]any{"ok": true, "profiles": pl})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleProfileSwitch switches the active profile and re-initializes the fetcher.
func (s *Server) handleProfileSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	pl, err := s.loadProfiles()
	if err != nil || pl == nil {
		http.Error(w, "no profiles", 400)
		return
	}
	var found *Profile
	for i, p := range pl.Profiles {
		if p.ID == req.ID {
			found = &pl.Profiles[i]
			break
		}
	}
	if found == nil {
		http.Error(w, "profile not found", 404)
		return
	}
	pl.Active = found.ID
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, fmt.Sprintf("save: %v", err), 500)
		return
	}
	if err := s.saveConfig(&found.Config); err != nil {
		http.Error(w, fmt.Sprintf("save config: %v", err), 500)
		return
	}

	// Reset state
	s.mu.Lock()
	s.config = &found.Config
	s.channels = nil
	s.messages = make(map[int][]protocol.Message)
	s.lastMsgIDs = make(map[int]uint32)
	s.lastHashes = make(map[int]uint32)
	s.mu.Unlock()

	if err := s.initFetcher(); err != nil {
		http.Error(w, fmt.Sprintf("init fetcher: %v", err), 500)
		return
	}
	s.startCheckerThenRefresh()
	writeJSON(w, map[string]any{"ok": true})
}

// handleSettings manages user preferences (font size etc.).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pl, _ := s.loadProfiles()
		if pl == nil {
			pl = &ProfileList{}
		}
		writeJSON(w, map[string]any{"fontSize": pl.FontSize, "debug": pl.Debug, "version": version.Version, "commit": version.Commit})

	case http.MethodPost:
		var req struct {
			FontSize int  `json:"fontSize"`
			Debug    bool `json:"debug"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if req.FontSize < 10 {
			req.FontSize = 0
		}
		if req.FontSize > 24 {
			req.FontSize = 24
		}
		pl, _ := s.loadProfiles()
		if pl == nil {
			pl = &ProfileList{}
		}
		pl.FontSize = req.FontSize
		pl.Debug = req.Debug
		if err := s.saveProfiles(pl); err != nil {
			http.Error(w, fmt.Sprintf("save: %v", err), 500)
			return
		}
		// Apply debug to the current fetcher session immediately.
		s.mu.RLock()
		f := s.fetcher
		s.mu.RUnlock()
		if f != nil {
			f.SetDebug(req.Debug)
		}
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleClearCache deletes all files in the cache directory.
func (s *Server) handleClearCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	cacheDir := filepath.Join(s.dataDir, "cache")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		writeJSON(w, map[string]any{"ok": true, "deleted": 0})
		return
	}
	deleted := 0
	for _, e := range entries {
		if !e.IsDir() {
			if os.Remove(filepath.Join(cacheDir, e.Name())) == nil {
				deleted++
			}
		}
	}
	s.addLog(fmt.Sprintf("Cache cleared: %d files deleted", deleted))
	writeJSON(w, map[string]any{"ok": true, "deleted": deleted})
}
