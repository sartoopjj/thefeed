package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// runMediaCacheSweep evicts expired media-cache entries every hour for the
// lifetime of the process.
func (s *Server) runMediaCacheSweep() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		if s.mediaCache == nil {
			return
		}
		s.mediaCache.Cleanup()
	}
}

const defaultCacheBudgetMB = 1024 // 1 GB

// applyCacheBudget splits the budget (in MB) across media-cache and telemirror
// images. 0 = default (1 GB). saved-media is exempt (never evicted).
func (s *Server) applyCacheBudget(mb int) {
	if mb <= 0 {
		mb = defaultCacheBudgetMB
	}
	total := int64(mb) << 20
	// 70% media-cache, 30% telemirror images.
	mediaBudget := total * 70 / 100
	tmBudget := total - mediaBudget
	if s.mediaCache != nil {
		s.mediaCache.SetMaxBytes(mediaBudget)
	}
	if s.telemirror != nil && s.telemirror.imgs != nil {
		s.telemirror.imgs.SetMaxBytes(tmBudget)
	}
}

// handleClearCache wipes both the per-channel message cache and the
// downloaded-media disk cache.
func (s *Server) handleClearCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	deleted := 0
	cacheDir := filepath.Join(s.dataDir, "cache")
	if entries, err := os.ReadDir(cacheDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if os.Remove(filepath.Join(cacheDir, e.Name())) == nil {
				deleted++
			}
		}
	}
	if s.telemirror != nil {
		s.telemirror.ClearCache()
	}
	if s.profilePics != nil {
		s.profilePics.Clear()
	}
	// Conversations go with everything else, but the session stays: "clear
	// cache" must never mean "sign out".
	if s.tel != nil {
		s.tel.store().Forget()
	}
	mediaDeleted := 0
	if s.mediaCache != nil {
		mediaDeleted = s.mediaCache.Clear()
	}
	_ = os.Remove(s.channelsCachePath())
	// Reset in-memory message state too so refreshChannel's "no changes"
	// guard doesn't skip the next fetch (prev IDs match what's on the
	// server, but our cache is gone).
	s.mu.Lock()
	s.messages = make(map[int][]protocol.Message)
	s.lastMsgIDs = make(map[int]uint32)
	s.lastHashes = make(map[int]uint32)
	s.mu.Unlock()
	s.addLog(fmt.Sprintf("Cache cleared: %d message files, %d media files", deleted, mediaDeleted))
	writeJSON(w, map[string]any{"ok": true, "deleted": deleted, "mediaDeleted": mediaDeleted})
}

// handleCacheStats returns per-cache sizes and the current budget.
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	pl, _ := s.loadProfiles()
	budgetMB := defaultCacheBudgetMB
	if pl != nil && pl.CacheBudgetMB > 0 {
		budgetMB = pl.CacheBudgetMB
	}
	type cacheEntry struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Bytes int64  `json:"bytes"`
	}
	caches := []cacheEntry{}
	var total int64
	if s.mediaCache != nil {
		sz := s.mediaCache.Size()
		caches = append(caches, cacheEntry{ID: "media", Label: "Media", Bytes: sz})
		total += sz
	}
	if s.telemirror != nil && s.telemirror.imgs != nil {
		sz := s.telemirror.imgs.Size()
		caches = append(caches, cacheEntry{ID: "telemirror", Label: "TeleMirror Images", Bytes: sz})
		total += sz
	}
	msgDir := filepath.Join(s.dataDir, "cache")
	msgSz := dirSize(msgDir)
	caches = append(caches, cacheEntry{ID: "messages", Label: "Messages", Bytes: msgSz})
	total += msgSz
	if s.savedMedia != nil {
		sz := s.savedMedia.Size()
		caches = append(caches, cacheEntry{ID: "saved-media", Label: "Saved Media", Bytes: sz})
		total += sz
	}
	// Its own row: these are conversations, not media, and clearing them costs
	// the queries to fetch them again -- which on this transport is a real
	// price the user should be able to weigh separately.
	if s.tel != nil {
		sz := fileSize(telCachePath(s.tel.dir))
		caches = append(caches, cacheEntry{ID: "telegram", Label: "Telegram Messages", Bytes: sz})
		total += sz
	}
	writeJSON(w, map[string]any{
		"total":    total,
		"budgetMB": budgetMB,
		"caches":   caches,
	})
}

// fileSize is one file's size, or zero when it is not there.
func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// handleCacheClearOne clears a single cache by id.
func (s *Server) handleCacheClearOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Which string `json:"which"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Which == "" {
		http.Error(w, "bad request", 400)
		return
	}
	switch req.Which {
	case "media":
		if s.mediaCache != nil {
			s.mediaCache.Clear()
		}
	case "telemirror":
		if s.telemirror != nil {
			s.telemirror.ClearCache()
		}
	case "telegram":
		// The cache only. The session is NOT touched: clearing storage must
		// never sign anyone out.
		if s.tel != nil {
			s.tel.store().Forget()
		}
	case "messages":
		cacheDir := filepath.Join(s.dataDir, "cache")
		if entries, err := os.ReadDir(cacheDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					_ = os.Remove(filepath.Join(cacheDir, e.Name()))
				}
			}
		}
		_ = os.Remove(s.channelsCachePath())
		s.mu.Lock()
		s.messages = make(map[int][]protocol.Message)
		s.lastMsgIDs = make(map[int]uint32)
		s.lastHashes = make(map[int]uint32)
		s.mu.Unlock()
	default:
		http.Error(w, "unknown cache id", 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleCacheBudget sets the cache budget in MB and persists it.
func (s *Server) handleCacheBudget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		MB int `json:"mb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MB < 0 {
		http.Error(w, "bad request", 400)
		return
	}
	s.profilesMu.Lock()
	defer s.profilesMu.Unlock()
	pl, err := s.loadProfilesExisting()
	if err != nil {
		http.Error(w, fmt.Sprintf("load: %v", err), 500)
		return
	}
	if pl == nil {
		pl = &ProfileList{}
	}
	pl.CacheBudgetMB = req.MB
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, fmt.Sprintf("save: %v", err), 500)
		return
	}
	s.applyCacheBudget(req.MB)
	writeJSON(w, map[string]any{"ok": true, "budgetMB": req.MB})
}

// dirSize returns the total size of regular files in a directory (non-recursive).
func dirSize(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}
