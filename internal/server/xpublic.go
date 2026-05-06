package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// XPublicReader fetches public posts for X usernames via Nitter RSS endpoints.
type XPublicReader struct {
	accounts []string
	feed     *Feed
	msgLimit int
	baseCh   int

	client    *http.Client
	instances []string

	mu            sync.RWMutex
	cache         map[string]cachedMessages
	cacheTTL      time.Duration
	fetchInterval time.Duration

	refreshCh chan struct{}
}

// SetFetchInterval overrides the default 10m fetch cadence.
func (xr *XPublicReader) SetFetchInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	xr.mu.Lock()
	xr.fetchInterval = d
	xr.cacheTTL = d
	xr.mu.Unlock()
}

const maxXRSSBodyBytes int64 = 2 << 20 // 2 MiB

var xSnowflakeRe = regexp.MustCompile(`\d{8,}`)
var xHandleRe = regexp.MustCompile(`@[A-Za-z0-9_]{1,15}`)
var xRepostLeadRe = regexp.MustCompile(`(?i)^RT\s+(@[A-Za-z0-9_]{1,15})\s*:\s*`)
var xRepostByRe = regexp.MustCompile(`(?i)^RT\s+by\s+.+\((@[A-Za-z0-9_]{1,15})\)\s*:\s*`)

func NewXPublicReader(accounts []string, feed *Feed, msgLimit int, baseCh int, instancesCSV string) *XPublicReader {
	cleaned := make([]string, 0, len(accounts))
	for _, a := range accounts {
		a = strings.TrimSpace(strings.TrimPrefix(a, "@"))
		a = strings.TrimPrefix(a, "x/")
		if a != "" {
			cleaned = append(cleaned, a)
		}
	}
	if msgLimit <= 0 {
		msgLimit = 15
	}
	if baseCh <= 0 {
		baseCh = 1
	}
	instances := normalizeXRSSInstances(instancesCSV)

	return &XPublicReader{
		accounts: cleaned,
		feed:     feed,
		msgLimit: msgLimit,
		baseCh:   baseCh,
		client: &http.Client{
			Timeout: 25 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		instances:     instances,
		cache:         make(map[string]cachedMessages),
		cacheTTL:      10 * time.Minute,
		fetchInterval: 10 * time.Minute,
		refreshCh:     make(chan struct{}, 1),
	}
}

func normalizeXRSSInstances(instancesCSV string) []string {
	defaults := []string{"https://nitter.net", "http://nitter.net"}
	if strings.TrimSpace(instancesCSV) == "" {
		return defaults
	}

	parts := strings.Split(instancesCSV, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		u := strings.TrimSpace(p)
		if u == "" {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			continue
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			continue
		}
		base := parsed.Scheme + "://" + parsed.Host
		if parsed.Path != "" && parsed.Path != "/" {
			continue
		}
		if _, ok := seen[base]; ok {
			continue
		}
		seen[base] = struct{}{}
		out = append(out, base)
	}
	if len(out) == 0 {
		return defaults
	}
	return out
}

func (xr *XPublicReader) Run(ctx context.Context) error {
	xr.fetchAll(ctx)

	interval := xr.fetchInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			xr.fetchAll(ctx)
		case <-xr.refreshCh:
			xr.mu.Lock()
			xr.cache = make(map[string]cachedMessages)
			xr.mu.Unlock()
			xr.fetchAll(ctx)
			ticker.Reset(interval)
		}
	}
}

func (xr *XPublicReader) RequestRefresh() {
	select {
	case xr.refreshCh <- struct{}{}:
	default:
	}
}

func (xr *XPublicReader) UpdateChannels(_ []string) {}

// SetBaseCh updates the base channel number when Telegram channels are added/removed.
func (xr *XPublicReader) SetBaseCh(baseCh int) {
	xr.mu.Lock()
	xr.baseCh = baseCh
	xr.mu.Unlock()
}

func (xr *XPublicReader) fetchAll(ctx context.Context) {
	log.Printf("[x] fetch cycle started for %d accounts (instances: %v)", len(xr.accounts), xr.instances)
	start := time.Now()
	var fetched, failed, skipped int

	xr.mu.RLock()
	baseCh := xr.baseCh
	cacheTTL := xr.cacheTTL
	xr.mu.RUnlock()

	for i := range xr.accounts {
		xr.feed.SetChatInfo(baseCh+i, protocol.ChatTypeX, false)
	}

	for i, account := range xr.accounts {
		chNum := baseCh + i

		xr.mu.RLock()
		cached, ok := xr.cache[account]
		xr.mu.RUnlock()
		if ok && time.Since(cached.fetched) < cacheTTL {
			skipped++
			continue
		}

		msgs, title, err := xr.fetchAccount(ctx, account)
		if err != nil {
			log.Printf("[x] fetch @%s: all instances failed: %v", account, err)
			failed++
			continue
		}

		if title != "" {
			xr.feed.SetChannelDisplayName(chNum, title)
		}

		if ok && len(cached.msgs) > 0 {
			msgs = mergeMessages(cached.msgs, msgs)
		}
		if xr.msgLimit > 0 && len(msgs) > xr.msgLimit {
			msgs = msgs[:xr.msgLimit]
		}

		xr.mu.Lock()
		xr.cache[account] = cachedMessages{msgs: msgs, fetched: time.Now()}
		xr.mu.Unlock()

		xr.feed.UpdateChannel(chNum, msgs)
		fetched++
		log.Printf("[x] updated @%s: %d posts", account, len(msgs))
	}
	log.Printf("[x] fetch cycle done in %s: %d fetched, %d failed, %d skipped, %d total",
		time.Since(start).Round(time.Millisecond), fetched, failed, skipped, len(xr.accounts))
	// Avatars: same cycle, merged into the feed's profile-pic bundle. We
	// merge rather than overwrite because TelegramReader / PublicReader
	// may also be feeding into the same bundle.
	xr.fetchAllXProfilePhotos(ctx)
	xr.feed.AfterFetchCycle(ctx)
}

func (xr *XPublicReader) fetchAccount(ctx context.Context, username string) ([]protocol.Message, string, error) {
	var lastErr error
	for _, instance := range xr.instances {
		u := strings.TrimSuffix(instance, "/") + "/" + url.PathEscape(username) + "/rss"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			log.Printf("[x] @%s: instance %s: request build error: %v", username, instance, err)
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; thefeed/1.0; +https://github.com/sartoopjj/thefeed)")
		req.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, text/xml;q=0.8")

		resp, err := xr.client.Do(req)
		if err != nil {
			log.Printf("[x] @%s: instance %s: network error: %v", username, instance, err)
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxXRSSBodyBytes))
		if cerr := resp.Body.Close(); cerr != nil {
			log.Printf("[x] close response body: %v", cerr)
		}
		if readErr != nil {
			log.Printf("[x] @%s: instance %s: body read error: %v", username, instance, readErr)
			lastErr = readErr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			log.Printf("[x] @%s: instance %s: HTTP %s", username, instance, resp.Status)
			lastErr = fmt.Errorf("%s: unexpected HTTP status %s", instance, resp.Status)
			continue
		}

		msgs, sources, title, err := parseXRSSMessagesWithMedia(body, username)
		if err != nil {
			log.Printf("[x] @%s: instance %s: parse error: %v", username, instance, err)
			lastErr = fmt.Errorf("%s: %w", instance, err)
			continue
		}
		// Filter out garbled messages (invalid UTF-8 or mostly non-printable).
		cleaned := msgs[:0]
		cleanedSources := sources[:0]
		for i, m := range msgs {
			if isReadableText(m.Text) {
				cleaned = append(cleaned, m)
				if i < len(sources) {
					cleanedSources = append(cleanedSources, sources[i])
				} else {
					cleanedSources = append(cleanedSources, mediaSource{})
				}
			} else {
				log.Printf("[x] @%s: skipping garbled message ID=%d (len=%d)", username, m.ID, len(m.Text))
			}
		}
		if len(cleaned) == 0 {
			lastErr = fmt.Errorf("%s: all %d messages were garbled", instance, len(msgs))
			continue
		}
		// Run image downloads when a media cache is attached. Each Nitter
		// item carries an image URL we extracted from the description; for
		// non-image media types we have no public URL to fetch on X, so the
		// downstream rendering simply falls back to the legacy [TAG]\ncaption
		// form for those.
		if cache := xr.feed.MediaCache(); cache != nil && len(cleanedSources) > 0 {
			applyHTTPMediaSources(ctx, cache, cleaned, cleanedSources)
		}
		return cleaned, title, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no Nitter instances configured")
	}
	return nil, "", lastErr
}

type xRSS struct {
	Channel struct {
		Title string     `xml:"title"`
		Items []xRSSItem `xml:"item"`
	} `xml:"channel"`
}

type xRSSItem struct {
	GUID        string `xml:"guid"`
	Link        string `xml:"link"`
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Encoded     string `xml:"encoded"`
	PubDate     string `xml:"pubDate"`
}

func parseXRSSMessages(body []byte, feedUser string) ([]protocol.Message, string, error) {
	msgs, _, title, err := parseXRSSMessagesWithMedia(body, feedUser)
	return msgs, title, err
}

// parseXRSSMessagesWithMedia parses a Nitter RSS feed and additionally
// returns one mediaSource per parsed message — same length and order — so
// the caller can run HTTP downloads against the extracted image URLs and
// rewrite messages to use the [IMAGE]<size>:<dl>:<ch>:<blk>:<crc32> form.
// X posts on Nitter can contain multiple images per status; we only surface
// the *first* one for now, which keeps the download pipeline simple and
// matches what the legacy text rendering shows.
func parseXRSSMessagesWithMedia(body []byte, feedUser string) ([]protocol.Message, []mediaSource, string, error) {
	body = sanitizeUTF8(body)
	var feed xRSS
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, nil, "", fmt.Errorf("parse rss: %w", err)
	}
	if len(feed.Channel.Items) == 0 {
		return nil, nil, "", fmt.Errorf("empty rss feed")
	}

	title := strings.TrimSpace(feed.Channel.Title)

	feedUserLower := strings.ToLower(strings.TrimPrefix(feedUser, "@"))
	msgs := make([]protocol.Message, 0, len(feed.Channel.Items))
	sources := make([]mediaSource, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		id, err := extractXStatusID(item.GUID, item.Link)
		if err != nil {
			continue
		}
		ts := parseRSSDate(item.PubDate)
		text := strings.TrimSpace(stripXHTML(item.Description))
		if text == "" {
			text = strings.TrimSpace(stripXHTML(item.Encoded))
		}
		if text == "" {
			text = strings.TrimSpace(item.Title)
		}
		if text == "" {
			text = protocol.MediaFile + "\n" + strings.TrimSpace(item.Link)
		}

		// Detect pure retweet: link points to a different user's status.
		// Skip if text already has a repost banner from RT-prefix detection.
		if !strings.HasPrefix(text, "--------- Repost from") {
			if orig := extractLinkUsername(item.Link); orig != "" && strings.ToLower(orig) != feedUserLower {
				text = xDivider("Repost from @"+orig) + "\n" + text
			}
		}

		// Best-effort image extraction from the description / encoded HTML.
		src := mediaSource{}
		if u := extractFirstImgSrc(item.Description); u != "" {
			src = mediaSource{tag: protocol.MediaImage, url: u}
		} else if u := extractFirstImgSrc(item.Encoded); u != "" {
			src = mediaSource{tag: protocol.MediaImage, url: u}
		}

		msgs = append(msgs, protocol.Message{ID: id, Timestamp: ts, Text: text})
		sources = append(sources, src)
	}
	if len(msgs) == 0 {
		return nil, nil, "", fmt.Errorf("no parseable posts")
	}
	return msgs, sources, title, nil
}

// extractFirstImgSrc scans an HTML fragment for the first <img src="..."> and
// returns the URL value. Returns "" when no img is present. We avoid pulling
// in golang.org/x/net/html for this single-purpose lookup; the regex only
// needs to handle the simple cases Nitter generates.
func extractFirstImgSrc(htmlFrag string) string {
	if htmlFrag == "" {
		return ""
	}
	low := strings.ToLower(htmlFrag)
	idx := strings.Index(low, "<img ")
	if idx < 0 {
		return ""
	}
	tail := htmlFrag[idx:]
	srcIdx := strings.Index(strings.ToLower(tail), "src=")
	if srcIdx < 0 {
		return ""
	}
	tail = tail[srcIdx+len("src="):]
	if len(tail) == 0 {
		return ""
	}
	quote := tail[0]
	if quote != '"' && quote != '\'' {
		// Bare attribute value — read until whitespace or '>'.
		end := strings.IndexAny(tail, " >")
		if end < 0 {
			return ""
		}
		return strings.TrimSpace(tail[:end])
	}
	tail = tail[1:]
	end := strings.IndexByte(tail, quote)
	if end < 0 {
		return ""
	}
	return tail[:end]
}

// extractLinkUsername extracts the username from a Nitter/X status URL.
// e.g. "https://nitter.net/IranIntlTV/status/123" → "IranIntlTV"
func extractLinkUsername(link string) string {
	if link == "" {
		return ""
	}
	parsed, err := url.Parse(link)
	if err != nil {
		return ""
	}
	// Path like /Username/status/12345...
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) >= 2 && parts[1] == "status" && parts[0] != "" {
		return parts[0]
	}
	return ""
}

func extractXStatusID(guid, link string) (uint32, error) {
	s := guid
	if s == "" {
		s = link
	}
	if s == "" {
		return 0, fmt.Errorf("status id not found")
	}

	// Fast path for canonical links like /status/<id>.
	if idx := strings.Index(s, "/status/"); idx != -1 {
		rest := s[idx+len("/status/"):]
		end := len(rest)
		for i, c := range rest {
			if c < '0' || c > '9' {
				end = i
				break
			}
		}
		if end > 0 {
			if id, ok := parseXNumericID(rest[:end]); ok {
				return id, nil
			}
		}
	}

	// Fallback: find any numeric token (supports alternate link formats).
	tokens := xSnowflakeRe.FindAllString(s, -1)
	if len(tokens) == 0 && link != "" && link != s {
		tokens = xSnowflakeRe.FindAllString(link, -1)
	}
	for _, tok := range tokens {
		if id, ok := parseXNumericID(tok); ok {
			return id, nil
		}
	}

	// Final fallback: deterministic hash so this item is still kept.
	material := guid + "|" + link
	if material == "|" {
		return 0, fmt.Errorf("status id not found")
	}
	return crc32.ChecksumIEEE([]byte(material)), nil
}

func parseXNumericID(tok string) (uint32, bool) {
	n, err := strconv.ParseUint(tok, 10, 64)
	if err != nil {
		return 0, false
	}
	if n <= uint64(^uint32(0)) {
		return uint32(n), true
	}
	return crc32.ChecksumIEEE([]byte(tok)), true
}

func parseRSSDate(v string) uint32 {
	if v == "" {
		return uint32(time.Now().Unix())
	}
	formats := []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822}
	for _, f := range formats {
		if ts, err := time.Parse(f, v); err == nil {
			return uint32(ts.Unix())
		}
	}
	return uint32(time.Now().Unix())
}

func stripHTML(src string) string {
	n, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return src
	}
	return extractMessageText(n)
}

// stripXHTML extracts plain text from a Nitter RSS description and adds clear
// banner markers so users can distinguish original posts from reposts and quotes.
func stripXHTML(src string) string {
	n, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return src
	}
	result := extractXPostText(n)

	if handle, body, ok := parseXRepost(result); ok {
		return xDivider("Repost from "+handle) + "\n" + body
	}

	result = formatXQuoteMarkers(result)
	return result
}

// extractXPostText walks an HTML node tree like extractMessageText but places
// explicit quote markers before <blockquote> content.
func extractXPostText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur == nil {
			return
		}
		if cur.Type == html.TextNode {
			text := strings.TrimSpace(cur.Data)
			if text != "" {
				if b.Len() > 0 {
					last := b.String()[b.Len()-1]
					if last != '\n' && last != ' ' {
						b.WriteByte(' ')
					}
				}
				b.WriteString(text)
			}
		}
		if cur.Type == html.ElementNode {
			switch cur.Data {
			case "br":
				trimTrailingSpace(&b)
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				for child := cur.FirstChild; child != nil; child = child.NextSibling {
					walk(child)
				}
				return
			case "blockquote":
				// Insert marker before quoted content and format it later.
				trimTrailingSpace(&b)
				if b.Len() > 0 {
					b.WriteString("\n\n[[X_QUOTE]] ")
				} else {
					b.WriteString("[[X_QUOTE]] ")
				}
				for child := cur.FirstChild; child != nil; child = child.NextSibling {
					walk(child)
				}
				return
			}
		}
		for child := cur.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.TrimSpace(strings.ReplaceAll(b.String(), " \n", "\n"))
}

func xDivider(label string) string {
	return "--------- " + label + " ---------"
}

func parseXRepost(s string) (string, string, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", "", false
	}

	if m := xRepostLeadRe.FindStringSubmatch(trimmed); len(m) > 1 {
		body := strings.TrimSpace(trimmed[len(m[0]):])
		if body == "" {
			return "", "", false
		}
		return strings.TrimSpace(m[1]), body, true
	}

	if m := xRepostByRe.FindStringSubmatch(trimmed); len(m) > 1 {
		body := strings.TrimSpace(trimmed[len(m[0]):])
		if body == "" {
			return "", "", false
		}
		return strings.TrimSpace(m[1]), body, true
	}

	return "", "", false
}

func formatXQuoteMarkers(s string) string {
	const marker = "[[X_QUOTE]]"
	if !strings.Contains(s, marker) {
		return s
	}

	parts := strings.Split(s, marker)
	if len(parts) == 0 {
		return s
	}

	var b strings.Builder
	b.WriteString(strings.TrimSpace(parts[0]))
	for i := 1; i < len(parts); i++ {
		chunk := strings.TrimSpace(parts[i])
		label := "Quote"
		if m := xHandleRe.FindString(chunk); m != "" {
			label = "Quote from " + m
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(xDivider(label))
		if chunk != "" {
			b.WriteString("\n")
			b.WriteString(chunk)
		}
	}
	return strings.TrimSpace(b.String())
}

// isReadableText returns true if s is valid UTF-8 and at least half of its
// runes are printable (letters, digits, punctuation, symbols, spaces).
// This filters out garbled binary data that some Nitter instances return.
func isReadableText(s string) bool {
	if s == "" {
		return false
	}
	if !utf8.ValidString(s) {
		return false
	}
	var total, printable int
	for _, r := range s {
		total++
		if unicode.IsPrint(r) || r == '\n' || r == '\r' || r == '\t' {
			printable++
		}
	}
	if total == 0 {
		return false
	}
	return float64(printable)/float64(total) >= 0.5
}

// sanitizeUTF8 replaces invalid UTF-8 sequences with the Unicode replacement
// character so xml.Unmarshal doesn't choke on broken encodings.
func sanitizeUTF8(data []byte) []byte {
	if utf8.Valid(data) {
		return data
	}
	return []byte(strings.ToValidUTF8(string(data), "\uFFFD"))
}
