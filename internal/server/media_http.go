package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// httpMediaClient is a small shared client for fetching media URLs the
// public-Telegram and X readers extract. It deliberately uses a relatively
// short timeout — media downloads must not stall the rest of a fetch cycle.
var httpMediaClient = &http.Client{
	Timeout: 60 * time.Second,
	// Disallow redirects to non-http(s) schemes; Telegram CDN sometimes
	// redirects through 301/302 to a regional host which is fine.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("disallowed redirect scheme %q", req.URL.Scheme)
		}
		return nil
	},
}

// allowedMediaSchemes is the set of URL schemes downloadHTTPMedia will load.
var allowedMediaSchemes = map[string]bool{
	"http":  true,
	"https": true,
}

// downloadHTTPMedia fetches the bytes at rawURL and stores them in cache,
// using the URL itself as the cache key (so refreshing the same channel
// every 10 min just bumps TTL on hit).
//
// It enforces the configured max-size both up-front (Content-Length) and on
// the wire (LimitReader) so a server lying about size can't blow past the
// limit. URLs are validated against allowedMediaSchemes; private-network
// targets are not blocked here because callers (PublicReader, XPublicReader)
// only pass URLs scraped from Telegram/Nitter responses.
func downloadHTTPMedia(ctx context.Context, cache *MediaCache, tag, rawURL string) (protocol.MediaMeta, bool) {
	if cache == nil || rawURL == "" {
		return protocol.MediaMeta{}, false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !allowedMediaSchemes[parsed.Scheme] {
		return protocol.MediaMeta{}, false
	}

	// Cache key is the canonical URL — image-link rotation on the upstream
	// side will create a fresh entry, but identical URLs across fetches will
	// just refresh TTL.
	cacheKey := tag + ":url:" + parsed.String()
	if meta, ok := cache.Lookup(cacheKey); ok {
		return meta, true
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return protocol.MediaMeta{}, false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; thefeed/1.0)")
	req.Header.Set("Accept", "image/*, application/octet-stream;q=0.9, */*;q=0.5")

	resp, err := httpMediaClient.Do(req)
	if err != nil {
		logfMedia("[media-http] %s: request failed: %v", parsed.String(), err)
		return protocol.MediaMeta{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protocol.MediaMeta{}, false
	}
	// Defense in depth: reject HTML/XHTML responses outright. Telegram's
	// public web view sometimes redirects "file" links to the channel page
	// itself; without this check we'd happily cache the channel's HTML as
	// the user's downloadable file.
	ctype := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if ctype == "text/html" || ctype == "application/xhtml+xml" {
		logfMedia("[media-http] %s: refusing HTML response (got %s)", parsed.String(), ctype)
		return protocol.MediaMeta{}, false
	}

	maxBytes := cache.MaxAcceptableBytes()
	if maxBytes > 0 && resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		size := resp.ContentLength
		return protocol.MediaMeta{
			Tag:    tag,
			Size:   size,
			Relays: nil,
		}, true
	}

	limit := int64(-1)
	if maxBytes > 0 {
		limit = maxBytes + 1 // +1 to detect overflow vs exact match
	}
	var body io.Reader = resp.Body
	if limit > 0 {
		body = io.LimitReader(resp.Body, limit)
	}
	bytes, err := io.ReadAll(body)
	if err != nil {
		logfMedia("[media-http] %s: read failed: %v", parsed.String(), err)
		return protocol.MediaMeta{}, false
	}
	if maxBytes > 0 && int64(len(bytes)) > maxBytes {
		return protocol.MediaMeta{
			Tag:    tag,
			Size:   int64(len(bytes)),
			Relays: nil,
		}, true
	}

	meta, err := cache.Store(cacheKey, tag, bytes, resp.Header.Get("Content-Type"), urlBaseName(parsed))
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return meta, true
		}
		return protocol.MediaMeta{}, false
	}
	return meta, true
}

// urlBaseName returns the trailing path segment, stripped of its query, as a
// best-effort filename for HTTP layer Content-Disposition headers.
func urlBaseName(u *url.URL) string {
	if u == nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "" || base == "/" || base == "." {
		return ""
	}
	if i := strings.IndexByte(base, '?'); i >= 0 {
		base = base[:i]
	}
	return base
}
