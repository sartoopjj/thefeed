package web

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

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

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
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
	encBody, _, err := fetchGitHubRaw(ctx, url, size+int64(aeadOverhead))
	if err != nil {
		s.addLog(fmt.Sprintf("relay: fetch %s: %v", url, err))
		// Not handled — caller falls back to DNS.
		rc.invalidate() // refresh next time in case the repo URL changed
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

func fetchGitHubRaw(ctx context.Context, url string, expectedSize int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "thefeed-client")
	// Ask the contents API for raw bytes; without this it returns a JSON
	// envelope with the body base64-encoded inside.
	req.Header.Set("Accept", "application/vnd.github.raw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("github raw: %s", resp.Status)
	}
	limit := expectedSize
	if limit <= 0 {
		limit = 100 * 1024 * 1024 // 100 MiB ceiling
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
