package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
	"github.com/sartoopjj/thefeed/internal/web"
)

func findFreePort(t *testing.T, network string) int {
	t.Helper()
	switch network {
	case "udp":
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find free udp port: %v", err)
		}
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).Port
	default:
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find free tcp port: %v", err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
}

func startDNSServer(t *testing.T, domain, passphrase string, channels []string, messages map[int][]protocol.Message) (string, context.CancelFunc) {
	addr, _, cancel := startDNSServerEx(t, domain, passphrase, false, channels, messages)
	return addr, cancel
}

func startDNSServerWithManage(t *testing.T, domain, passphrase string, allowManage bool, channels []string, messages map[int][]protocol.Message) (string, context.CancelFunc) {
	addr, _, cancel := startDNSServerEx(t, domain, passphrase, allowManage, channels, messages)
	return addr, cancel
}

// startDNSServerEx starts a DNS server and returns the address, live feed (for updates), and cancel.
func startDNSServerEx(t *testing.T, domain, passphrase string, allowManage bool, channels []string, messages map[int][]protocol.Message) (string, *server.Feed, context.CancelFunc) {
	t.Helper()

	qk, rk, err := protocol.DeriveKeys(passphrase)
	if err != nil {
		t.Fatalf("derive keys: %v", err)
	}

	feed := server.NewFeed(channels)
	for ch, msgs := range messages {
		feed.UpdateChannel(ch, msgs)
	}

	port := findFreePort(t, "udp")
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	channelsFile := ""
	if allowManage {
		f, err := os.CreateTemp(t.TempDir(), "channels-*.txt")
		if err != nil {
			t.Fatalf("create temp channels file: %v", err)
		}
		for _, ch := range channels {
			fmt.Fprintf(f, "@%s\n", ch)
		}
		f.Close()
		channelsFile = f.Name()
	}

	dnsServer := server.NewDNSServer(addr, domain, feed, qk, rk, protocol.DefaultMaxPadding, nil, allowManage, channelsFile, nil, false)

	ctx, cancel := context.WithCancel(context.Background())

	ready := make(chan struct{})
	go func() {
		close(ready)
		if err := dnsServer.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("dns server error: %v", err)
		}
	}()
	<-ready
	time.Sleep(100 * time.Millisecond)

	return addr, feed, cancel
}

func startWebServer(t *testing.T) (string, *web.Server) {
	t.Helper()
	dataDir := t.TempDir()
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "127.0.0.1", "")
	if err != nil {
		t.Fatalf("create web server: %v", err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)
	return fmt.Sprintf("http://127.0.0.1:%d", port), srv
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func getJSON(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}
