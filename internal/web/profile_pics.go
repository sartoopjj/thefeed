package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// profilePicsHub caches channel avatars on disk and coordinates the
// background fetch. GitHub bundle is automatic; per-entry DNS path
// requires the ProfilePicsEnabled toggle.
type profilePicsHub struct {
	dataDir string

	mu       sync.Mutex
	index    map[string]profilePicCacheEntry // username (lower) -> entry
	fetching bool
	progress profilePicProgress
	// CRC of the bundle that produced our current cache; if the next
	// directory advertises the same value we skip the download.
	bundleCRC uint32
}

type profilePicCacheEntry struct {
	CRC       uint32 `json:"crc"`
	Size      uint32 `json:"size"`
	MIME      uint8  `json:"mime"`
	Extension string `json:"ext"`
	StoredAt  int64  `json:"storedAt"`
}

// profilePicProgress is polled by the UI during refresh.
type profilePicProgress struct {
	Active   bool   `json:"active"`
	Total    int    `json:"total"`
	Done     int    `json:"done"`
	Failed   int    `json:"failed"`
	Username string `json:"username,omitempty"`
	Error    string `json:"error,omitempty"`
}

type profilePicsIndexFile struct {
	BundleCRC uint32                          `json:"bundleCrc"`
	Users     map[string]profilePicCacheEntry `json:"users"`
}

func newProfilePicsHub(dataDir string) *profilePicsHub {
	h := &profilePicsHub{
		dataDir: dataDir,
		index:   make(map[string]profilePicCacheEntry),
	}
	h.loadIndex()
	return h
}

func (h *profilePicsHub) cacheDir() string {
	return filepath.Join(h.dataDir, "profile_pics")
}

func (h *profilePicsHub) indexPath() string {
	return filepath.Join(h.cacheDir(), "index.json")
}

func (h *profilePicsHub) imagePath(username, ext string) string {
	// "x:handle" → "x__handle" — Windows can't use ':' in filenames.
	safe := strings.ReplaceAll(strings.ToLower(username), ":", "__")
	return filepath.Join(h.cacheDir(), safe+ext)
}

func (h *profilePicsHub) loadIndex() {
	b, err := os.ReadFile(h.indexPath())
	if err != nil {
		return
	}
	var idx profilePicsIndexFile
	if err := json.Unmarshal(b, &idx); err != nil {
		return
	}
	h.mu.Lock()
	h.index = idx.Users
	if h.index == nil {
		h.index = make(map[string]profilePicCacheEntry)
	}
	h.bundleCRC = idx.BundleCRC
	h.mu.Unlock()
}

func (h *profilePicsHub) saveIndexLocked() {
	if err := os.MkdirAll(h.cacheDir(), 0700); err != nil {
		return
	}
	idx := profilePicsIndexFile{
		BundleCRC: h.bundleCRC,
		Users:     h.index,
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(h.indexPath(), b, 0600)
}

// Store writes bytes to disk and updates the index. Any previous file
// for this username (different extension) is removed.
func (h *profilePicsHub) Store(username string, content []byte, crc uint32, mime uint8) error {
	if username == "" || len(content) == 0 {
		return errors.New("profile-pics: empty input")
	}
	if err := os.MkdirAll(h.cacheDir(), 0700); err != nil {
		return err
	}
	ext := extensionFor(mime)
	tmp := h.imagePath(username, ext) + ".tmp"
	if err := os.WriteFile(tmp, content, 0600); err != nil {
		return err
	}
	final := h.imagePath(username, ext)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	h.mu.Lock()
	if old, ok := h.index[strings.ToLower(username)]; ok && old.Extension != ext {
		_ = os.Remove(h.imagePath(username, old.Extension))
	}
	h.index[strings.ToLower(username)] = profilePicCacheEntry{
		CRC:       crc,
		Size:      uint32(len(content)),
		MIME:      mime,
		Extension: ext,
		StoredAt:  time.Now().Unix(),
	}
	h.saveIndexLocked()
	h.mu.Unlock()
	return nil
}

// Get returns the cached bytes + content type, or os.ErrNotExist.
func (h *profilePicsHub) Get(username string) ([]byte, string, error) {
	h.mu.Lock()
	e, ok := h.index[strings.ToLower(username)]
	h.mu.Unlock()
	if !ok {
		return nil, "", os.ErrNotExist
	}
	b, err := os.ReadFile(h.imagePath(username, e.Extension))
	if err != nil {
		return nil, "", err
	}
	return b, contentTypeFor(e.MIME), nil
}

// Clear wipes both the on-disk cache and the index. Hooked by /api/cache/clear.
func (h *profilePicsHub) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.index = make(map[string]profilePicCacheEntry)
	h.bundleCRC = 0
	if entries, err := os.ReadDir(h.cacheDir()); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				_ = os.Remove(filepath.Join(h.cacheDir(), e.Name()))
			}
		}
	}
}

func (h *profilePicsHub) Progress() profilePicProgress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.progress
}

// relayFetcher pulls the bundle bytes from the GitHub relay. nil means
// "skip the GitHub path".
type relayFetcher func(ctx context.Context, size int64, crc uint32) ([]byte, error)

// storedCallback fires after each successful persist so the server can
// push an SSE event mid-refresh.
type storedCallback func(username string)

// Refresh is the test entry point (DNS only, no GitHub fetcher).
func (h *profilePicsHub) Refresh(ctx context.Context, fetcher *client.Fetcher, dnsAllowed bool) error {
	return h.refresh(ctx, fetcher, dnsAllowed, nil, nil)
}

// refresh tries the GitHub bundle first, then per-entry DNS for
// anything still missing. Coalesces concurrent calls.
func (h *profilePicsHub) refresh(ctx context.Context, fetcher *client.Fetcher, dnsAllowed bool, viaGitHub relayFetcher, onStored storedCallback) error {
	h.mu.Lock()
	if h.fetching {
		h.mu.Unlock()
		return errors.New("profile-pics: refresh already in progress")
	}
	h.fetching = true
	h.progress = profilePicProgress{Active: true}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		h.fetching = false
		h.progress.Active = false
		h.mu.Unlock()
	}()

	bundle, err := fetcher.FetchProfilePicDirectory(ctx)
	if err != nil {
		h.setProgressErr(fmt.Sprintf("fetch directory: %v", err))
		return err
	}
	if len(bundle.Entries) == 0 {
		return nil
	}

	h.mu.Lock()
	h.progress.Total = len(bundle.Entries)
	prevCRC := h.bundleCRC
	h.mu.Unlock()

	// Same bundle, all entries cached → no download.
	if prevCRC != 0 && prevCRC == bundle.BundleCRC && h.allEntriesCached(bundle.Entries) {
		h.mu.Lock()
		h.progress.Done = len(bundle.Entries)
		h.mu.Unlock()
		return nil
	}

	// Phase 1: GitHub bundle (one fetch, all entries).
	missing := bundle.Entries
	hasGitHub := bundle.HasRelay(protocol.RelayGitHub) && viaGitHub != nil && bundle.BundleSize > 0
	if hasGitHub {
		body, ghErr := viaGitHub(ctx, int64(bundle.BundleSize), bundle.BundleCRC)
		if ghErr != nil {
			h.setProgressErr(fmt.Sprintf("github bundle: %v", ghErr))
		} else if body != nil {
			if uint32(len(body)) == bundle.BundleSize && crc32.ChecksumIEEE(body) == bundle.BundleCRC {
				missing = h.persistFromBundle(ctx, body, bundle.Entries, onStored)
				h.mu.Lock()
				h.bundleCRC = bundle.BundleCRC
				h.saveIndexLocked()
				h.mu.Unlock()
			} else {
				h.setProgressErr(fmt.Sprintf("github bundle size/crc mismatch: have %d/%08x want %d/%08x",
					len(body), crc32.ChecksumIEEE(body), bundle.BundleSize, bundle.BundleCRC))
			}
		}
	}

	// Phase 2: per-entry DNS for whatever the bundle didn't cover.
	if dnsAllowed && len(missing) > 0 {
		h.fetchMissingViaDNS(ctx, fetcher, missing, onStored)
	}
	return nil
}

// persistFromBundle slices each entry out, verifies, writes to disk.
// Returns the entries that didn't land (so the DNS phase can retry).
func (h *profilePicsHub) persistFromBundle(ctx context.Context, body []byte, entries []client.ProfilePicEntry, onStored storedCallback) []client.ProfilePicEntry {
	missing := make([]client.ProfilePicEntry, 0, len(entries))
	for _, entry := range entries {
		if ctx.Err() != nil {
			return missing
		}
		h.markProgressUsername(entry.Username)
		if h.HasFresh(entry.Username, entry.CRC) {
			h.bumpProgress(true)
			continue
		}
		slice, err := protocol.VerifyEntry(body, protocol.ProfilePicEntry{
			Username:   entry.Username,
			Offset:     entry.Offset,
			Size:       entry.Size,
			CRC:        entry.CRC,
			MIME:       entry.MIME,
			DNSChannel: entry.DNSChannel,
			DNSBlocks:  entry.DNSBlocks,
		})
		if err != nil {
			missing = append(missing, entry)
			continue
		}
		if err := h.Store(entry.Username, slice, entry.CRC, entry.MIME); err != nil {
			missing = append(missing, entry)
			continue
		}
		h.bumpProgress(true)
		if onStored != nil {
			onStored(entry.Username)
		}
	}
	return missing
}

// fetchMissingViaDNS fetches each entry on its own DNS channel,
// verifies, persists. Per-entry independent: one failure doesn't
// affect the others.
func (h *profilePicsHub) fetchMissingViaDNS(ctx context.Context, fetcher *client.Fetcher, entries []client.ProfilePicEntry, onStored storedCallback) {
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		h.markProgressUsername(entry.Username)
		if entry.DNSChannel == 0 || entry.DNSBlocks == 0 {
			h.bumpProgress(false)
			continue
		}
		// HasFresh is checked again here — a previous Phase-1 success
		// could already have populated this entry from another path.
		if h.HasFresh(entry.Username, entry.CRC) {
			h.bumpProgress(true)
			continue
		}
		body, err := fetcher.FetchMedia(ctx, entry.DNSChannel, entry.DNSBlocks, entry.CRC, nil)
		if err != nil {
			h.bumpProgress(false)
			continue
		}
		if uint32(len(body)) != entry.Size {
			h.bumpProgress(false)
			continue
		}
		if crc32.ChecksumIEEE(body) != entry.CRC {
			h.bumpProgress(false)
			continue
		}
		if err := h.Store(entry.Username, body, entry.CRC, entry.MIME); err != nil {
			h.bumpProgress(false)
			continue
		}
		h.bumpProgress(true)
		if onStored != nil {
			onStored(entry.Username)
		}
	}
}

// HasFresh: cached pic with this CRC is already on disk.
func (h *profilePicsHub) HasFresh(username string, crc uint32) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.index[strings.ToLower(username)]
	if !ok || e.CRC != crc {
		return false
	}
	_, err := os.Stat(h.imagePath(username, e.Extension))
	return err == nil
}

func (h *profilePicsHub) allEntriesCached(entries []client.ProfilePicEntry) bool {
	for _, e := range entries {
		if !h.HasFresh(e.Username, e.CRC) {
			return false
		}
	}
	return true
}

func (h *profilePicsHub) markProgressUsername(username string) {
	h.mu.Lock()
	h.progress.Username = username
	h.mu.Unlock()
}

func (h *profilePicsHub) bumpProgress(ok bool) {
	h.mu.Lock()
	h.progress.Done++
	if !ok {
		h.progress.Failed++
	}
	h.mu.Unlock()
}

func (h *profilePicsHub) setProgressErr(msg string) {
	h.mu.Lock()
	h.progress.Error = msg
	h.mu.Unlock()
}

// ===== HTTP handlers =====

// handleProfilePic serves /api/profile-pics/<key>. Key is "<handle>"
// or "x:<handle>". 404 → front-end falls back to the letter avatar.
func (h *profilePicsHub) handleProfilePic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/api/profile-pics/")
	key = strings.TrimSpace(strings.Trim(key, "/"))
	if key == "" || !isValidProfilePicKey(key) {
		http.Error(w, "missing username", 400)
		return
	}
	body, ctype, err := h.Get(key)
	if err != nil {
		http.Error(w, "not cached", 404)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	_, _ = w.Write(body)
}

// isValidProfilePicKey: "<handle>" or "<type>:<handle>", alphanumeric +
// _-. Bars slashes/back-slashes/dots so the key can't escape the cache dir.
func isValidProfilePicKey(s string) bool {
	if strings.ContainsAny(s, "/\\.") {
		return false
	}
	parts := strings.Split(s, ":")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if !(r == '_' || r == '-' || (r >= '0' && r <= '9') ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
		}
	}
	return true
}

// handleProfilePicsRefresh kicks off a background refresh and returns
// immediately; UI polls /api/profile-pics/progress for the progress bar.
func (s *Server) handleProfilePicsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.mu.RLock()
	fetcher := s.fetcher
	hub := s.profilePics
	s.mu.RUnlock()
	if fetcher == nil || hub == nil {
		http.Error(w, "fetcher not ready", 503)
		return
	}
	dnsAllowed := s.profilePicsEnabled()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		// SSE per stored avatar so the UI updates mid-batch.
		onStored := func(string) {
			s.broadcast("event: update\ndata: \"profile-pics\"\n\n")
		}
		if err := hub.refresh(ctx, fetcher, dnsAllowed, s.fetchFromGitHubRelayBytes, onStored); err == nil {
			s.broadcast("event: update\ndata: \"profile-pics\"\n\n")
		}
	}()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleProfilePicsProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	hub := s.profilePics
	if hub == nil {
		writeJSON(w, profilePicProgress{})
		return
	}
	writeJSON(w, hub.Progress())
}

// handleProfilePicsList returns which usernames have a cached avatar.
// Cache-Control: no-store so SSE-driven reloads see fresh data.
func (s *Server) handleProfilePicsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	hub := s.profilePics
	if hub == nil {
		writeJSON(w, map[string]any{"enabled": false, "users": []string{}})
		return
	}
	hub.mu.Lock()
	users := make([]string, 0, len(hub.index))
	for u := range hub.index {
		users = append(users, u)
	}
	hub.mu.Unlock()
	writeJSON(w, map[string]any{
		"enabled": s.profilePicsEnabled(),
		"users":   users,
	})
}

func (s *Server) profilePicsEnabled() bool {
	pl, err := s.loadProfiles()
	if err != nil || pl == nil {
		return false
	}
	return pl.ProfilePicsEnabled
}

// ===== mime helpers =====

func extensionFor(mime uint8) string {
	switch mime {
	case 1:
		return ".png"
	case 2:
		return ".webp"
	default:
		return ".jpg"
	}
}

func contentTypeFor(mime uint8) string {
	switch mime {
	case 1:
		return "image/png"
	case 2:
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
