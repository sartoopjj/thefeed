package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
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
	// Timeout is the per-query DNS timeout in seconds (0 = default 5 s).
	// Also used as the resolver health-check probe timeout.
	Timeout float64 `json:"timeout,omitempty"`
	// Debug enables verbose query logging (shows generated DNS query names).
	Debug bool `json:"debug,omitempty"`
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
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/admin", s.handleAdmin)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/", s.handleIndex)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	log.Printf("thefeed client %s", version.Version)
	fmt.Printf("\n  Open in browser: http://%s\n\n", addr)

	if s.fetcher != nil {
		go s.refreshMetadataOnly()
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
		go s.refreshMetadataOnly()
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
	s.mu.RUnlock()

	writeJSON(w, msgs)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	// Background (quiet) refreshes skip silently if one is already running,
	// so the auto-refresh timer never cancels a slow in-progress fetch.
	if r.URL.Query().Get("quiet") == "1" {
		s.refreshMu.Lock()
		running := s.refreshCancel != nil
		s.refreshMu.Unlock()
		if running {
			writeJSON(w, map[string]any{"ok": true, "skipped": true})
			return
		}
	}
	chParam := r.URL.Query().Get("channel")
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 100)
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
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			fmt.Fprint(w, msg)
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
	fetcher.SetDebug(cfg.Debug)
	if cfg.RateLimit > 0 {
		fetcher.SetRateLimit(cfg.RateLimit)
	}

	timeout := 10 * time.Second
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

	// Start periodic resolver health checks (runs first check in background immediately).
	checker := client.NewResolverChecker(fetcher, timeout)
	checker.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})
	checker.Start(ctx)

	s.fetcher = fetcher
	s.cache = cache
	return nil
}

func (s *Server) refreshMetadataOnly() {
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

	s.addLog("Fetching metadata...")
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
		s.addLog(fmt.Sprintf("Channel %d is already being fetched, skipping", channelNum))
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

	meta, err := fetcher.FetchMetadata(ctx)
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
	s.mu.Unlock()

	if cache != nil {
		_ = cache.PutMetadata(meta)
	}
	s.broadcast("event: update\ndata: \"channels\"\n\n")

	channels := meta.Channels
	if channelNum < 1 || channelNum > len(channels) {
		s.addLog(fmt.Sprintf("Warning: channel %d is not available", channelNum))
		return
	}

	ch := channels[channelNum-1]

	// Skip refresh if the last message ID and content hash haven't changed
	s.mu.RLock()
	prevID := s.lastMsgIDs[channelNum]
	prevHash := s.lastHashes[channelNum]
	s.mu.RUnlock()
	if prevID > 0 && ch.LastMsgID == prevID && ch.ContentHash == prevHash {
		s.addLog(fmt.Sprintf("Channel %s: no changes (last ID: %d)", ch.Name, prevID))
		s.broadcast("event: update\ndata: \"messages\"\n\n")
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
		s.broadcast("event: update\ndata: \"messages\"\n\n")
		return
	}

	var msgs []protocol.Message
	msgs, err = fetcher.FetchChannel(ctx, channelNum, blockCount)
	if err != nil {
		if ctx.Err() != nil {
			s.addLog("Refresh cancelled")
			return
		}
		s.addLog(fmt.Sprintf("Channel %s error: %v", ch.Name, err))
		return
	}

	s.mu.Lock()
	s.messages[channelNum] = msgs
	s.lastMsgIDs[channelNum] = ch.LastMsgID
	s.lastHashes[channelNum] = ch.ContentHash
	s.mu.Unlock()

	if cache != nil {
		_ = cache.PutMessages(channelNum, msgs)
	}

	s.addLog(fmt.Sprintf("Updated %s: %d messages", ch.Name, len(msgs)))
	s.broadcast("event: update\ndata: \"messages\"\n\n")
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
