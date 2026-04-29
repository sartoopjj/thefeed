package server

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Config holds server configuration.
type Config struct {
	ListenAddr    string
	Domain        string
	Passphrase    string
	ChannelsFile  string
	XAccountsFile string
	XRSSInstances string
	MaxPadding    int
	MsgLimit      int  // max messages per channel (0 = default 15)
	NoTelegram    bool // if true, fetch public channels without Telegram login
	AllowManage   bool // if true, remote channel management and sending via DNS is allowed
	Debug         bool // if true, log every decoded DNS query
	// DNSMediaEnabled toggles the slow DNS-relay path. When false the
	// server still ingests media bytes (so other relays can serve them)
	// but the wire-format DNS flag is unset for clients.
	DNSMediaEnabled     bool
	DNSMediaMaxSize     int64  // per-file cap for the DNS relay (0 = no cap)
	DNSMediaCacheTTL    int    // DNS-relay TTL in minutes
	DNSMediaCompression string // DNS-relay compression: none|gzip|deflate
	FetchInterval       time.Duration // 0 = default 10m; floor enforced by main
	GitHubRelay         GitHubRelayConfig
	Telegram            TelegramConfig
}

// GitHubRelayConfig configures the GitHub fast relay. Active() requires
// Enabled + Token + Repo.
type GitHubRelayConfig struct {
	Enabled    bool
	Token      string
	Repo       string
	Branch     string // default branch to commit to; "" → "main"
	StatePath  string // file used to persist lastSeen across restarts
	MaxBytes   int64
	TTLMinutes int
}

func (g GitHubRelayConfig) Active() bool {
	return g.Enabled && g.Token != "" && g.Repo != ""
}

// Server orchestrates the DNS server and Telegram reader.
type Server struct {
	cfg              Config
	feed             *Feed
	reader           *TelegramReader // nil when --no-telegram
	telegramChannels []string
	xAccounts        []string
}

// New creates a new Server.
func New(cfg Config) (*Server, error) {
	channels, err := loadUsernames(cfg.ChannelsFile)
	if err != nil {
		return nil, fmt.Errorf("load channels: %w", err)
	}
	xAccounts, err := loadUsernames(cfg.XAccountsFile)
	if err != nil {
		return nil, fmt.Errorf("load X accounts: %w", err)
	}

	if len(channels) == 0 && len(xAccounts) == 0 {
		return nil, fmt.Errorf("no channels configured in %s and no X accounts configured in %s", cfg.ChannelsFile, cfg.XAccountsFile)
	}

	log.Printf("[server] loaded %d Telegram channels and %d X accounts", len(channels), len(xAccounts))

	feed := NewFeed(append(append([]string{}, channels...), prefixXAccounts(xAccounts)...))
	return &Server{cfg: cfg, feed: feed, telegramChannels: channels, xAccounts: xAccounts}, nil
}

// Run starts both the DNS server and the Telegram reader.
func (s *Server) Run(ctx context.Context) error {
	queryKey, responseKey, err := protocol.DeriveKeys(s.cfg.Passphrase)
	if err != nil {
		return fmt.Errorf("derive keys: %w", err)
	}

	SetMediaDebugLogs(s.cfg.Debug)

	// Spin up the media cache when at least one relay is enabled. The cache
	// owns the byte pipeline; whether DNS or GitHub serves bytes to clients
	// is controlled by per-relay flags on each MediaMeta.
	anyRelay := s.cfg.DNSMediaEnabled || s.cfg.GitHubRelay.Active()
	if anyRelay {
		ttlMin := s.cfg.DNSMediaCacheTTL
		if ttlMin <= 0 {
			ttlMin = 600
		}
		ttl := time.Duration(ttlMin) * time.Minute
		compName := s.cfg.DNSMediaCompression
		if compName == "" {
			compName = "gzip"
		}
		compression, err := protocol.ParseMediaCompressionName(compName)
		if err != nil {
			return fmt.Errorf("--dns-media-compression: %w", err)
		}
		mediaCache := NewMediaCache(MediaCacheConfig{
			MaxFileBytes:    s.cfg.DNSMediaMaxSize,
			TTL:             ttl,
			Compression:     compression,
			Logf:            logfMedia,
			DNSRelayEnabled: s.cfg.DNSMediaEnabled,
		})
		s.feed.SetMediaCache(mediaCache)
		log.Printf("[server] media: dns=%v max=%d ttl=%s compression=%s",
			s.cfg.DNSMediaEnabled, s.cfg.DNSMediaMaxSize, ttl, compression)
		go s.runMediaSweep(ctx, mediaCache, ttl)

		if s.cfg.GitHubRelay.Active() {
			gh := NewGitHubRelay(s.cfg.GitHubRelay, s.cfg.Domain, s.cfg.Passphrase)
			if gh != nil {
				mediaCache.SetGitHubRelay(gh)
				s.feed.SetGitHubRelay(gh)
				go gh.Run(ctx)
				branch := s.cfg.GitHubRelay.Branch
				if branch == "" {
					branch = "main"
				}
				log.Printf("[server] github relay: repo=%s branch=%s max=%d ttl=%dm",
					gh.Repo(), branch, gh.MaxBytes(), s.cfg.GitHubRelay.TTLMinutes)
			}
		}
	} else {
		log.Println("[server] media disabled (no relays enabled)")
	}

	go startLatestVersionTracker(ctx, s.feed)
	var channelCtl channelRefresher

	// Handle login-only mode
	if s.cfg.Telegram.LoginOnly {
		reader := NewTelegramReader(s.cfg.Telegram, s.telegramChannels, s.feed, 15, 1)
		return reader.Run(ctx)
	}

	// Start Telegram reader in background, or public web fetcher in no-login mode.
	if !s.cfg.NoTelegram {
		msgLimit := s.cfg.MsgLimit
		if msgLimit <= 0 {
			msgLimit = 15
		}
		if len(s.telegramChannels) > 0 {
			reader := NewTelegramReader(s.cfg.Telegram, s.telegramChannels, s.feed, msgLimit, 1)
			reader.SetFetchInterval(s.cfg.FetchInterval)
			s.reader = reader
			channelCtl = reader
			go func() {
				log.Println("[telegram] reader goroutine started")
				if err := reader.Run(ctx); err != nil && ctx.Err() == nil {
					log.Printf("[telegram] reader goroutine STOPPED with error: %v", err)
				} else {
					log.Println("[telegram] reader goroutine exited")
				}
			}()
		} else {
			s.feed.SetTelegramLoggedIn(true)
		}
	} else {
		msgLimit := s.cfg.MsgLimit
		if msgLimit <= 0 {
			msgLimit = 15
		}
		publicReader := NewPublicReader(s.telegramChannels, s.feed, msgLimit, 1)
		publicReader.SetFetchInterval(s.cfg.FetchInterval)
		channelCtl = publicReader
		go func() {
			log.Println("[public] reader goroutine started")
			if err := publicReader.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[public] reader goroutine STOPPED with error: %v", err)
			} else {
				log.Println("[public] reader goroutine exited")
			}
		}()
		log.Println("[server] running without Telegram login; fetching public channels via t.me")
	}

	var xReader *XPublicReader
	if len(s.xAccounts) > 0 {
		msgLimit := s.cfg.MsgLimit
		if msgLimit <= 0 {
			msgLimit = 15
		}
		xReader = NewXPublicReader(s.xAccounts, s.feed, msgLimit, len(s.telegramChannels)+1, s.cfg.XRSSInstances)
		xReader.SetFetchInterval(s.cfg.FetchInterval)
		go func() {
			log.Println("[x] reader goroutine started")
			if err := xReader.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[x] reader goroutine STOPPED with error: %v", err)
			} else {
				log.Println("[x] reader goroutine exited")
			}
		}()
		log.Printf("[server] enabled X source for %d accounts", len(s.xAccounts))
	}

	// Start DNS server (blocking, respects ctx cancellation)
	maxPad := s.cfg.MaxPadding
	if maxPad == 0 {
		maxPad = protocol.DefaultMaxPadding
	}
	dnsServer := NewDNSServer(s.cfg.ListenAddr, s.cfg.Domain, s.feed, queryKey, responseKey, maxPad, s.reader, s.cfg.AllowManage, s.cfg.ChannelsFile, s.xAccounts, s.cfg.Debug)
	if channelCtl != nil {
		dnsServer.SetChannelRefresher(channelCtl)
	}
	if xReader != nil {
		dnsServer.AddRefresher(xReader)
		dnsServer.SetXReader(xReader)
	}
	return dnsServer.ListenAndServe(ctx)
}

func loadUsernames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("[server] close usernames file: %v", err)
		}
	}()

	var users []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.TrimPrefix(line, "@")
		users = append(users, name)
	}
	return users, scanner.Err()
}

func prefixXAccounts(accounts []string) []string {
	out := make([]string, len(accounts))
	for i, a := range accounts {
		out[i] = "x/" + a
	}
	return out
}

// runMediaSweep periodically evicts expired entries from the cache. The
// interval is min(ttl/4, 5min) so we don't waste cycles on long-TTL configs
// while still reclaiming slots in time under steady-state churn.
func (s *Server) runMediaSweep(ctx context.Context, cache *MediaCache, ttl time.Duration) {
	if cache == nil {
		return
	}
	interval := ttl / 4
	if interval <= 0 || interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cache.Sweep()
		}
	}
}
