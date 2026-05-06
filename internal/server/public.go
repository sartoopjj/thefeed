package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// PublicReader fetches recent posts from public Telegram channels via the web view.
type PublicReader struct {
	channels []string
	feed     *Feed
	msgLimit int
	baseCh   int

	client  *http.Client
	baseURL string

	mu            sync.RWMutex
	cache         map[string]cachedMessages
	cacheTTL      time.Duration
	fetchInterval time.Duration

	refreshCh chan struct{}
}

// SetFetchInterval overrides the default 10m fetch cadence. Caller must
// invoke before Run starts.
func (pr *PublicReader) SetFetchInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	pr.mu.Lock()
	pr.fetchInterval = d
	pr.cacheTTL = d
	pr.mu.Unlock()
}

// NewPublicReader creates a reader for public channels without Telegram login.
func NewPublicReader(channelUsernames []string, feed *Feed, msgLimit int, baseCh int) *PublicReader {
	cleaned := make([]string, len(channelUsernames))
	for i, u := range channelUsernames {
		cleaned[i] = strings.TrimPrefix(strings.TrimSpace(u), "@")
	}
	if msgLimit <= 0 {
		msgLimit = 15
	}
	if baseCh <= 0 {
		baseCh = 1
	}
	return &PublicReader{
		channels: cleaned,
		feed:     feed,
		msgLimit: msgLimit,
		baseCh:   baseCh,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		baseURL:   "https://t.me/s",
		cache:         make(map[string]cachedMessages),
		cacheTTL:      10 * time.Minute,
		fetchInterval: 10 * time.Minute,
		refreshCh:     make(chan struct{}, 1),
	}
}

// Run starts the periodic public-channel fetch loop.
func (pr *PublicReader) Run(ctx context.Context) error {
	pr.feed.SetTelegramLoggedIn(false)
	pr.fetchAll(ctx)

	interval := pr.fetchInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	pr.feed.SetNextFetch(uint32(time.Now().Add(interval).Unix()))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pr.fetchAll(ctx)
			pr.feed.SetNextFetch(uint32(time.Now().Add(interval).Unix()))
		case <-pr.refreshCh:
			pr.mu.Lock()
			pr.cache = make(map[string]cachedMessages)
			pr.mu.Unlock()
			pr.fetchAll(ctx)
			ticker.Reset(interval)
			pr.feed.SetNextFetch(uint32(time.Now().Add(interval).Unix()))
		}
	}
}

// RequestRefresh signals the fetch loop to re-fetch immediately.
func (pr *PublicReader) RequestRefresh() {
	select {
	case pr.refreshCh <- struct{}{}:
	default:
	}
}

// UpdateChannels replaces the channel list and updates Feed metadata.
func (pr *PublicReader) UpdateChannels(channels []string) {
	cleaned := make([]string, len(channels))
	for i, u := range channels {
		cleaned[i] = strings.TrimPrefix(strings.TrimSpace(u), "@")
	}
	pr.mu.Lock()
	pr.channels = cleaned
	pr.cache = make(map[string]cachedMessages)
	pr.mu.Unlock()
}

func (pr *PublicReader) fetchAll(ctx context.Context) {
	log.Printf("[public] fetch cycle started for %d channels", len(pr.channels))
	start := time.Now()
	var fetched, failed, skipped int
	pr.mu.RLock()
	cacheTTL := pr.cacheTTL
	pr.mu.RUnlock()
	for i, username := range pr.channels {
		chNum := pr.baseCh + i

		pr.mu.RLock()
		cached, ok := pr.cache[username]
		pr.mu.RUnlock()
		if ok && time.Since(cached.fetched) < cacheTTL {
			skipped++
			continue
		}

		msgs, title, err := pr.fetchChannel(ctx, username)
		if err != nil {
			log.Printf("[public] fetch %s: %v", username, err)
			failed++
			continue
		}

		// Merge new messages with previously cached ones to accumulate history.
		if ok && len(cached.msgs) > 0 {
			msgs = mergeMessages(cached.msgs, msgs)
		}
		if pr.msgLimit > 0 && len(msgs) > pr.msgLimit {
			msgs = msgs[:pr.msgLimit]
		}

		pr.mu.Lock()
		pr.cache[username] = cachedMessages{msgs: msgs, fetched: time.Now()}
		pr.mu.Unlock()

		pr.feed.UpdateChannel(chNum, msgs)
		pr.feed.SetChatInfo(chNum, protocol.ChatTypeChannel, false)
		pr.feed.SetChannelDisplayName(chNum, title)
		fetched++
		log.Printf("[public] updated %s (%s): %d messages", username, title, len(msgs))
	}
	log.Printf("[public] fetch cycle done in %s: %d fetched, %d failed, %d skipped, %d total",
		time.Since(start).Round(time.Millisecond), fetched, failed, skipped, len(pr.channels))
	// Fetch avatars for the same channel set — same cadence, best-effort.
	// Public mode has no MTProto session so we scrape them off t.me/s/<u>.
	pr.fetchAllPublicProfilePhotos(ctx)
	pr.feed.AfterFetchCycle(ctx)
}

func (pr *PublicReader) fetchChannel(ctx context.Context, username string) ([]protocol.Message, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pr.baseURL+"/"+url.PathEscape(username), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; thefeed/1.0; +https://github.com/sartoopjj/thefeed)")

	resp, err := pr.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("unexpected HTTP status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	msgs, sources, err := parsePublicMessagesWithMedia(body)
	if err != nil {
		return nil, "", err
	}
	// If the server has a configured media cache, fetch each scraped image
	// URL and rewrite the corresponding message text to embed downloadable
	// metadata. Failures here are best-effort: messages keep their legacy
	// "[IMAGE]\ncaption" body when downloads don't succeed.
	if cache := pr.feed.MediaCache(); cache != nil {
		applyHTTPMediaSources(ctx, cache, msgs, sources)
	}
	return msgs, extractChannelTitle(body), nil
}

// applyHTTPMediaSources downloads each src.url (+ extraURLs for albums) and
// rewrites the matching message body with N stacked downloadable metadata
// lines. Failed downloads emit a bare [TAG] so the album's ID span is
// preserved.
func applyHTTPMediaSources(ctx context.Context, cache *MediaCache, msgs []protocol.Message, sources []mediaSource) {
	for i := range msgs {
		if i >= len(sources) {
			break
		}
		src := sources[i]
		if src.url == "" || src.tag == "" {
			continue
		}
		// Strip every leading [TAG] header so we can re-emit clean metadata
		// (ParseMediaText only peels one tag per call).
		body := msgs[i].Text
		for {
			_, rest, parsed := protocol.ParseMediaText(body)
			if !parsed {
				break
			}
			body = rest
		}
		caption := body

		urls := append([]string{src.url}, src.extraURLs...)
		var encoded strings.Builder
		downloaded := 0
		for j, u := range urls {
			meta, ok := downloadHTTPMedia(ctx, cache, src.tag, u)
			if j > 0 {
				encoded.WriteByte('\n')
			}
			if !ok {
				encoded.WriteString(src.tag)
				continue
			}
			downloaded++
			encoded.WriteString(strings.TrimSuffix(meta.String(), "\n"))
		}
		if downloaded == 0 {
			continue
		}
		newText := encoded.String()
		if caption != "" {
			newText += "\n" + caption
		}
		msgs[i].Text = newText
	}
}

// mediaSource is the per-message media descriptor returned by the public
// scraper. extraURLs holds additional album siblings; url is the first one.
type mediaSource struct {
	tag       string
	url       string
	extraURLs []string
}

// extractChannelTitle parses the channel display name from the Telegram public page.
func extractChannelTitle(body []byte) string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return ""
	}
	titleNode := findFirstByClass(doc, "tgme_channel_info_header_title")
	if titleNode == nil {
		return ""
	}
	return strings.TrimSpace(extractInnerText(titleNode))
}

type publicMessage struct {
	id        uint32
	timestamp uint32
	text      string
}

// mergeMessages combines old cached messages with newly fetched ones.
// New messages win on ID conflicts (edits). Result is sorted by ID descending.
func mergeMessages(old, new []protocol.Message) []protocol.Message {
	byID := make(map[uint32]protocol.Message, len(old)+len(new))
	for _, m := range old {
		byID[m.ID] = m
	}
	for _, m := range new {
		byID[m.ID] = m // new overwrites old (edits)
	}
	merged := make([]protocol.Message, 0, len(byID))
	for _, m := range byID {
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].ID > merged[j].ID
	})
	return merged
}

func parsePublicMessages(body []byte) ([]protocol.Message, error) {
	msgs, _, err := parsePublicMessagesWithMedia(body)
	return msgs, err
}

// parsePublicMessagesWithMedia is identical to parsePublicMessages but also
// returns a per-message media descriptor — same length and ordering as the
// returned messages — that callers can use to fetch the underlying photo or
// document over HTTP and rewrite the message body. The legacy behaviour
// (returning just messages) is preserved by parsePublicMessages above for
// existing tests and pre-feature callers.
func parsePublicMessagesWithMedia(body []byte) ([]protocol.Message, []mediaSource, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, nil, fmt.Errorf("parse html: %w", err)
	}

	type collectedMsg struct {
		msg publicMessage
		src mediaSource
	}
	var collected []collectedMsg
	visitNodes(doc, func(n *html.Node) {
		post := attrValue(n, "data-post")
		if post == "" {
			return
		}
		id, err := parsePostID(post)
		if err != nil {
			return
		}
		text := strings.TrimSpace(extractMessageText(findMessageBodyNode(n)))
		var src mediaSource
		mediaPrefix := ""
		switch {
		case findFirstByClass(n, "tgme_widget_message_photo_wrap") != nil:
			// Albums share one data-post block with N nested photo wraps.
			// Stack N [IMAGE] headers so the client-side gap detector
			// (albumSpan) doesn't flag the absorbed sibling IDs as missing.
			photoWraps := findAllByClass(n, "tgme_widget_message_photo_wrap")
			if len(photoWraps) > 1 {
				headers := make([]string, len(photoWraps))
				for i := range headers {
					headers[i] = protocol.MediaImage
				}
				mediaPrefix = strings.Join(headers, "\n")
			} else {
				mediaPrefix = protocol.MediaImage
			}
			src = mediaSource{tag: protocol.MediaImage, url: extractBackgroundImageURL(photoWraps[0])}
			for i := 1; i < len(photoWraps); i++ {
				if u := extractBackgroundImageURL(photoWraps[i]); u != "" {
					src.extraURLs = append(src.extraURLs, u)
				}
			}
		case findFirstByClass(n, "tgme_widget_message_video_player") != nil ||
			findFirstByClass(n, "tgme_widget_message_roundvideo_player") != nil:
			mediaPrefix = protocol.MediaVideo
			// t.me/s/ does not serve real video bytes — the player anchor links
			// to the channel page itself. Don't try to download.
		case findFirstByClass(n, "tgme_widget_message_sticker_wrap") != nil:
			mediaPrefix = protocol.MediaSticker
			// Stickers are emitted as the legacy tag only; we don't cache or
			// serve their bytes (animated/.tgs variants don't render inline
			// in the browser anyway).
		case findFirstByClass(n, "tgme_widget_message_voice") != nil:
			mediaPrefix = protocol.MediaAudio
			// Public web view doesn't expose voice file bytes either.
		case findFirstByClass(n, "tgme_widget_message_poll") != nil:
			mediaPrefix = protocol.MediaPoll
			pollBody := extractPollData(n)
			if pollBody != "" {
				if text != "" {
					text = mediaPrefix + "\n" + pollBody + "\n" + text
				} else {
					text = mediaPrefix + "\n" + pollBody
				}
				mediaPrefix = "" // already handled
			}
		case findFirstByClass(n, "tgme_widget_message_location_wrap") != nil ||
			findFirstByClass(n, "tgme_widget_message_venue_wrap") != nil:
			mediaPrefix = protocol.MediaLocation
		case findFirstByClass(n, "tgme_widget_message_contact_wrap") != nil:
			mediaPrefix = protocol.MediaContact
		case findFirstByClass(n, "tgme_widget_message_document_wrap") != nil:
			mediaPrefix = protocol.MediaFile
			// In t.me/s/ the document link is a "view in Telegram" page link,
			// not the file CDN — fetching it would download the channel HTML.
			// Skip; documents are downloadable only when the server runs with
			// a Telegram login (gotd UploadGetFile path).
		case findFirstByClass(n, "message_media_not_supported") != nil:
			// Telegram shows "Please open Telegram to view this post" for
			// content the public web view can't render: polls/quizzes, but
			// also for messages that contain Telegram Premium custom emoji.
			// Only tag a message as a poll when it has no other text body —
			// otherwise we'd false-positive normal posts (e.g. @networkti)
			// that happen to use premium emojis. See bug: #239.
			if text == "" {
				mediaPrefix = protocol.MediaPoll
			}
		}
		if mediaPrefix != "" {
			if text != "" {
				text = mediaPrefix + "\n" + text
			} else {
				text = mediaPrefix
			}
		}
		if text == "" {
			return
		}
		// Detect replies by checking for the reply preview element.
		if replyNode := findFirstByClass(n, "tgme_widget_message_reply"); replyNode != nil {
			replyID := extractReplyID(replyNode)
			if replyID > 0 {
				text = fmt.Sprintf("%s:%d\n%s", protocol.MediaReply, replyID, text)
			} else {
				text = protocol.MediaReply + "\n" + text
			}
		}
		collected = append(collected, collectedMsg{
			msg: publicMessage{
				id:        id,
				timestamp: extractMessageTimestamp(n),
				text:      text,
			},
			src: src,
		})
	})

	if len(collected) == 0 {
		return nil, nil, fmt.Errorf("no public messages found")
	}

	sort.Slice(collected, func(i, j int) bool {
		return collected[i].msg.id > collected[j].msg.id
	})

	msgs := make([]protocol.Message, 0, len(collected))
	sources := make([]mediaSource, 0, len(collected))
	for _, c := range collected {
		msgs = append(msgs, protocol.Message{ID: c.msg.id, Timestamp: c.msg.timestamp, Text: c.msg.text})
		sources = append(sources, c.src)
	}
	return msgs, sources, nil
}

func visitNodes(n *html.Node, fn func(*html.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		visitNodes(child, fn)
	}
}

func findFirstByClass(n *html.Node, class string) *html.Node {
	var found *html.Node
	visitNodes(n, func(cur *html.Node) {
		if found != nil {
			return
		}
		if hasClass(cur, class) {
			found = cur
		}
	})
	return found
}

// findAllByClass returns every descendant of n that carries the given class.
func findAllByClass(n *html.Node, class string) []*html.Node {
	var found []*html.Node
	visitNodes(n, func(cur *html.Node) {
		if hasClass(cur, class) {
			found = append(found, cur)
		}
	})
	return found
}

func hasClass(n *html.Node, class string) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	for _, attr := range n.Attr {
		if attr.Key != "class" {
			continue
		}
		for _, token := range strings.Fields(attr.Val) {
			if token == class {
				return true
			}
		}
	}
	return false
}

func attrValue(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func parsePostID(post string) (uint32, error) {
	idx := strings.LastIndex(post, "/")
	if idx == -1 || idx+1 >= len(post) {
		return 0, fmt.Errorf("invalid post id")
	}
	id, err := strconv.ParseUint(post[idx+1:], 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(id), nil
}

func extractMessageTimestamp(n *html.Node) uint32 {
	timeNode := findFirstByClass(n, "tgme_widget_message_date")
	if timeNode == nil {
		timeNode = findFirstElement(n, "time")
	}
	if timeNode == nil {
		return uint32(time.Now().Unix())
	}
	datetime := attrValue(timeNode, "datetime")
	if datetime == "" {
		timeChild := findFirstElement(timeNode, "time")
		datetime = attrValue(timeChild, "datetime")
	}
	if datetime == "" {
		return uint32(time.Now().Unix())
	}
	ts, err := time.Parse(time.RFC3339, datetime)
	if err != nil {
		return uint32(time.Now().Unix())
	}
	return uint32(ts.Unix())
}

func findFirstElement(n *html.Node, tag string) *html.Node {
	var found *html.Node
	visitNodes(n, func(cur *html.Node) {
		if found == nil && cur.Type == html.ElementNode && cur.Data == tag {
			found = cur
		}
	})
	return found
}

// findMessageBodyNode returns the main post text node while skipping reply preview
// snippets. In Telegram public HTML, quoted/replied text may appear before the
// real message body and can otherwise be mistakenly parsed as the post text.
func findMessageBodyNode(n *html.Node) *html.Node {
	var found *html.Node
	visitNodes(n, func(cur *html.Node) {
		if found != nil || !hasClass(cur, "tgme_widget_message_text") {
			return
		}
		for p := cur.Parent; p != nil; p = p.Parent {
			if hasClass(p, "tgme_widget_message_reply") {
				return
			}
		}
		found = cur
	})
	if found != nil {
		return found
	}
	return findFirstByClass(n, "tgme_widget_message_text")
}

func extractMessageText(n *html.Node) string {
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
		if cur.Type == html.ElementNode && cur.Data == "br" {
			trimTrailingSpace(&b)
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
		}
		// Preserve hyperlinks: if <a href="URL"> and the link text differs from the URL, append it.
		if cur.Type == html.ElementNode && cur.Data == "a" {
			href := attrValue(cur, "href")
			// Only preserve safe http(s) URLs; reject javascript: and other schemes.
			if href != "" && !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
				href = ""
			}
			linkText := extractInnerText(cur)
			if href != "" && linkText != "" && linkText != href {
				if b.Len() > 0 {
					last := b.String()[b.Len()-1]
					if last != '\n' && last != ' ' {
						b.WriteByte(' ')
					}
				}
				b.WriteString("[" + linkText + "](" + href + ")")
				return // skip walking children, already consumed
			} else if href != "" && (linkText == "" || linkText == href) {
				if b.Len() > 0 {
					last := b.String()[b.Len()-1]
					if last != '\n' && last != ' ' {
						b.WriteByte(' ')
					}
				}
				b.WriteString(href)
				return // skip walking children
			}
		}
		for child := cur.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.TrimSpace(strings.ReplaceAll(b.String(), " \n", "\n"))
}

func trimTrailingSpace(b *strings.Builder) {
	s := b.String()
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	b.Reset()
	b.WriteString(s)
}

// extractInnerText returns the concatenated text content of a node and its children.
func extractInnerText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur.Type == html.TextNode {
			b.WriteString(cur.Data)
		}
		for child := cur.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// extractPollData extracts poll question and options from the public HTML widget.
// Telegram's poll HTML uses these classes:
//   - tgme_widget_message_poll_question → question text
//   - tgme_widget_message_poll_option_text → each option's text
func extractPollData(n *html.Node) string {
	question := ""
	if qNode := findFirstByClass(n, "tgme_widget_message_poll_question"); qNode != nil {
		question = strings.TrimSpace(extractMessageText(qNode))
	}
	var options []string
	visitNodes(n, func(cur *html.Node) {
		if hasClass(cur, "tgme_widget_message_poll_option_text") {
			opt := strings.TrimSpace(extractMessageText(cur))
			if opt != "" {
				options = append(options, "○ "+opt)
			}
		}
	})
	// Only return poll if both question and at least one option exist
	if question == "" || len(options) == 0 {
		return ""
	}
	result := "📊 " + question
	result += "\n" + strings.Join(options, "\n")
	return result
}

// extractReplyID parses the href of the reply element to get the replied-to message ID.
// The href typically looks like "https://t.me/channel/123" or "?single&reply=123".
func extractReplyID(replyNode *html.Node) uint32 {
	href := ""
	// The reply element itself may be an <a> or contain one.
	if replyNode.Type == html.ElementNode && replyNode.Data == "a" {
		href = attrValue(replyNode, "href")
	}
	if href == "" {
		linkNode := findFirstElement(replyNode, "a")
		if linkNode != nil {
			href = attrValue(linkNode, "href")
		}
	}
	if href == "" {
		return 0
	}
	// Parse the last path segment as the message ID.
	id, err := parsePostID(href)
	if err != nil {
		return 0
	}
	return id
}

// extractBackgroundImageURL pulls the URL out of an inline
// `style="background-image:url('...')"` attribute. Telegram's public photo
// widget uses this pattern to render thumbnails — the URL points to the
// CDN-hosted image and is the source we want to download. Returns an empty
// string when the pattern is not present.
func extractBackgroundImageURL(n *html.Node) string {
	if n == nil {
		return ""
	}
	style := attrValue(n, "style")
	if style == "" {
		return ""
	}
	idx := strings.Index(style, "background-image")
	if idx < 0 {
		return ""
	}
	// Find url(...) after the property name.
	rest := style[idx:]
	open := strings.Index(rest, "url(")
	if open < 0 {
		return ""
	}
	rest = rest[open+len("url("):]
	close := strings.IndexByte(rest, ')')
	if close < 0 {
		return ""
	}
	raw := strings.TrimSpace(rest[:close])
	raw = strings.TrimPrefix(raw, "'")
	raw = strings.TrimSuffix(raw, "'")
	raw = strings.TrimPrefix(raw, "\"")
	raw = strings.TrimSuffix(raw, "\"")
	return raw
}

