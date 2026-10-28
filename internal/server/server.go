package server

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Config holds server configuration.
type Config struct {
	ListenAddr          string
	Domain              string   // main domain; canonical for relay path derivation
	ExtraDomains        []string // additional sub-domains the server also answers feed queries on
	Passphrase          string
	DataDir             string // server state dir; holds the signing key
	ChannelsFile        string
	PrivateChannelsFile string // optional: invite links for private channels
	XAccountsFile       string
	XRSSInstances       string
	MaxPadding          int
	MsgLimit            int  // max messages per channel (0 = default 15)
	NoTelegram          bool // if true, fetch public channels without Telegram login
	AllowManage         bool // if true, remote channel management and sending via DNS is allowed
	Debug               bool // if true, log every decoded DNS query
	// DNSMediaEnabled toggles the slow DNS-relay path. When false the
	// server still ingests media bytes (so other relays can serve them)
	// but the wire-format DNS flag is unset for clients.
	DNSMediaEnabled     bool
	DNSMediaMaxSize     int64         // per-file cap for the DNS relay (0 = no cap)
	DNSMediaCacheTTL    int           // DNS-relay TTL in minutes
	DNSMediaCompression string        // DNS-relay compression: none|gzip|deflate
	FetchInterval       time.Duration // 0 = default 10m; floor enforced by main
	GitHubRelay         GitHubRelayConfig
	Telegram            TelegramConfig

	// Chat (standalone messenger). Configured when ChatDomains is non-empty;
	// the domains are dedicated chat sub-domains, distinct from the feed
	// domains. Zero limits fall back to protocol defaults.
	ChatDomains        []string
	ChatEnabled        bool // serve chat (default true when domains set); false advertises chat-but-disabled
	ChatSendPerHour    int
	ChatInboxCap       int
	ChatPerPairCap     int
	ChatMaxMsgBytes    int
	ChatTTLHours       int // inbox-message TTL (hours)
	ChatAccountTTLDays int // delete idle accounts after N days (0 = never)
	ChatMaxAccounts    int // cap on stored accounts (0 = unlimited)
	// ChatSyncSeconds controls durability vs throughput for the chat store.
	// 0 = fsync every committed message (max durability, lowest throughput).
	// N>0 = flush to disk every N seconds instead (much higher throughput; a
	// crash can lose up to ~N seconds of just-received messages — acceptable
	// since chat is E2E and senders can resend).
	ChatSyncSeconds int

	// MTProto (DNS tunnel to Telegram). Configured when MTProtoDomains is
	// non-empty; MTProtoEnabled turns the relay itself on, mirroring chat so
	// an operator can advertise "configured but off".
	MTProtoDomains     []string
	MTProtoEnabled     bool
	MTProtoMaxTunnels  int           // 0 = uncapped (fd limit is the real ceiling)
	MTProtoIdleTTL     time.Duration // expire a tunnel with no cells
	MTProtoBufferBytes int           // per-tunnel downstream cap
	// MTProtoDownKBPerMin throttles one tunnel's downstream traffic; 0 is
	// uncapped. The relay carries opaque bytes, so volume is the only limit it
	// can apply to a client that pulls files through it.
	MTProtoDownKBPerMin int
	MTProtoDialTimeout  time.Duration
	MTProtoTestDCs      bool // dial Telegram's test DCs
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
	QuotaKB    int64 // repo size budget for headroom warnings; 0 = default
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
	privateInvites   []string // resolved invite hashes (post-parse)
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
	var privateInvites []string
	if cfg.PrivateChannelsFile != "" {
		privateInvites, err = LoadPrivateInvites(cfg.PrivateChannelsFile)
		if err != nil {
			return nil, fmt.Errorf("load private channels: %w", err)
		}
	}

	if len(channels) == 0 && len(xAccounts) == 0 && len(privateInvites) == 0 {
		return nil, fmt.Errorf("no channels configured in %s, no private invites in %s, and no X accounts in %s",
			cfg.ChannelsFile, cfg.PrivateChannelsFile, cfg.XAccountsFile)
	}

	log.Printf("[server] loaded %d Telegram public channels, %d private invites, %d X accounts",
		len(channels), len(privateInvites), len(xAccounts))

	// Feed slot order: public Telegram, then private Telegram, then X.
	// Private channels use the short hash-derived ID (privateChannelID)
	// so caches stay stable across file reorders without bloating the
	// DNS metadata payload. Display title is set later via
	// SetChannelDisplayName once the channel is resolved.
	allChannelNames := append([]string{}, channels...)
	for _, hash := range privateInvites {
		allChannelNames = append(allChannelNames, privateChannelID(hash))
	}
	allChannelNames = append(allChannelNames, prefixXAccounts(xAccounts)...)
	feed := NewFeed(allChannelNames)

	return &Server{
		cfg:              cfg,
		feed:             feed,
		telegramChannels: channels,
		privateInvites:   privateInvites,
		xAccounts:        xAccounts,
	}, nil
}

// Run starts both the DNS server and the Telegram reader.
func (s *Server) Run(ctx context.Context) error {
	queryKey, responseKey, err := protocol.DeriveKeys(s.cfg.Passphrase)
	if err != nil {
		return fmt.Errorf("derive keys: %w", err)
	}

	// Load (or generate) the server signing key and enable ExtraBlock
	// signing so clients that pin the public key can verify feed content.
	signKey, err := LoadOrCreateServerKey(s.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("server signing key: %w", err)
	}
	s.feed.SetSigningKey(signKey)
	log.Printf("[server] signing public key (sk=): %s", ServerPublicKeyString(signKey))

	// Chat: dedicated sub-domains, x25519 enc key, bbolt account store, and
	// the signed ChatInfo capability payload on the feed metadata path. When
	// domains are configured the metadata bit advertises chat (so clients
	// don't waste a ChatInfo probe on chatless servers); ChatInfo.Enabled
	// reflects whether it is actually on.
	var chat *ChatService
	if len(s.cfg.ChatDomains) > 0 {
		s.feed.SetChatAvailable(true)
		if s.cfg.ChatEnabled {
			ek, err := LoadOrCreateServerEncKey(s.cfg.DataDir)
			if err != nil {
				return fmt.Errorf("chat enc key: %w", err)
			}
			limits := protocol.DefaultChatLimits()
			if s.cfg.ChatSendPerHour > 0 {
				limits.SendPerHour = uint16(s.cfg.ChatSendPerHour)
			}
			if s.cfg.ChatInboxCap > 0 {
				limits.InboxCap = uint16(s.cfg.ChatInboxCap)
			}
			if s.cfg.ChatPerPairCap > 0 {
				limits.PerPairCap = uint16(s.cfg.ChatPerPairCap)
			}
			if s.cfg.ChatMaxMsgBytes > 0 {
				limits.MaxMsgBytes = uint16(s.cfg.ChatMaxMsgBytes)
			}
			if s.cfg.ChatTTLHours > 0 {
				limits.TTLHours = uint16(s.cfg.ChatTTLHours)
			}
			store, err := OpenChatStore(filepath.Join(s.cfg.DataDir, "chat.db"), limits)
			if err != nil {
				return fmt.Errorf("chat store: %w", err)
			}
			defer store.Close()
			store.SetMaxAccounts(s.cfg.ChatMaxAccounts)
			if s.cfg.ChatAccountTTLDays > 0 {
				store.SetAccountTTL(time.Duration(s.cfg.ChatAccountTTLDays) * 24 * time.Hour)
			}
			syncDesc := "every-commit"
			if s.cfg.ChatSyncSeconds > 0 {
				store.EnablePeriodicSync(time.Duration(s.cfg.ChatSyncSeconds) * time.Second)
				go store.RunSync(ctx)
				syncDesc = fmt.Sprintf("%ds", s.cfg.ChatSyncSeconds)
			}
			chat = NewChatService(store, ek, queryKey, limits, s.cfg.ChatDomains)
			s.feed.SetChatInfoPayload(protocol.EncodeChatInfo(chat.Info()))
			go chat.RunSweeper(ctx)
			// Periodic ek rotation: persist the new key, then re-sign/publish
			// ChatInfo so clients adopt it. Limits the blast radius of an ek
			// compromise to one rotation window (forward secrecy at that grain).
			dataDir := s.cfg.DataDir
			chat.SetEkPersist(func(k *ecdh.PrivateKey) error { return SaveServerEncKey(dataDir, k) })
			go chat.RunEkRotation(ctx, func() {
				s.feed.SetChatInfoPayload(protocol.EncodeChatInfo(chat.Info()))
			})
			acctTTL := "never"
			if s.cfg.ChatAccountTTLDays > 0 {
				acctTTL = fmt.Sprintf("%dd", s.cfg.ChatAccountTTLDays)
			}
			log.Printf("[server] chat enabled on %s (inbox=%d per-pair=%d send/h=%d msg=%dB msg-ttl=%dh account-ttl=%s max-accounts=%d sync=%s)",
				strings.Join(s.cfg.ChatDomains, ", "), limits.InboxCap, limits.PerPairCap,
				limits.SendPerHour, limits.MaxMsgBytes, limits.TTLHours, acctTTL, s.cfg.ChatMaxAccounts, syncDesc)
		} else {
			// Configured but turned off: advertise a disabled ChatInfo so
			// clients show "messenger disabled by server" rather than probing.
			s.feed.SetChatInfoPayload(protocol.EncodeChatInfo(protocol.ChatInfo{
				MinVersion: protocol.ChatProtocolVersion,
				MaxVersion: protocol.ChatProtocolVersion,
				Enabled:    false,
				Limits:     protocol.DefaultChatLimits(),
			}))
			log.Printf("[server] chat configured on %s but DISABLED (--chat-enabled=false)", strings.Join(s.cfg.ChatDomains, ", "))
		}
	}

	// MTProto tunnel: its own sub-domains, and the SAME x25519 ek the
	// messenger publishes -- safe because the two derive session keys in
	// different key spaces (MTPSessionKey vs ChatSessionKey). The metadata bit
	// is set whenever domains are configured, so a client can mark the config
	// without probing; the relay is built only when it is actually on.
	var mtproto *MTProtoRelay
	if len(s.cfg.MTProtoDomains) > 0 {
		s.feed.SetMTProtoAvailable(true)
		if s.cfg.MTProtoEnabled {
			ek, err := LoadOrCreateServerEncKey(s.cfg.DataDir)
			if err != nil {
				return fmt.Errorf("mtproto enc key: %w", err)
			}
			mtproto = NewMTProtoRelay(MTProtoConfig{
				Enabled:         true,
				Domains:         s.cfg.MTProtoDomains,
				MaxTunnels:      s.cfg.MTProtoMaxTunnels,
				IdleTTL:         s.cfg.MTProtoIdleTTL,
				BufferBytes:     s.cfg.MTProtoBufferBytes,
				DownBytesPerMin: s.cfg.MTProtoDownKBPerMin * 1024,
				DialTimeout:     s.cfg.MTProtoDialTimeout,
				TestDCs:         s.cfg.MTProtoTestDCs,
				Debug:           s.cfg.Debug,
				QueryKey:        queryKey,
				Ek:              ek,
			})
			if mtproto == nil {
				return fmt.Errorf("mtproto relay: not created")
			}
			s.feed.SetMTProtoInfoPayload(protocol.EncodeMTPInfo(mtproto.Info()))
			go mtproto.RunSweeper(ctx)
			defer mtproto.closeAll()
			dcs := "production"
			if s.cfg.MTProtoTestDCs {
				dcs = "TEST"
			}
			_, maxTunnels := mtproto.Stats()
			log.Printf("[server] mtproto tunnel enabled on %s (dcs=%s max-tunnels=%d idle-ttl=%s buffer=%dB)",
				strings.Join(s.cfg.MTProtoDomains, ", "), dcs, maxTunnels,
				s.cfg.MTProtoIdleTTL, s.cfg.MTProtoBufferBytes)
		} else {
			// Configured but off: publish a disabled payload so clients say
			// "tunnel off on this server" instead of probing it.
			s.feed.SetMTProtoInfoPayload(protocol.EncodeMTPInfo(protocol.MTPInfo{
				MinVersion: protocol.MTProtoVersion,
				MaxVersion: protocol.MTProtoVersion,
				Enabled:    false,
				Domains:    s.cfg.MTProtoDomains,
			}))
			log.Printf("[server] mtproto configured on %s but DISABLED (--mtproto-enabled=false)",
				strings.Join(s.cfg.MTProtoDomains, ", "))
		}
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
		reader := NewTelegramReader(s.cfg.Telegram, s.telegramChannels, s.privateInvites, s.feed, 15, 1)
		return reader.Run(ctx)
	}

	// Start Telegram reader in background, or public web fetcher in no-login mode.
	if !s.cfg.NoTelegram {
		msgLimit := s.cfg.MsgLimit
		if msgLimit <= 0 {
			msgLimit = 15
		}
		if len(s.telegramChannels) > 0 || len(s.privateInvites) > 0 {
			reader := NewTelegramReader(s.cfg.Telegram, s.telegramChannels, s.privateInvites, s.feed, msgLimit, 1)
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
		// X channel numbers start after all Telegram channels (public + private).
		xReader = NewXPublicReader(s.xAccounts, s.feed, msgLimit, len(s.telegramChannels)+len(s.privateInvites)+1, s.cfg.XRSSInstances)
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
	dnsServer.SetExtraDomains(s.cfg.ExtraDomains)
	if err := dnsServer.SetReportFile(filepath.Join(s.cfg.DataDir, "dns_hourly.jsonl")); err != nil {
		log.Printf("[server] report file disabled: %v", err)
	}
	if chat != nil {
		if err := dnsServer.SetChatService(chat); err != nil {
			return fmt.Errorf("chat domains: %w", err)
		}
	}
	if mtproto != nil {
		if err := dnsServer.SetMTProtoRelay(mtproto); err != nil {
			return fmt.Errorf("mtproto domains: %w", err)
		}
	}
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
