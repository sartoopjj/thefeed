package e2e_test

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
	"github.com/sartoopjj/thefeed/internal/web"
)

// startChatDNSServer runs a real feed + messenger server with its own limits.
// Returns the resolver address and the sk= key clients must pin.
func startChatDNSServer(t *testing.T, domain, chatDomain, passphrase string, limits protocol.ChatLimits) (string, string) {
	t.Helper()
	qk, rk, err := protocol.DeriveKeys(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	_, signKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	store, err := server.OpenChatStore(filepath.Join(t.TempDir(), "chat.db"), limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	feed := server.NewFeed(nil)
	feed.SetSigningKey(signKey)
	feed.SetChatAvailable(true)
	chat := server.NewChatService(store, ek, qk, limits, []string{chatDomain})
	feed.SetChatInfoPayload(protocol.EncodeChatInfo(chat.Info()))

	addr := fmt.Sprintf("127.0.0.1:%d", findFreePort(t, "udp"))
	dns := server.NewDNSServer(addr, domain, feed, qk, rk, protocol.DefaultMaxPadding, nil, false, "", nil, false)
	if err := dns.SetChatService(chat); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = dns.ListenAndServe(ctx) }()
	time.Sleep(100 * time.Millisecond)
	return addr, server.ServerPublicKeyString(signKey)
}

// startRoutingResolver stands in for a recursive resolver: it forwards each
// query to the server authoritative for its name, as the real network does.
func startRoutingResolver(t *testing.T, routes map[string]string) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", findFreePort(t, "udp"))
	srv := &dns.Server{Addr: addr, Net: "udp", Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		if len(m.Question) == 1 {
			name := strings.ToLower(m.Question[0].Name)
			for suffix, upstream := range routes {
				if strings.HasSuffix(name, suffix+".") {
					if r, err := dns.Exchange(m, upstream); err == nil {
						_ = w.WriteMsg(r)
					}
					return
				}
			}
		}
		r := new(dns.Msg)
		r.SetRcode(m, dns.RcodeNameError)
		_ = w.WriteMsg(r)
	})}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	time.Sleep(100 * time.Millisecond)
	return addr
}

type limitsRow struct {
	Key       string         `json:"key"`
	Domain    string         `json:"domain"`
	Available bool           `json:"available"`
	Checking  bool           `json:"checking"`
	Limits    map[string]int `json:"limits"`
}

func getChatJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v: %s", url, err, raw)
	}
}

// Two servers with different limits: every row the UI reads must carry its
// own server's numbers, every time, however the probes interleave. One shared
// value made the number flip between the two.
func TestChatLimitsArePerServer(t *testing.T) {
	const pass = "e2e-chat-limits"
	ownLim := protocol.DefaultChatLimits()
	ownLim.SendPerHour, ownLim.MaxMsgBytes, ownLim.InboxCap = 300, 900, 200
	otherLim := protocol.DefaultChatLimits()
	otherLim.SendPerHour, otherLim.MaxMsgBytes, otherLim.InboxCap = 30, 500, 50

	ownAddr, ownSK := startChatDNSServer(t, "own.e2e.test", "c.own.e2e.test", pass, ownLim)
	otherAddr, otherSK := startChatDNSServer(t, "other.e2e.test", "c.other.e2e.test", pass, otherLim)
	resolver := startRoutingResolver(t, map[string]string{"own.e2e.test": ownAddr, "other.e2e.test": otherAddr})

	dataDir := t.TempDir()
	pl := web.ProfileList{
		Active: "own",
		Profiles: []web.Profile{
			{ID: "own", Nickname: "own", Config: web.Config{Domain: "own.e2e.test", Key: pass, ServerKey: ownSK, Resolvers: []string{resolver}}},
			{ID: "other", Nickname: "other", Config: web.Config{Domain: "other.e2e.test", Key: pass, ServerKey: otherSK, Resolvers: []string{resolver}}},
		},
		ResolverBank: []string{resolver},
	}
	raw, _ := json.Marshal(pl)
	if err := os.WriteFile(filepath.Join(dataDir, "profiles.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	port := findFreePort(t, "tcp")
	srv, err := web.New(dataDir, port, "127.0.0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Run()
	time.Sleep(200 * time.Millisecond)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	if r := postJSON(t, base+"/api/chat/enable", `{"action":"create"}`); r.StatusCode != 200 {
		t.Fatalf("create identity: %d", r.StatusCode)
	}

	want := func(row limitsRow) (int, int, bool) {
		switch {
		case strings.Contains(row.Domain, "own") || strings.Contains(row.Key, "own"):
			return 300, 900, true
		case strings.Contains(row.Domain, "other") || strings.Contains(row.Key, "other"):
			return 30, 500, true
		}
		return 0, 0, false
	}
	check := func(where string, row limitsRow) {
		t.Helper()
		if row.Limits == nil {
			return
		}
		sph, mmb, ok := want(row)
		if !ok {
			t.Fatalf("%s: row for an unknown server: %+v", where, row)
		}
		if row.Limits["sendPerHour"] != sph || row.Limits["maxMsgBytes"] != mmb {
			t.Fatalf("%s: %s shows sendPerHour=%d maxMsgBytes=%d, its server allows %d / %d",
				where, row.Domain, row.Limits["sendPerHour"], row.Limits["maxMsgBytes"], sph, mmb)
		}
	}

	// Turning a server on registers through its ChatInfo, which already holds
	// its limits: the very next list, served from the reachability cache
	// without a probe, must carry them.
	for _, k := range []string{"own.e2e.test", "other.e2e.test"} {
		r := postJSON(t, base+"/api/chat/enable", `{"action":"server","server":"`+k+`","on":true}`)
		var en map[string]any
		_ = json.NewDecoder(r.Body).Decode(&en)
		r.Body.Close()
		if en["available"] != true {
			t.Fatalf("enable %s: %v", k, en)
		}
	}
	{
		var av struct {
			Servers []limitsRow `json:"servers"`
		}
		getChatJSON(t, base+"/api/chat/availability", &av)
		for _, row := range av.Servers {
			if row.Limits == nil {
				t.Fatalf("%s is reachable but its row has no limits: the quota ring has nothing to show", row.Key)
			}
			check("availability after enable", row)
		}
	}

	// Wait until both servers have answered with their limits.
	var keys []string
	deadline := time.Now().Add(60 * time.Second)
	for {
		var av struct {
			Servers []limitsRow `json:"servers"`
		}
		getChatJSON(t, base+"/api/chat/availability?retry=1", &av)
		got := 0
		keys = keys[:0]
		for _, row := range av.Servers {
			check("availability", row)
			keys = append(keys, row.Key)
			if row.Limits != nil {
				got++
			}
		}
		if len(av.Servers) == 2 && got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("servers never reported their limits: %+v", av.Servers)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Now hammer every path the UI reads limits from, alternating servers:
	// fresh probes, the per-server probe, and the cached aggregate.
	for i := 0; i < 20; i++ {
		for _, k := range keys {
			var pr struct {
				Server limitsRow      `json:"server"`
				Limits map[string]int `json:"limits"`
			}
			q := ""
			if i%2 == 0 {
				q = "&retry=1"
			}
			getChatJSON(t, base+"/api/chat/probe?server="+k+q, &pr)
			check(fmt.Sprintf("probe %s #%d", k, i), pr.Server)
			if pr.Limits != nil {
				check(fmt.Sprintf("probe %s #%d (top-level)", k, i), limitsRow{Key: pr.Server.Key, Domain: pr.Server.Domain, Limits: pr.Limits})
			}
		}
		var av struct {
			Servers []limitsRow `json:"servers"`
		}
		getChatJSON(t, base+"/api/chat/availability", &av)
		for _, row := range av.Servers {
			check(fmt.Sprintf("availability #%d", i), row)
		}
	}
}
