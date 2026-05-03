package web

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// ghRateLimitError is returned by fetchGitHubRaw when GitHub rejects
// with 403 + X-RateLimit-Remaining: 0. Surfacing it as a typed error
// lets the caller render a specific 429 to the browser so the UI can
// show a "rate limited, try again at HH:MM" popup instead of a generic
// relay failure.
type ghRateLimitError struct {
	ResetUnix int64
	Body      string
}

func (e *ghRateLimitError) Error() string {
	return fmt.Sprintf("github rate limit (reset=%d): %s", e.ResetUnix, e.Body)
}

// relayHTTPClient is the single shared HTTP client for the GitHub relay
// path. Reusing one client (and its underlying *http.Transport) gives us
// connection pooling and DNS-result caching for free across the many
// per-file fetches a media-heavy refresh cycle produces.
//
// We use the OS resolver everywhere. On Android the build is cgo-enabled
// (see .github/workflows/build.yml), so net.Lookup* goes through
// bionic libc → netd → the device's actual DNS, the same path any other
// Android app uses. On desktop the OS resolver is similarly fine.
var relayHTTPClient = &http.Client{
	// Per-request budget — large enough to cover multi-MB downloads
	// over a slow link without hugging short-circuit timeouts.
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// relayInfoTTL is how long the cached repo-discovery payload stays valid.
// Re-fetched after expiry, on profile switch, or after a download failure.
const relayInfoTTL = time.Hour

// relayCache holds the most recent answer from RelayInfoChannel so we don't
// hit DNS for every fast-path media fetch.
type relayCache struct {
	mu       sync.Mutex
	info     client.RelayInfo
	fetched  time.Time
	fetching bool
	cond     *sync.Cond
}

func newRelayCache() *relayCache {
	rc := &relayCache{}
	rc.cond = sync.NewCond(&rc.mu)
	return rc
}

func (c *relayCache) invalidate() {
	c.mu.Lock()
	c.info = client.RelayInfo{}
	c.fetched = time.Time{}
	c.mu.Unlock()
}

func (c *relayCache) get(ctx context.Context, fetcher *client.Fetcher) (client.RelayInfo, error) {
	c.mu.Lock()
	if !c.fetched.IsZero() && time.Since(c.fetched) < relayInfoTTL {
		info := c.info
		c.mu.Unlock()
		return info, nil
	}
	for c.fetching {
		c.cond.Wait()
		if !c.fetched.IsZero() && time.Since(c.fetched) < relayInfoTTL {
			info := c.info
			c.mu.Unlock()
			return info, nil
		}
	}
	c.fetching = true
	c.mu.Unlock()

	info, err := fetcher.FetchRelayInfo(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetching = false
	c.cond.Broadcast()
	if err != nil {
		return client.RelayInfo{}, err
	}
	c.info = info
	c.fetched = time.Now()
	return info, nil
}

// serveFromGitHubRelay tries to stream the file from raw.githubusercontent.com
// Returns true if the request was fully handled (success or terminal error
// already written). Returns false to let the caller fall back to DNS.
func (s *Server) serveFromGitHubRelay(w http.ResponseWriter, r *http.Request, size int64, crc uint32, filename, mimeOverride string) bool {
	if size <= 0 || crc == 0 {
		return false
	}
	s.mu.RLock()
	fetcher := s.fetcher
	rc := s.relayInfo
	cache := s.mediaCache
	cfg := s.config
	s.mu.RUnlock()
	if fetcher == nil || rc == nil || cfg == nil || cfg.Domain == "" {
		return false
	}

	// Long enough to cover a multi-MB GitHub fetch over a slow link, plus
	// a multi-block DNS-tunneled relay-info lookup if the cache is empty.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	info, err := rc.get(ctx, fetcher)
	if err != nil || info.GitHubRepo == "" {
		return false
	}
	if cfg.Key == "" {
		return false
	}
	relayKey, err := protocol.DeriveRelayKey(cfg.Key)
	if err != nil {
		return false
	}
	domainSeg := protocol.RelayDomainSegment(cfg.Domain, cfg.Key)
	objectSeg := protocol.RelayObjectName(size, crc, cfg.Key)

	// Disk cache short-circuit (same as DNS path) — we cache PLAINTEXT under
	// (size, crc), so a hit doesn't need to decrypt.
	if cache != nil {
		if body, mime, ok := cache.Get(size, crc); ok {
			servedMime := pickMime(mimeOverride, mime, body)
			writeMediaHeaders(w, servedMime, size, filename, "HIT-relay")
			if _, err := w.Write(body); err != nil {
				s.addLog(fmt.Sprintf("relay: hit-cache write: %v", err))
			}
			return true
		}
	}

	// Use api.github.com (a *.github.com host) instead of
	// raw.githubusercontent.com — the latter is blocked in some countries
	// where the api host still resolves. The Accept header asks for raw
	// bytes instead of the default JSON envelope. Both path segments are
	// HMAC'd with the passphrase so the URL itself doesn't leak the domain
	// or which file is being requested.
	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s/%s",
		info.GitHubRepo, domainSeg, objectSeg)
	// The blob on disk is AES-256-GCM(nonce||ct||tag) over the plaintext.
	// Cap the fetch at plaintext size + small overhead.
	const aeadOverhead = protocol.NonceSize + 16 // GCM tag is 16 bytes
	encBody, _, err := fetchGitHubRaw(ctx, relayHTTPClient, url, size+int64(aeadOverhead))
	if err != nil {
		s.addLog(fmt.Sprintf("relay: fetch %s: %v", url, err))
		// Rate-limit gets surfaced specifically so the UI can show a
		// "try again at HH:MM" popup. Other errors fall through to the
		// caller's 502 → existing slow-DNS fallback prompt.
		var rl *ghRateLimitError
		if errors.As(err, &rl) {
			minutes := int64(1)
			if rl.ResetUnix > 0 {
				secs := rl.ResetUnix - time.Now().Unix()
				if secs > 0 {
					minutes = (secs + 59) / 60
				}
			}
			w.Header().Set("X-Relay-Reset", strconv.FormatInt(rl.ResetUnix, 10))
			w.Header().Set("X-Relay-Reset-Min", strconv.FormatInt(minutes, 10))
			http.Error(w, "github rate limit", http.StatusTooManyRequests)
			return true
		}
		return false
	}
	body, err := protocol.DecryptRelayBlob(relayKey, encBody)
	if err != nil {
		s.addLog(fmt.Sprintf("relay: decrypt %s: %v", url, err))
		return false
	}
	if int64(len(body)) != size || crc32.ChecksumIEEE(body) != crc {
		s.addLog(fmt.Sprintf("relay: hash/size mismatch from %s", url))
		return false
	}
	mime := http.DetectContentType(body)

	servedMime := pickMime(mimeOverride, mime, body)
	writeMediaHeaders(w, servedMime, size, filename, "MISS-relay")
	if _, err := w.Write(body); err != nil {
		s.addLog(fmt.Sprintf("relay: stream: %v", err))
	}
	if cache != nil {
		if err := cache.Put(size, crc, body, servedMime); err != nil {
			s.addLog(fmt.Sprintf("relay: cache put %d_%08x: %v", size, crc, err))
		} else {
			s.addLog(fmt.Sprintf("media cached (relay): %d bytes, crc=%08x, mime=%s", size, crc, servedMime))
		}
	}
	return true
}

func fetchGitHubRaw(ctx context.Context, hc *http.Client, url string, expectedSize int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "git-client/1.0")
	// Ask the contents API for raw bytes; without this it returns a JSON
	// envelope with the body base64-encoded inside.
	req.Header.Set("Accept", "application/vnd.github.raw")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		rl := resp.Header.Get("X-RateLimit-Remaining")
		reset := resp.Header.Get("X-RateLimit-Reset")
		retry := resp.Header.Get("Retry-After")
		// Distinct typed error for the rate-limit case so the caller
		// can render a 429 with the reset time instead of a generic
		// 502 / fallback error.
		if resp.StatusCode == http.StatusForbidden && rl == "0" {
			resetUnix, _ := strconv.ParseInt(reset, 10, 64)
			return nil, "", &ghRateLimitError{
				ResetUnix: resetUnix,
				Body:      strings.TrimSpace(string(errBody)),
			}
		}
		hdr := ""
		if rl != "" {
			hdr += " rl-remaining=" + rl
		}
		if reset != "" {
			hdr += " rl-reset=" + reset
		}
		if retry != "" {
			hdr += " retry-after=" + retry
		}
		return nil, "", fmt.Errorf("github raw: %s%s — %s",
			resp.Status, hdr, strings.TrimSpace(string(errBody)))
	}
	limit := expectedSize
	if limit <= 0 {
		limit = 100 * 1024 * 1024 // 100 MiB ceiling
	}
	// Prefer Content-Length, fall back to the caller's expectedSize
	// (the encrypted-blob byte count). ReadFull stops the moment we
	// have that many bytes — no waiting for FIN, so a censoring
	// proxy that holds the socket open after delivering the body
	// can't wedge us at 100 %.
	target := resp.ContentLength
	if target <= 0 && expectedSize > 0 {
		target = expectedSize
	}
	if target > 0 && target <= limit+1 {
		body := make([]byte, target)
		if _, err := io.ReadFull(resp.Body, body); err != nil {
			return nil, "", err
		}
		return body, resp.Header.Get("Content-Type"), nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, "", errors.New("github raw: body exceeds expected size")
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func pickMime(override, fromCache string, sniff []byte) string {
	if m := sanitizeMime(override); m != "" && m != "application/octet-stream" {
		return m
	}
	if fromCache != "" {
		if m := sanitizeMime(fromCache); m != "" {
			return m
		}
	}
	if sniff != nil {
		if m := sanitizeMime(http.DetectContentType(sniff)); m != "" {
			return m
		}
	}
	return "application/octet-stream"
}

func writeMediaHeaders(w http.ResponseWriter, mime string, size int64, filename, cacheTag string) {
	w.Header().Set("Content-Type", mime)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if cacheTag != "" {
		w.Header().Set("X-Cache", cacheTag)
	}
	if fn := sanitizeFilename(filename); fn != "" {
		w.Header().Set("Content-Disposition", "inline; filename=\""+fn+"\"")
	}
}
