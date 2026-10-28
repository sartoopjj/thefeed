package report

import (
	"fmt"
	"sort"
	"strings"
)

// Rendering helpers: plain ANSI + Unicode block bars, no external TUI deps.

const (
	cReset = "\x1b[0m"
	cBold  = "\x1b[1m"
	cDim   = "\x1b[2m"
	cCyan  = "\x1b[36m"
	cGreen = "\x1b[32m"
	cYel   = "\x1b[33m"
)

func comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// humanBytes formats a byte count in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func pct(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}

// bar renders a proportional Unicode block bar of the given cell width.
func bar(value, max int64, width int) string {
	if max <= 0 || value <= 0 {
		return strings.Repeat(" ", width)
	}
	frac := float64(value) / float64(max)
	if frac > 1 {
		frac = 1
	}
	full := int(frac * float64(width))
	rem := frac*float64(width) - float64(full)
	eighths := []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}
	var b strings.Builder
	for i := 0; i < full; i++ {
		b.WriteRune('█')
	}
	if full < width {
		b.WriteRune(eighths[int(rem*8)])
		for i := full + 1; i < width; i++ {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// sparkline renders a series as one line of block characters.
func sparkline(series []int64, width int) string {
	if len(series) == 0 {
		return ""
	}
	if len(series) > width {
		series = series[len(series)-width:]
	}
	var max int64
	for _, v := range series {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return strings.Repeat("▁", len(series))
	}
	levels := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range series {
		idx := int(float64(v) / float64(max) * float64(len(levels)-1))
		if idx < 0 {
			idx = 0
		}
		b.WriteRune(levels[idx])
	}
	return b.String()
}

func renderDashboard(a *aggregate, accounts, top int, live bool) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	line := strings.Repeat("─", 64)

	w("%s%sthefeed server report%s", cBold, cCyan, cReset)
	if live {
		w("   %s(live; Ctrl-C to quit)%s", cDim, cReset)
	}
	w("\n")
	filtered := !a.filterFrom.IsZero() || !a.filterTo.IsZero()
	if filtered {
		from, to := "start", "now"
		if !a.filterFrom.IsZero() {
			from = a.filterFrom.Format("2006-01-02 15:04")
		}
		if !a.filterTo.IsZero() {
			to = a.filterTo.Format("2006-01-02 15:04")
		}
		w("%srange: %s … %s UTC%s\n", cDim, from, to, cReset)
	}
	if a.reports == 0 {
		if filtered {
			w("%sNo hourly reports in the selected range.%s\n", cYel, cReset)
		} else {
			w("%sNo hourly reports yet.%s\n", cYel, cReset)
			w("%sThe server writes one line per hour to {data-dir}/dns_hourly.jsonl.%s\n", cDim, cReset)
		}
		return b.String()
	}
	span := fmt.Sprintf("%s … %s", a.firstTo.Format("2006-01-02 15:04"), a.lastTo.Format("2006-01-02 15:04"))
	w("%s%d hourly reports   %s UTC%s\n%s\n", cDim, a.reports, span, cReset, line)

	// ---- totals ----
	cf := a.channelFetch()
	w("%sTotals%s\n", cBold, cReset)
	w("  DNS queries          : %14s\n", comma(a.total))
	w("  Channel-fetch queries: %14s   (~%s–%s channel loads)\n",
		comma(cf), comma(cf/qpcHigh), comma(cf/qpcLow))
	w("  Metadata queries     : %14s   (%.1f%%)\n", comma(a.metadata), pct(a.metadata, a.total))
	if a.media > 0 {
		w("  Media queries        : %14s\n", comma(a.media))
	}
	w("  Chat queries         : %14s   (%.1f%%)\n", comma(a.chat), pct(a.chat, a.total))
	if a.version > 0 {
		w("  Latest-version       : %14s\n", comma(a.version))
	}
	if a.invalid > 0 {
		w("  Invalid (excluded)   : %14s\n", comma(a.invalid))
	}

	// ---- telegram proxy block ----
	if ms := a.lastMTProtoStats; ms != nil {
		w("%s\n%sTelegram proxy%s\n", line, cBold, cReset)
		// Two different numbers on purpose: a session outlives the socket it
		// was using, so "connected" and "open right now" diverge and the gap
		// is what says whether people are idling or actually pulling data.
		w("  Connected users    : %14s\n", comma(ms["sessions"]))
		w("  Open connections   : %14s\n", comma(ms["liveTunnels"]))
		if cap := ms["maxTunnels"]; cap > 0 {
			w("  Cap                : %14s\n", comma(cap))
		} else {
			w("  Cap                : %14s   %s(uncapped)%s\n", "-", cDim, cReset)
		}
	}

	// ---- chat block ----
	if a.chat > 0 || a.lastChatStats != nil || accounts >= 0 {
		w("%s\n%sChat (messenger)%s\n", line, cBold, cReset)
		acc := int64(-1)
		if accounts >= 0 {
			acc = int64(accounts)
		} else if a.lastChatStats != nil {
			if v, ok := a.lastChatStats["accounts"]; ok {
				acc = v
			}
		}
		if acc >= 0 {
			src := "live"
			if accounts < 0 {
				src = "last report"
			}
			w("  Registered accounts: %14s   %s(%s)%s\n", comma(acc), cDim, src, cReset)
		}
		if cs := a.lastChatStats; cs != nil {
			w("  Messages committed : %14s\n", comma(cs["messages"]))
			if cs["registers"] > 0 {
				w("  Registrations      : %14s\n", comma(cs["registers"]))
			}
		}
		w("  Chat queries / hr  : %14s   %s(avg over window)%s\n", comma(a.chat/int64(maxInt(a.reports, 1))), cDim, cReset)
	}

	// ---- media cache + relay health ----
	if a.lastMedia != nil || a.lastRelay != nil {
		w("%s\n%sMedia cache & relay%s  %s(latest report)%s\n", line, cBold, cReset, cDim, cReset)
		if m := a.lastMedia; m != nil {
			w("  Cached files       : %14s\n", comma(m.Entries))
			w("  RAM held (DNS)     : %14s   %sblocks served over DNS%s\n",
				humanBytes(m.RAMBytes), cDim, cReset)
			w("  Total file bytes   : %14s   %sincl. relay-only, not in RAM%s\n",
				humanBytes(m.Bytes), cDim, cReset)
		}
		if r := a.lastRelay; r != nil && r.Repo != "" {
			w("  Relay repo         : %14s\n", r.Repo)
			if r.RepoSizeKB > 0 {
				w("  Repo size          : %14s   %s%.0f%% of %s budget%s  %s\n",
					humanBytes(r.RepoSizeKB*1024), cDim, r.PercentUsed,
					humanBytes(r.QuotaKB*1024), cReset,
					bar(r.RepoSizeKB, r.QuotaKB, 20))
			}
			w("  Awaiting commit    : %14s   %sblobs uploaded, not yet committed%s\n",
				comma(int64(r.PendingFiles)), cDim, cReset)
			switch {
			case r.TokenDenied:
				w("  %sStatus             : TOKEN LACKS ACCESS — re-grant this repo on the token%s\n", cBold, cReset)
			case r.Quota403:
				w("  %sStatus             : REPO FULL — recreate the repo; uploads are blocked%s\n", cBold, cReset)
			case r.FailStreak > 0:
				w("  Status             : %sfailing (streak %d) %s%s\n", cBold, r.FailStreak, r.LastError, cReset)
			case r.LastError != "":
				// Uploads are failing without arming the backoff (a transient
				// or unclassified error). Never render this as "ok".
				w("  Status             : %sdegraded — %s%s\n", cBold, r.LastError, cReset)
			default:
				w("  Status             : %14s\n", "ok")
			}
		}
	}

	// ---- time series ----
	if len(a.series) > 1 {
		w("%s\n%sTotal queries per report%s  %s\n", line, cBold, cReset, sparkline(a.series, 56))
	}

	// ---- hour of day (only hours that actually saw traffic) ----
	var hmax int64
	for _, q := range a.hours {
		if q > hmax {
			hmax = q
		}
	}
	if hmax > 0 {
		w("%s\n%sQueries per hour of day (UTC)%s\n", line, cBold, cReset)
		for h := 0; h < 24; h++ {
			q := a.hours[h]
			if q == 0 {
				continue
			}
			w("  %02d:00 %12s  %s\n", h, comma(q), bar(q, hmax, 30))
		}
	}

	// ---- top channels ----
	type kv struct {
		name string
		q    int64
	}
	chs := make([]kv, 0, len(a.channels))
	for n, q := range a.channels {
		chs = append(chs, kv{n, q})
	}
	if len(chs) > 0 {
		sort.Slice(chs, func(i, j int) bool {
			if chs[i].q == chs[j].q {
				return chs[i].name < chs[j].name
			}
			return chs[i].q > chs[j].q
		})
		w("%s\n%sTop channels%s  %s(est. loads assume %d–%d queries each)%s\n", line, cBold, cReset, cDim, qpcLow, qpcHigh, cReset)
		chMax := chs[0].q
		shown := chs
		if len(shown) > top {
			shown = shown[:top]
		}
		for _, c := range shown {
			name := c.name
			if len(name) > 22 {
				name = name[:21] + "…"
			}
			w("  %-22s %10s  %s %s~%s–%s%s\n",
				name, comma(c.q), bar(c.q, chMax, 18),
				cDim, comma(c.q/qpcHigh), comma(c.q/qpcLow), cReset)
		}
		if len(chs) > len(shown) {
			w("  %s… and %d more channels%s\n", cDim, len(chs)-len(shown), cReset)
		}
	}

	// ---- per-domain ----
	if len(a.domains) > 0 {
		doms := make([]kv, 0, len(a.domains))
		var dmax int64
		for n, q := range a.domains {
			doms = append(doms, kv{n, q})
			if q > dmax {
				dmax = q
			}
		}
		sort.Slice(doms, func(i, j int) bool { return doms[i].q > doms[j].q })
		w("%s\n%sPer-domain queries%s\n", line, cBold, cReset)
		for _, d := range doms {
			name := d.name
			if len(name) > 30 {
				name = name[:29] + "…"
			}
			w("  %-30s %12s  %s\n", name, comma(d.q), bar(d.q, dmax, 16))
		}
	}

	// ---- reserved channels ----
	if len(a.reserved) > 0 {
		type rk struct {
			ch int
			q  int64
		}
		rs := make([]rk, 0, len(a.reserved))
		for ch, q := range a.reserved {
			rs = append(rs, rk{ch, q})
		}
		// Sort by queries desc, then channel id — a stable order so rows
		// don't jump around between refreshes when counts tie.
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].q == rs[j].q {
				return rs[i].ch < rs[j].ch
			}
			return rs[i].q > rs[j].q
		})
		w("%s\n%sReserved/control channels%s\n", line, cBold, cReset)
		for _, r := range rs {
			label := reservedNames[r.ch]
			if label == "" {
				label = "media blocks"
			}
			w("  %12s  %6.1f%%  %s\n", comma(r.q), pct(r.q, a.total), label)
		}
	}
	w("%s\n", line)
	return b.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
