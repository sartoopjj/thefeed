package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sartoopjj/thefeed/internal/update"
	"github.com/sartoopjj/thefeed/internal/version"
)

// handleSettings manages user preferences (font size etc.).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pl, _ := s.loadProfiles()
		if pl == nil {
			pl = &ProfileList{}
		}
		qm, rl, sc, to := connectionSettings(pl)
		// Gather pinned channels from the active profile.
		var pinnedCh []string
		if pl.Active != "" {
			for _, p := range pl.Profiles {
				if p.ID == pl.Active {
					pinnedCh = p.PinnedChannels
					break
				}
			}
		}
		if pinnedCh == nil {
			pinnedCh = []string{}
		}
		writeJSON(w, map[string]any{
			"fontSize":           pl.FontSize,
			"debug":              pl.Debug,
			"theme":              pl.Theme,
			"lang":               pl.Lang,
			"scanPromptOff":      pl.ScanPromptOff,
			"mirrorNoteOff":      pl.MirrorNoteOff,
			"telIntroSeen":       pl.TelIntroSeen,
			"profilePicsEnabled": pl.ProfilePicsEnabled,
			"skipUpdateVersion":  pl.SkipUpdateVersion,
			"downloadedVersion":  pl.DownloadedVersion,
			"migrationVersion":   pl.MigrationVersion,
			// Multi-user deployment: the frontend keeps per-browser state
			// (seen markers, migration level) local instead of in the shared
			// profiles.json, and never writes global settings on its own.
			"shared": s.sharedBackend,
			// Lets the frontend hide its update entry points.
			"storeBuild":         update.StoreBuild(),
			"queryMode":          qm,
			"rateLimit":          rl,
			"scatter":            sc,
			"timeout":            to,
			"resolverCacheShare": pl.ShareEnabled(),
			"pinnedChannels":     pinnedCh,
			"version":            version.Version,
			"commit":             version.Commit,
		})

	case http.MethodPost:
		// Optional pointers so partial requests don't reset other fields.
		var req struct {
			FontSize           *int     `json:"fontSize"`
			Debug              *bool    `json:"debug"`
			Theme              *string  `json:"theme"`
			Lang               *string  `json:"lang"`
			ScanPromptOff      *bool    `json:"scanPromptOff"`
			MirrorNoteOff      *bool    `json:"mirrorNoteOff"`
			TelIntroSeen       *bool    `json:"telIntroSeen"`
			ProfilePicsEnabled *bool    `json:"profilePicsEnabled"`
			SkipUpdateVersion  *string  `json:"skipUpdateVersion"`
			DownloadedVersion  *string  `json:"downloadedVersion"`
			MigrationVersion   *int     `json:"migrationVersion"`
			QueryMode          *string  `json:"queryMode"`
			RateLimit          *float64 `json:"rateLimit"`
			Scatter            *int     `json:"scatter"`
			Timeout            *float64 `json:"timeout"`
			ResolverCacheShare *bool    `json:"resolverCacheShare"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
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
		if req.FontSize != nil {
			fs := *req.FontSize
			if fs < 10 {
				fs = 0
			}
			if fs > 24 {
				fs = 24
			}
			pl.FontSize = fs
		}
		if req.Debug != nil {
			pl.Debug = *req.Debug
		}
		if req.Theme != nil && (*req.Theme == "dark" || *req.Theme == "light" || *req.Theme == "system") {
			pl.Theme = *req.Theme
		}
		if req.Lang != nil && (*req.Lang == "fa" || *req.Lang == "en") {
			pl.Lang = *req.Lang
		}
		if req.MirrorNoteOff != nil {
			pl.MirrorNoteOff = *req.MirrorNoteOff
		}
		if req.TelIntroSeen != nil {
			pl.TelIntroSeen = *req.TelIntroSeen
		}
		if req.ScanPromptOff != nil {
			pl.ScanPromptOff = *req.ScanPromptOff
		}
		if req.ProfilePicsEnabled != nil {
			pl.ProfilePicsEnabled = *req.ProfilePicsEnabled
		}
		if req.SkipUpdateVersion != nil {
			pl.SkipUpdateVersion = *req.SkipUpdateVersion
		}
		if req.DownloadedVersion != nil {
			pl.DownloadedVersion = *req.DownloadedVersion
		}
		// Monotonic: a stale client must never walk the level backwards and
		// make already-applied migrations run again.
		if req.MigrationVersion != nil && *req.MigrationVersion > pl.MigrationVersion {
			pl.MigrationVersion = *req.MigrationVersion
		}
		if req.QueryMode != nil && (*req.QueryMode == "single" || *req.QueryMode == "double") {
			pl.QueryMode = *req.QueryMode
		}
		if req.RateLimit != nil && *req.RateLimit >= 0 {
			pl.RateLimit = *req.RateLimit
		}
		if req.Scatter != nil && *req.Scatter >= 0 {
			pl.Scatter = *req.Scatter
		}
		if req.Timeout != nil && *req.Timeout >= 0 {
			pl.Timeout = *req.Timeout
		}
		if req.ResolverCacheShare != nil {
			v := *req.ResolverCacheShare
			pl.ResolverCacheShare = &v
		}
		if err := s.saveProfiles(pl); err != nil {
			http.Error(w, fmt.Sprintf("save: %v", err), 500)
			return
		}
		// Apply debug to the current fetcher session immediately.
		if req.Debug != nil {
			s.mu.RLock()
			f := s.fetcher
			s.mu.RUnlock()
			if f != nil {
				f.SetDebug(*req.Debug)
			}
			s.scanner.SetDebug(*req.Debug)
		}
		// Same for shared-cache toggle — apply to the running fetcher so
		// the user sees the change without having to restart anything.
		if req.ResolverCacheShare != nil {
			s.mu.RLock()
			f := s.fetcher
			s.mu.RUnlock()
			if f != nil {
				f.SetCacheShare(pl.ShareEnabled())
			}
		}
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleBgImage(w http.ResponseWriter, r *http.Request) {
	bgPath := filepath.Join(s.dataDir, "bg_image")
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(bgPath)
		if err != nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(204)
			return
		}
		// Detect content type from file data.
		ct := http.DetectContentType(data)
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(data)

	case http.MethodPost:
		// Limit upload to 10 MB.
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "file too large (max 10MB)", 413)
			return
		}
		ct := http.DetectContentType(data)
		if !strings.HasPrefix(ct, "image/") {
			http.Error(w, "not an image", 400)
			return
		}
		if err := os.WriteFile(bgPath, data, 0600); err != nil {
			http.Error(w, fmt.Sprintf("save: %v", err), 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true})

	case http.MethodDelete:
		os.Remove(bgPath)
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "method not allowed", 405)
	}
}
