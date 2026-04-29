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

	"github.com/sartoopjj/thefeed/internal/server"
	"github.com/sartoopjj/thefeed/internal/version"
)

func main() {
	dataDir := flag.String("data-dir", "./data", "Data directory for channels, session, and config")
	listen := flag.String("listen", ":53", "DNS listen address (host:port)")
	domain := flag.String("domain", "", "DNS domain (e.g., t.example.com)")
	key := flag.String("key", "", "Encryption passphrase")
	channelsFile := flag.String("channels", "", "Path to channels file (default: {data-dir}/channels.txt)")
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
	dnsMediaCacheTTLMin := flag.Int("dns-media-cache-ttl", 600, "TTL for DNS-relay cached media, in minutes")
	dnsMediaCompression := flag.String("dns-media-compression", "gzip", "Compression for DNS-relay media bytes: none|gzip|deflate")
	ghEnabled := flag.Bool("github-relay-enabled", false, "Serve media via GitHub (fast relay)")
	ghToken := flag.String("github-relay-token", "", "GitHub PAT with contents:write on the relay repo")
	ghRepo := flag.String("github-relay-repo", "", "GitHub repo for the fast relay, e.g. owner/repo")
	ghBranch := flag.String("github-relay-branch", "main", "Default branch to commit to (e.g. main, master)")
	ghMaxSizeKB := flag.Int("github-relay-max-size", 25*1024, "Per-file cap for the GitHub relay in KB (0 = no cap)")
	ghCacheTTLMin := flag.Int("github-relay-ttl", 600, "TTL for GitHub-relay objects in minutes")
	showVersion := flag.Bool("version", false, "Show version and exit")
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

	// Create data directory
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		log.Fatalf("Create data dir: %v", err)
	}

	// Default paths relative to data directory
	if *channelsFile == "" {
		*channelsFile = filepath.Join(*dataDir, "channels.txt")
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

	if *domain == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "Error: --domain and --key are required")
		flag.Usage()
		os.Exit(1)
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
		ListenAddr:       *listen,
		Domain:           *domain,
		Passphrase:       *key,
		ChannelsFile:     *channelsFile,
		XAccountsFile:    *xAccountsFile,
		XRSSInstances:    *xRSSInstances,
		MaxPadding:       *maxPadding,
		MsgLimit:         *msgLimit,
		NoTelegram:       *noTelegram,
		AllowManage:      *allowManage,
		Debug:            *debug,
		DNSMediaEnabled:     *dnsMediaEnabled,
		DNSMediaMaxSize:     int64(*dnsMediaMaxSizeKB) * 1024,
		DNSMediaCacheTTL:    *dnsMediaCacheTTLMin,
		DNSMediaCompression: *dnsMediaCompression,
		FetchInterval:       time.Duration(*fetchIntervalMin) * time.Minute,
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
