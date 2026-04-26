package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"

	"github.com/sartoopjj/thefeed/internal/version"
	"github.com/sartoopjj/thefeed/internal/web"
)

func main() {
	dataDir := flag.String("data-dir", "./thefeeddata", "Data directory for config, cache, and sessions")
	port := flag.Int("port", 8080, "Web UI port")
	host := flag.String("host", "127.0.0.1", "Web UI listen address (host), use 0.0.0.0 to expose to LAN")
	password := flag.String("password", "", "Admin password for web UI (empty = no auth)")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "thefeed-client %s\n\nWeb UI for reading thefeed content over DNS.\n\nUsage:\n  thefeed-client [flags]\n\nFlags:\n", version.Version)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("thefeed-client %s (commit: %s, built: %s)\n", version.Version, version.Commit, version.Date)
		os.Exit(0)
	}

	srv, err := web.New(*dataDir, *port, *host, *password)
	if err != nil {
		log.Fatalf("Failed to start: %v", err)
	}

	// Try to open browser automatically

	// Open browser on the chosen host (localhost if default)
	browserHost := *host
	if browserHost == "0.0.0.0" {
		browserHost = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s:%d", browserHost, *port)
	go openBrowser(url)

	if err := srv.Run(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = cmd.Start()
}
