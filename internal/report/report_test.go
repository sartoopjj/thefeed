package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAndAggregate(t *testing.T) {
	const log = `noise line
Jun 10 host thefeed[1]: [dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T09:00:00Z","to":"2026-06-10T10:00:00Z","totalDnsQueries":1000,"totalMetadataQueries":100,"totalMediaQueries":50,"totalChatQueries":40,"totalInvalidQueries":3,"channels":[{"channel":1,"name":"A","queries":400},{"channel":0,"queries":100},{"channel":65525,"queries":20}],"domains":[{"domain":"t.example.com","queries":700}],"chat":{"accounts":12,"messages":5}}
[dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T10:00:00Z","to":"2026-06-10T11:00:00Z","totalDnsQueries":2000,"totalMetadataQueries":200,"channels":[{"channel":1,"name":"A","queries":900}],"chat":{"accounts":15,"messages":9}}
[dns_hourly] {"type":"something_else","totalDnsQueries":99999}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	agg, err := parseLines(path, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if agg.reports != 2 {
		t.Fatalf("reports = %d, want 2 (non-report type ignored)", agg.reports)
	}
	if agg.total != 3000 || agg.metadata != 300 || agg.chat != 40 {
		t.Fatalf("totals: total=%d meta=%d chat=%d", agg.total, agg.metadata, agg.chat)
	}
	if agg.channels["A"] != 1300 {
		t.Fatalf("channel A = %d, want 1300", agg.channels["A"])
	}
	// channel 0 (metadata) and 65525 (chat info) are reserved, not content.
	if _, ok := agg.channels["channel 0"]; ok {
		t.Fatal("reserved channel leaked into content channels")
	}
	if agg.reserved[65525] != 20 {
		t.Fatalf("chat-info reserved = %d, want 20", agg.reserved[65525])
	}
	if agg.domains["t.example.com"] != 700 {
		t.Fatalf("domain total = %d", agg.domains["t.example.com"])
	}
	// channelFetch = 3000 - 300(meta) - 0(version) - 50(media) - 40(chat) -
	// reserved(100+20): chat cells are NOT channel fetches.
	if cf := agg.channelFetch(); cf != 2490 {
		t.Fatalf("channelFetch = %d, want 2490", cf)
	}
	// latest chat stats win.
	if agg.lastChatStats["accounts"] != 15 {
		t.Fatalf("last accounts = %d, want 15", agg.lastChatStats["accounts"])
	}

	out := renderDashboard(agg, 21, 10, false)
	for _, want := range []string{"thefeed server report", "Channel-fetch queries", "Metadata queries", "Top channels", "Chat (messenger)", "Registered accounts", "t.example.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// -chat-db live count (21) overrides the report's 15.
	if !strings.Contains(out, "21") {
		t.Fatal("live account count not rendered")
	}
}

func TestParseTimeRange(t *testing.T) {
	const log = `[dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T09:00:00Z","to":"2026-06-10T10:00:00Z","totalDnsQueries":1000}
[dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T10:00:00Z","to":"2026-06-10T11:00:00Z","totalDnsQueries":2000}
[dns_hourly] {"type":"dns_hourly_report","from":"2026-06-11T09:00:00Z","to":"2026-06-11T10:00:00Z","totalDnsQueries":4000}
`
	path := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	from, _, err := ParseTimeArg("2026-06-10 10:30")
	if err != nil {
		t.Fatal(err)
	}
	to, dateOnly, err := ParseTimeArg("2026-06-10")
	if err != nil || !dateOnly {
		t.Fatalf("bare date: dateOnly=%v err=%v", dateOnly, err)
	}
	to = to.Add(24*time.Hour - time.Second) // whole day, as main.go does

	agg, err := parseLines(path, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if agg.reports != 1 || agg.total != 2000 {
		t.Fatalf("range filter: reports=%d total=%d, want 1/2000", agg.reports, agg.total)
	}
	// Open-ended from.
	agg, err = parseLines(path, time.Time{}, to)
	if err != nil {
		t.Fatal(err)
	}
	if agg.reports != 2 || agg.total != 3000 {
		t.Fatalf("to-only filter: reports=%d total=%d, want 2/3000", agg.reports, agg.total)
	}
	// The header notes the active range; empty result says so.
	out := renderDashboard(agg, -1, 10, false)
	if !strings.Contains(out, "range:") {
		t.Fatal("filtered dashboard missing range header")
	}
	agg, _ = parseLines(path, from.AddDate(1, 0, 0), time.Time{})
	out = renderDashboard(agg, -1, 10, false)
	if !strings.Contains(out, "No hourly reports in the selected range") {
		t.Fatal("empty-range message missing")
	}
	if _, _, err := ParseTimeArg("not a time"); err == nil {
		t.Fatal("bad time accepted")
	}
}

func TestRenderEmpty(t *testing.T) {
	out := renderDashboard(newAggregate(), -1, 10, false)
	if !strings.Contains(out, "No hourly reports") {
		t.Fatal("empty dashboard missing hint")
	}
}

func TestBarAndSparkline(t *testing.T) {
	if got := bar(0, 100, 10); strings.TrimRight(got, " ") != "" {
		t.Fatalf("zero bar not blank: %q", got)
	}
	if got := bar(100, 100, 10); !strings.Contains(got, "█") {
		t.Fatalf("full bar empty: %q", got)
	}
	if s := sparkline([]int64{1, 5, 9}, 10); len([]rune(s)) != 3 {
		t.Fatalf("sparkline rune count = %d, want 3", len([]rune(s)))
	}
}

// The Telegram proxy is reported as two numbers, not one. A session outlives
// the socket it was using, so "connected users" and "open connections" are
// different facts; collapsing them overstates what the relay is carrying.
func TestReportShowsTelegramProxy(t *testing.T) {
	const logs = `[dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T09:00:00Z","to":"2026-06-10T10:00:00Z","totalDnsQueries":500,"mtproto":{"sessions":7,"liveTunnels":3,"maxTunnels":50}}
[dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T10:00:00Z","to":"2026-06-10T11:00:00Z","totalDnsQueries":600,"mtproto":{"sessions":9,"liveTunnels":4,"maxTunnels":50}}`

	agg, err := parseLines(writeTmpLog(t, logs), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// The newest line wins, the way the chat stats do.
	if got := agg.lastMTProtoStats["sessions"]; got != 9 {
		t.Fatalf("sessions = %d, want 9", got)
	}
	if got := agg.lastMTProtoStats["liveTunnels"]; got != 4 {
		t.Fatalf("liveTunnels = %d, want 4", got)
	}

	out := renderDashboard(agg, -1, 10, false)
	for _, want := range []string{"Telegram proxy", "Connected users", "Open connections", "Cap"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q", want)
		}
	}
}

// A server with the proxy switched off must not grow an empty section.
func TestReportOmitsProxyWhenUnused(t *testing.T) {
	const logs = `[dns_hourly] {"type":"dns_hourly_report","from":"2026-06-10T09:00:00Z","to":"2026-06-10T10:00:00Z","totalDnsQueries":500}`
	agg, err := parseLines(writeTmpLog(t, logs), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(renderDashboard(agg, -1, 10, false), "Telegram proxy") {
		t.Fatal("a server without the proxy grew a proxy section")
	}
}

// writeTmpLog drops log text in a file, which is what parseLines reads.
func writeTmpLog(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
