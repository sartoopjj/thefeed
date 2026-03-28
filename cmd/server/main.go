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
	apiID := flag.String("api-id", "", "Telegram API ID")
	apiHash := flag.String("api-hash", "", "Telegram API Hash")
	phone := flag.String("phone", "", "Telegram phone number")
	loginOnly := flag.Bool("login-only", false, "Authenticate to Telegram, save session, and exit")
	sessionPath := flag.String("session", "", "Path to Telegram session file (default: {data-dir}/session.json)")
	maxPadding := flag.Int("padding", 32, "Max random padding bytes in DNS responses (anti-DPI, 0=disabled)")
	msgLimit := flag.Int("msg-limit", 15, "Maximum messages to fetch per Telegram channel")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("thefeed-server %s (commit: %s, built: %s)\n", version.Version, version.Commit, version.Date)
		os.Exit(0)
	}

	// Create data directory
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		log.Fatalf("Create data dir: %v", err)
	}

	// Default paths relative to data directory
	if *channelsFile == "" {
		*channelsFile = filepath.Join(*dataDir, "channels.txt")
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
	if *apiID == "" {
		*apiID = os.Getenv("TELEGRAM_API_ID")
	}
	if *apiHash == "" {
		*apiHash = os.Getenv("TELEGRAM_API_HASH")
	}
	if *phone == "" {
		*phone = os.Getenv("TELEGRAM_PHONE")
	}

	if *domain == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "Error: --domain and --key are required")
		flag.Usage()
		os.Exit(1)
	}
	if *apiID == "" || *apiHash == "" || *phone == "" {
		fmt.Fprintln(os.Stderr, "Error: --api-id, --api-hash, and --phone are required")
		flag.Usage()
		os.Exit(1)
	}

	id, err := strconv.Atoi(*apiID)
	if err != nil {
		log.Fatalf("Invalid API ID: %v", err)
	}

	// Interactive 2FA password prompt — only when --login-only or no existing session
	password := os.Getenv("TELEGRAM_PASSWORD")
	if password == "" {
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

	cfg := server.Config{
		ListenAddr:   *listen,
		Domain:       *domain,
		Passphrase:   *key,
		ChannelsFile: *channelsFile,
		MaxPadding:   *maxPadding,
		MsgLimit:     *msgLimit,
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
