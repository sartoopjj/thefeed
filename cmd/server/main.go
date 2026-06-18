package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/sartoopjj/thefeed/internal/report"
	"github.com/sartoopjj/thefeed/internal/server"
	"github.com/sartoopjj/thefeed/internal/version"
)

func main() {
	dataDir := flag.String("data-dir", "./data", "Data directory for channels, session, and config")
	listen := flag.String("listen", ":53", "DNS listen address (host:port)")
	domain := flag.String("domain", "", "Main DNS domain (e.g., t.example.com); canonical for relay paths")
	extraDomains := flag.String("extra-domains", "", "Comma-separated extra sub-domains to also answer feed queries on (clients spread queries across all of them)")
	key := flag.String("key", "", "Encryption passphrase")
	channelsFile := flag.String("channels", "", "Path to channels file (default: {data-dir}/channels.txt)")
	privateChannelsFile := flag.String("private-channels", "", "Path to private-channel invite-links file (default: {data-dir}/private_channels.txt; one invite link per line; requires Telegram login)")
	xAccountsFile := flag.String("x-accounts", "", "Path to X accounts file (default: {data-dir}/x_accounts.txt)")
	xRSSInstances := flag.String("x-rss-instances", "", "Comma-separated X RSS base URLs (e.g., https://nitter.net,http://nitter.net)")
	apiID := flag.String("api-id", "", "Telegram API ID (optional if --no-telegram)")
	apiHash := flag.String("api-hash", "", "Telegram API Hash (optional if --no-telegram)")
	phone := flag.String("phone", "", "Telegram phone number (optional if --no-telegram)")
	loginOnly := flag.Bool("login-only", false, "Authenticate to Telegram, save session, and exit")
	noTelegram := flag.Bool("no-telegram", false, "Fetch public channels without Telegram login")
	sessionPath := flag.String("session", "", "Path to Telegram session file (default: {data-dir}/session.json)")
	maxPadding := flag.Int("padding", 32, "Max random padding bytes in DNS responses (anti-DPI, 0=disabled)")
	msgLimit := flag.Int("msg-limit", 15, "Maximum messages to fetch per Telegram channel")
	fetchIntervalMin := flag.Int("fetch-interval", 10, "Fetch cycle interval in minutes (min 3, default 10)")
	allowManage := flag.Bool("allow-manage", false, "Allow remote channel management and sending via DNS")
	debug := flag.Bool("debug", false, "Log every decoded DNS query")
	dnsMediaEnabled := flag.Bool("dns-media-enabled", false, "Serve media via DNS (slow relay)")
	dnsMediaMaxSizeKB := flag.Int("dns-media-max-size", 100, "Per-file cap for the DNS relay in KB (0 = no cap)")
	dnsAudioMaxSizeKB := flag.Int("dns-audio-max-size", 0, "Per-file cap for audio/voice files in the DNS relay in KB (0 = fallback to dns-media-max-size)")
	dnsMediaCacheTTLMin := flag.Int("dns-media-cache-ttl", 600, "TTL for DNS-relay cached media, in minutes")
	dnsMediaCompression := flag.String("dns-media-compression", "gzip", "Compression for DNS-relay media bytes: none|gzip|deflate")
	chatDomains := flag.String("chat-domains", "", "Comma-separated dedicated sub-domains for the chat messenger (empty = chat off)")
	chatEnabled := flag.Bool("chat-enabled", true, "Serve chat when --chat-domains is set; false advertises chat-but-disabled to clients")
	chatSendPerHour := flag.Int("chat-send-per-hour", 30, "Chat: max messages a user may send per hour")
	chatInboxCap := flag.Int("chat-inbox-cap", 50, "Chat: max messages stored per inbox")
	chatPerPairCap := flag.Int("chat-per-pair-cap", 10, "Chat: max stored messages in one inbox from a single sender")
	chatMaxMsgBytes := flag.Int("chat-max-msg-bytes", 500, "Chat: max message text size in bytes")
	chatTTLHours := flag.Int("chat-ttl-hours", 72, "Chat: hours before an undelivered message expires")
	chatAccountTTLDays := flag.Int("chat-account-ttl-days", 0, "Chat: delete idle accounts after N days (0 = never; keeps reports accurate)")
	chatMaxAccounts := flag.Int("chat-max-accounts", 0, "Chat: cap on stored accounts (0 = unlimited)")
	chatSyncSeconds := flag.Int("chat-sync-seconds", 1, "Chat: flush the message store to disk every N seconds (0 = fsync every message: max durability, lower throughput; N>0: higher throughput, a crash may lose ≤N seconds of received messages)")
	ghEnabled := flag.Bool("github-relay-enabled", false, "Serve media via GitHub (fast relay)")
	ghToken := flag.String("github-relay-token", "", "GitHub PAT with contents:write on the relay repo")
	ghRepo := flag.String("github-relay-repo", "", "GitHub repo for the fast relay, e.g. owner/repo")
	ghBranch := flag.String("github-relay-branch", "main", "Default branch to commit to (e.g. main, master)")
	ghMaxSizeKB := flag.Int("github-relay-max-size", 15*1024, "Per-file cap for the GitHub relay in KB (0 = no cap)")
	ghCacheTTLMin := flag.Int("github-relay-ttl", 600, "TTL for GitHub-relay objects in minutes")
	showVersion := flag.Bool("version", false, "Show version and exit")
	printConfig := flag.Bool("print-config", false, "Print the client config URI (server public key + bootstrap resolvers) and exit")
	printPubKey := flag.Bool("print-pubkey", false, "Print the server signing public key (sk=) and exit")
	showReport := flag.Bool("report", false, "Show the hourly stats dashboard (reads {data-dir}/dns_hourly.jsonl) and exit")
	reportRefresh := flag.Duration("report-refresh", 0, "With --report, redraw every interval (e.g. 5s; 0 = once)")
	reportTop := flag.Int("report-top", 15, "With --report, number of top channels to show")
	reportFrom := flag.String("report-from", "", "With --report, include only reports from this UTC time (e.g. 2026-06-10 or \"2026-06-10 18:00\")")
	reportTo := flag.String("report-to", "", "With --report, include only reports up to this UTC time (a bare date covers that whole day)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "thefeed-server %s\n\nServes Telegram/X feed content over encrypted DNS for censorship-resistant access.\n\nUsage:\n  thefeed-server [flags]\n\nFlags:\n", version.Version)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("thefeed-server %s (commit: %s, built: %s)\n", version.Version, version.Commit, version.Date)
		os.Exit(0)
	}

	// Catch the common --bool true mistake: Go's flag package stops parsing at
	// the first positional, so any flags after it are silently dropped. Bool
	// flags must use --foo or --foo=true (no space).
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected positional argument(s): %v\n", flag.Args())
		fmt.Fprintln(os.Stderr, "Hint: bool flags must be written as --flag or --flag=true (NOT --flag true).")
		os.Exit(1)
	}

	// Stats dashboard: read the server's own report file from the data dir
	// and render it in the terminal. Read-only, serves nothing, needs no
	// domain/key — so run it before any of that validation.
	if *showReport {
		chatDB := filepath.Join(*dataDir, "chat.db")
		if _, err := os.Stat(chatDB); err != nil {
			chatDB = "" // no chat.db → skip the live account count
		}
		var fromT, toT time.Time
		if *reportFrom != "" {
			t, _, err := report.ParseTimeArg(*reportFrom)
			if err != nil {
				fmt.Fprintln(os.Stderr, "report: --report-from:", err)
				os.Exit(1)
			}
			fromT = t
		}
		if *reportTo != "" {
			t, dateOnly, err := report.ParseTimeArg(*reportTo)
			if err != nil {
				fmt.Fprintln(os.Stderr, "report: --report-to:", err)
				os.Exit(1)
			}
			if dateOnly {
				t = t.Add(24*time.Hour - time.Second)
			}
			toT = t
		}
		if err := report.Run(report.Options{
			Path:    filepath.Join(*dataDir, "dns_hourly.jsonl"),
			ChatDB:  chatDB,
			Top:     *reportTop,
			Refresh: *reportRefresh,
			From:    fromT,
			To:      toT,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "report:", err)
			os.Exit(1)
		}
		return
	}

	// Create data directory
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		log.Fatalf("Create data dir: %v", err)
	}

	// Default paths relative to data directory
	if *channelsFile == "" {
		*channelsFile = filepath.Join(*dataDir, "channels.txt")
	}
	if *privateChannelsFile == "" {
		*privateChannelsFile = filepath.Join(*dataDir, "private_channels.txt")
	}
	if *xAccountsFile == "" {
		*xAccountsFile = filepath.Join(*dataDir, "x_accounts.txt")
	}
	if *sessionPath == "" {
		*sessionPath = filepath.Join(*dataDir, "session.json")
	}

	if *domain == "" {
		*domain = os.Getenv("THEFEED_DOMAIN")
	}
	if *extraDomains == "" {
		*extraDomains = os.Getenv("THEFEED_EXTRA_DOMAINS")
	}
	var extraDomainList []string
	for _, d := range strings.Split(*extraDomains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			extraDomainList = append(extraDomainList, d)
		}
	}
	if *key == "" {
		*key = os.Getenv("THEFEED_KEY")
	}
	if !*allowManage && os.Getenv("THEFEED_ALLOW_MANAGE") == "1" {
		*allowManage = true
	}
	// THEFEED_ALLOW_MANAGE=0 explicitly disables, even if flag was set
	if os.Getenv("THEFEED_ALLOW_MANAGE") == "0" {
		*allowManage = false
	}
	if env := os.Getenv("THEFEED_DEBUG"); env == "1" {
		*debug = true
	} else if env == "0" {
		*debug = false
	}
	if *fetchIntervalMin == 10 {
		if v := os.Getenv("THEFEED_FETCH_INTERVAL"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				*fetchIntervalMin = n
			}
		}
	}
	if *fetchIntervalMin < 3 {
		fmt.Fprintf(os.Stderr, "Error: --fetch-interval must be at least 3 minutes (got %d)\n", *fetchIntervalMin)
		os.Exit(1)
	}
	if *msgLimit == 15 {
		if v := os.Getenv("THEFEED_MSG_LIMIT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				*msgLimit = n
			}
		}
	}
	if *maxPadding == 32 {
		if v := os.Getenv("THEFEED_PADDING"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				*maxPadding = n
			}
		}
	}
	if *apiID == "" {
		*apiID = os.Getenv("TELEGRAM_API_ID")
	}
	if *apiHash == "" {
		*apiHash = os.Getenv("TELEGRAM_API_HASH")
	}
	if *phone == "" {
		*phone = os.Getenv("TELEGRAM_PHONE")
	}
	if *xRSSInstances == "" {
		*xRSSInstances = os.Getenv("THEFEED_X_RSS_INSTANCES")
	}
	if *chatDomains == "" {
		*chatDomains = os.Getenv("THEFEED_CHAT_DOMAINS")
	}
	var chatDomainList []string
	for _, d := range strings.Split(*chatDomains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			chatDomainList = append(chatDomainList, d)
		}
	}
	chatIntEnv := func(flagVal *int, def int, env string) {
		if *flagVal == def {
			if v := os.Getenv(env); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					*flagVal = n
				}
			}
		}
	}
	chatIntEnv(chatSendPerHour, 30, "THEFEED_CHAT_SEND_PER_HOUR")
	chatIntEnv(chatInboxCap, 50, "THEFEED_CHAT_INBOX_CAP")
	chatIntEnv(chatPerPairCap, 10, "THEFEED_CHAT_PER_PAIR_CAP")
	chatIntEnv(chatMaxMsgBytes, 500, "THEFEED_CHAT_MAX_MSG_BYTES")
	chatIntEnv(chatTTLHours, 72, "THEFEED_CHAT_TTL_HOURS")
	chatIntEnv(chatAccountTTLDays, 0, "THEFEED_CHAT_ACCOUNT_TTL_DAYS")
	chatIntEnv(chatMaxAccounts, 0, "THEFEED_CHAT_MAX_ACCOUNTS")
	chatIntEnv(chatSyncSeconds, 1, "THEFEED_CHAT_SYNC_SECONDS")
	// THEFEED_CHAT_ENABLED=0 turns chat off even with domains configured.
	if v := os.Getenv("THEFEED_CHAT_ENABLED"); v == "0" {
		*chatEnabled = false
	} else if v == "1" {
		*chatEnabled = true
	}
	// Clamp chat limits that ride uint16 on the wire.
	clamp16 := func(v *int) {
		if *v > 65535 {
			*v = 65535
		}
	}
	clamp16(chatSendPerHour)
	clamp16(chatInboxCap)
	clamp16(chatPerPairCap)
	clamp16(chatMaxMsgBytes)
	clamp16(chatTTLHours)

	if *domain == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "Error: --domain and --key are required")
		flag.Usage()
		os.Exit(1)
	}

	// Admin helpers: print the pinned public key or the full client config
	// URI (loading/generating the signing key if needed), then exit. No
	// Telegram credentials required.
	if *printConfig || *printPubKey {
		signKey, err := server.LoadOrCreateServerKey(*dataDir)
		if err != nil {
			log.Fatalf("server signing key: %v", err)
		}
		if *printPubKey {
			fmt.Println(server.ServerPublicKeyString(signKey))
		} else {
			fmt.Println(server.ConfigURI(*domain, extraDomainList, *key, signKey))
		}
		os.Exit(0)
	}

	// Telegram credentials are required unless --no-telegram
	needTelegram := !*noTelegram
	if needTelegram {
		if *apiID == "" || *apiHash == "" || *phone == "" {
			fmt.Fprintln(os.Stderr, "Error: --api-id, --api-hash, and --phone are required (use --no-telegram to skip)")
			flag.Usage()
			os.Exit(1)
		}
	}

	var id int
	if *apiID != "" {
		var err error
		id, err = strconv.Atoi(*apiID)
		if err != nil {
			log.Fatalf("Invalid API ID: %v", err)
		}
	}

	// Interactive 2FA password prompt — only when Telegram is enabled
	password := os.Getenv("TELEGRAM_PASSWORD")
	if password == "" && needTelegram {
		hasSession := false
		if info, statErr := os.Stat(*sessionPath); statErr == nil && info.Size() > 0 {
			hasSession = true
		}
		if *loginOnly || !hasSession {
			fmt.Print("Telegram 2FA password (press Enter if none): ")
			pwBytes, err := term.ReadPassword(int(syscall.Stdin))
			fmt.Println()
			if err == nil && len(pwBytes) > 0 {
				password = string(pwBytes)
			}
		}
	}

	if env := os.Getenv("THEFEED_DNS_MEDIA_ENABLED"); env == "0" {
		*dnsMediaEnabled = false
	} else if env == "1" {
		*dnsMediaEnabled = true
	}
	if env := os.Getenv("THEFEED_DNS_MEDIA_MAX_SIZE_KB"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			*dnsMediaMaxSizeKB = n
		}
	}
	if env := os.Getenv("THEFEED_DNS_AUDIO_MAX_SIZE_KB"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			*dnsAudioMaxSizeKB = n
		}
	}
	if env := os.Getenv("THEFEED_DNS_MEDIA_CACHE_TTL_MIN"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			*dnsMediaCacheTTLMin = n
		}
	}
	if env := os.Getenv("THEFEED_DNS_MEDIA_COMPRESSION"); env != "" {
		*dnsMediaCompression = env
	}
	if !*ghEnabled && os.Getenv("THEFEED_GITHUB_RELAY_ENABLED") == "1" {
		*ghEnabled = true
	}
	if *ghToken == "" {
		*ghToken = os.Getenv("THEFEED_GITHUB_RELAY_TOKEN")
	}
	if *ghRepo == "" {
		*ghRepo = os.Getenv("THEFEED_GITHUB_RELAY_REPO")
	}
	if *ghBranch == "main" {
		if v := os.Getenv("THEFEED_GITHUB_RELAY_BRANCH"); v != "" {
			*ghBranch = v
		}
	}
	if env := os.Getenv("THEFEED_GITHUB_RELAY_MAX_SIZE_KB"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			*ghMaxSizeKB = n
		}
	}
	if env := os.Getenv("THEFEED_GITHUB_RELAY_TTL_MIN"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			*ghCacheTTLMin = n
		}
	}

	cfg := server.Config{
		ListenAddr:          *listen,
		Domain:              *domain,
		ExtraDomains:        extraDomainList,
		Passphrase:          *key,
		DataDir:             *dataDir,
		ChannelsFile:        *channelsFile,
		PrivateChannelsFile: *privateChannelsFile,
		XAccountsFile:       *xAccountsFile,
		XRSSInstances:       *xRSSInstances,
		MaxPadding:          *maxPadding,
		MsgLimit:            *msgLimit,
		NoTelegram:          *noTelegram,
		AllowManage:         *allowManage,
		Debug:               *debug,
		DNSMediaEnabled:     *dnsMediaEnabled,
		DNSMediaMaxSize:     int64(*dnsMediaMaxSizeKB) * 1024,
		DNSAudioMaxSize:     int64(*dnsAudioMaxSizeKB) * 1024,
		DNSMediaCacheTTL:    *dnsMediaCacheTTLMin,
		DNSMediaCompression: *dnsMediaCompression,
		FetchInterval:       time.Duration(*fetchIntervalMin) * time.Minute,
		ChatDomains:         chatDomainList,
		ChatEnabled:         *chatEnabled,
		ChatSendPerHour:     *chatSendPerHour,
		ChatInboxCap:        *chatInboxCap,
		ChatPerPairCap:      *chatPerPairCap,
		ChatMaxMsgBytes:     *chatMaxMsgBytes,
		ChatTTLHours:        *chatTTLHours,
		ChatAccountTTLDays:  *chatAccountTTLDays,
		ChatMaxAccounts:     *chatMaxAccounts,
		ChatSyncSeconds:     *chatSyncSeconds,
		GitHubRelay: server.GitHubRelayConfig{
			Enabled:    *ghEnabled,
			Token:      *ghToken,
			Repo:       *ghRepo,
			Branch:     *ghBranch,
			StatePath:  filepath.Join(*dataDir, "gh_relay_state.json"),
			MaxBytes:   int64(*ghMaxSizeKB) * 1024,
			TTLMinutes: *ghCacheTTLMin,
		},
		Telegram: server.TelegramConfig{
			APIID:       id,
			APIHash:     *apiHash,
			Phone:       *phone,
			Password:    password,
			SessionPath: *sessionPath,
			LoginOnly:   *loginOnly,
			CodePrompt: func(ctx context.Context) (string, error) {
				fmt.Print("Enter Telegram auth code: ")
				reader := bufio.NewReader(os.Stdin)
				code, err := reader.ReadString('\n')
				if err != nil {
					return "", err
				}
				return strings.TrimSpace(code), nil
			},
		},
	}

	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("Create server: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Printf("Starting thefeed server %s on %s for domain %s", version.Version, cfg.ListenAddr, cfg.Domain)
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("Server error: %v", err)
	}
	log.Println("Server stopped")
}
