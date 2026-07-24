package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// handleProfiles manages CRUD for config profiles.
// GET: returns profile list. POST: create/update/delete profiles.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.profilesMu.Lock()
		pl, err := s.loadProfilesExisting()
		if err != nil {
			s.profilesMu.Unlock()
			http.Error(w, fmt.Sprintf("load: %v", err), 500)
			return
		}
		if pl == nil {
			// First-run migration from config.json.
			pl = &ProfileList{}
			if s.config != nil {
				p := Profile{
					ID:       generateID(),
					Nickname: s.config.Domain,
					Config:   *s.config,
				}
				pl.Profiles = []Profile{p}
				pl.Active = p.ID
				_ = s.saveProfiles(pl)
			}
		}
		s.profilesMu.Unlock()
		writeJSON(w, pl)

	case http.MethodPost:
		var req struct {
			Action    string   `json:"action"` // "create", "update", "delete", "reorder"
			Profile   Profile  `json:"profile"`
			Order     []string `json:"order"` // for reorder
			SkipCheck bool     `json:"skipCheck"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		s.profilesMu.Lock()
		pl, err := s.loadProfilesExisting()
		if err != nil {
			s.profilesMu.Unlock()
			http.Error(w, fmt.Sprintf("load: %v", err), 500)
			return
		}
		if pl == nil {
			pl = &ProfileList{}
		}

		needsReinit := false

		switch req.Action {
		case "create":
			// Re-importing an identical config (same domain + key) must not
			// duplicate the profile — repeated imports of the same URI or
			// default config are common when the first click looks like it
			// did nothing. Resolvers still merge into the shared bank below.
			dup := false
			for _, p := range pl.Profiles {
				if p.Config.Domain == req.Profile.Config.Domain && p.Config.Key == req.Profile.Config.Key {
					dup = true
					break
				}
			}
			// Move resolvers to the shared bank.
			if len(req.Profile.Config.Resolvers) > 0 {
				addToBank(pl, req.Profile.Config.Resolvers)
				req.Profile.Config.Resolvers = nil
			}
			if !dup {
				req.Profile.ID = generateID()
				if req.Profile.Nickname == "" {
					req.Profile.Nickname = req.Profile.Config.Domain
				}
				pl.Profiles = append(pl.Profiles, req.Profile)
				if len(pl.Profiles) == 1 {
					pl.Active = req.Profile.ID
					needsReinit = true
				}
			}

		case "update":
			for i, p := range pl.Profiles {
				if p.ID == req.Profile.ID {
					// Move resolvers to the shared bank.
					if len(req.Profile.Config.Resolvers) > 0 {
						addToBank(pl, req.Profile.Config.Resolvers)
						req.Profile.Config.Resolvers = nil
					}
					// Carry over fields the edit-profile UI doesn't manage so
					// they don't get wiped on save (auto-update list etc.).
					req.Profile.AutoUpdate = p.AutoUpdate
					req.Profile.AutoUpdateInterval = p.AutoUpdateInterval
					req.Profile.PinnedChannels = p.PinnedChannels
					pl.Profiles[i] = req.Profile
					if p.ID == pl.Active {
						needsReinit = true
					}
					break
				}
			}

		case "delete":
			for i, p := range pl.Profiles {
				if p.ID == req.Profile.ID {
					pl.Profiles = append(pl.Profiles[:i], pl.Profiles[i+1:]...)
					if pl.Active == req.Profile.ID {
						pl.Active = ""
						if len(pl.Profiles) > 0 {
							pl.Active = pl.Profiles[0].ID
							needsReinit = true
						}
					}
					s.dropChannelsCacheEntry(req.Profile.ID)
					break
				}
			}

		case "reorder":
			if len(req.Order) > 0 {
				ordered := make([]Profile, 0, len(pl.Profiles))
				byID := make(map[string]Profile)
				for _, p := range pl.Profiles {
					byID[p.ID] = p
				}
				for _, id := range req.Order {
					if p, ok := byID[id]; ok {
						ordered = append(ordered, p)
					}
				}
				pl.Profiles = ordered
			}

		default:
			s.profilesMu.Unlock()
			http.Error(w, "unknown action", 400)
			return
		}

		saveErr := s.saveProfiles(pl)
		var activeConfig *Config
		if needsReinit && pl.Active != "" {
			for _, p := range pl.Profiles {
				if p.ID == pl.Active {
					cfg := p.Config
					activeConfig = &cfg
					break
				}
			}
		}
		s.profilesMu.Unlock()

		if saveErr != nil {
			http.Error(w, fmt.Sprintf("save profiles: %v", saveErr), 500)
			return
		}

		// initFetcher takes s.mu — call it OUTSIDE profilesMu so handlers
		// that need both don't AB-BA against it.
		if activeConfig != nil {
			_ = s.saveConfig(activeConfig)
			s.mu.Lock()
			s.config = activeConfig
			s.mu.Unlock()
			// Snapshot live resolvers before initFetcher discards the fetcher, so
			// "skip" can reuse them rather than rescanning (see switch below).
			s.mu.RLock()
			var liveResolvers []string
			if s.fetcher != nil {
				liveResolvers = s.fetcher.Resolvers()
			}
			s.mu.RUnlock()
			if err := s.initFetcher(); err != nil {
				log.Printf("[web] re-init fetcher after profile change: %v", err)
			} else {
				// Populate active resolvers from the user's selected list right
				// away so any profile change (edit / delete / first-create)
				// never leaves the fetcher with "no active resolvers" until the
				// resolver panel is opened or a scan finishes. Resolvers are
				// shared and server-agnostic, so reusing them is correct.
				applied := s.applySelectedList()
				switch {
				case req.SkipCheck && applied:
					s.mu.RLock()
					checker := s.checker
					ctx := s.fetcherCtx
					s.mu.RUnlock()
					if checker != nil && ctx != nil {
						checker.StartPeriodic(ctx)
					}
					go s.refreshMetadataOnly()
				case req.SkipCheck:
					s.skipCheckerUseSaved(liveResolvers)
				default:
					// Full health-check; active is already populated above so
					// the feed keeps working during the scan.
					s.startCheckerThenRefresh()
				}
			}
		}

		s.broadcast("event: update\ndata: \"profiles\"\n\n")
		writeJSON(w, map[string]any{"ok": true, "profiles": pl})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleProfileSwitch switches the active profile and re-initializes the fetcher.
func (s *Server) handleProfileSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID        string `json:"id"`
		SkipCheck bool   `json:"skipCheck"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	// Snapshot the currently-active resolvers BEFORE we tear the fetcher down.
	// On "skip" we reuse exactly these (see below) so switching config never
	// silently rescans — initFetcher() builds a fresh, empty fetcher, and
	// SelectedList/last_scan aren't always populated even when resolvers are live
	// (which is precisely when the "skip rescan?" prompt appears).
	s.mu.RLock()
	var liveResolvers []string
	if s.fetcher != nil {
		liveResolvers = s.fetcher.Resolvers()
	}
	s.mu.RUnlock()
	s.profilesMu.Lock()
	pl, err := s.loadProfiles()
	if err != nil || pl == nil {
		s.profilesMu.Unlock()
		http.Error(w, "no profiles", 400)
		return
	}
	var found *Profile
	for i, p := range pl.Profiles {
		if p.ID == req.ID {
			found = &pl.Profiles[i]
			break
		}
	}
	if found == nil {
		s.profilesMu.Unlock()
		http.Error(w, "profile not found", 404)
		return
	}
	pl.Active = found.ID
	saveErr := s.saveProfiles(pl)
	s.profilesMu.Unlock()
	if err := saveErr; err != nil {
		http.Error(w, fmt.Sprintf("save: %v", err), 500)
		return
	}
	if err := s.saveConfig(&found.Config); err != nil {
		http.Error(w, fmt.Sprintf("save config: %v", err), 500)
		return
	}

	// Reset state and seed channels from the new profile's cache (if any).
	cc := s.loadChannelsCache()
	s.mu.Lock()
	s.config = &found.Config
	if cc != nil {
		s.channels = cc.Channels
		s.nextFetch = cc.NextFetch
	} else {
		s.channels = nil
	}
	s.messages = make(map[int][]protocol.Message)
	if s.relayInfo != nil {
		s.relayInfo.invalidate()
	}
	s.lastMsgIDs = make(map[int]uint32)
	s.lastHashes = make(map[int]uint32)
	s.mu.Unlock()
	// Tell every connected client (other tabs / devices) that the active
	// profile changed so they refresh their UI instead of pointing at the
	// old one.
	s.broadcast("event: update\ndata: \"profiles\"\n\n")
	if cc != nil {
		s.broadcast("event: update\ndata: \"channels\"\n\n")
	}

	if err := s.initFetcher(); err != nil {
		http.Error(w, fmt.Sprintf("init fetcher: %v", err), 500)
		return
	}
	// Skip path: selected list → live → last scan → bank → (only then) scan.
	if req.SkipCheck && s.applySelectedList() {
		s.mu.RLock()
		checker := s.checker
		ctx := s.fetcherCtx
		s.mu.RUnlock()
		if checker != nil && ctx != nil {
			checker.StartPeriodic(ctx)
		}
		go s.refreshMetadataOnly()
	} else if req.SkipCheck {
		s.skipCheckerUseSaved(liveResolvers)
	} else {
		s.startCheckerThenRefresh()
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleAutoUpdate exposes the active profile's auto-update channel list.
// GET → {channels, intervalSeconds, defaultIntervalSeconds}.
// POST {channels, intervalSeconds?} replaces both. Names are stripped and
// dedup'd before saving.
func (s *Server) handleAutoUpdate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pl, _ := s.loadProfiles()
		channels := []string{}
		interval := 0
		if pl != nil && pl.Active != "" {
			for _, p := range pl.Profiles {
				if p.ID == pl.Active {
					if p.AutoUpdate != nil {
						channels = p.AutoUpdate
					}
					interval = p.AutoUpdateInterval
					break
				}
			}
		}
		writeJSON(w, map[string]any{
			"channels":               channels,
			"intervalSeconds":        interval,
			"defaultIntervalSeconds": int(minAutoUpdateInterval / time.Second),
		})

	case http.MethodPost:
		var req struct {
			Channels        []string `json:"channels"`
			IntervalSeconds *int     `json:"intervalSeconds,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		pl, err := s.loadProfiles()
		if err != nil || pl == nil || pl.Active == "" {
			http.Error(w, "no active profile", 400)
			return
		}
		idx := -1
		for i, p := range pl.Profiles {
			if p.ID == pl.Active {
				idx = i
				break
			}
		}
		if idx < 0 {
			http.Error(w, "active profile not found", 400)
			return
		}
		pl.Profiles[idx].AutoUpdate = normaliseAutoUpdateList(req.Channels)
		if req.IntervalSeconds != nil {
			v := *req.IntervalSeconds
			if v < 0 {
				v = 0
			}
			minSec := int(minAutoUpdateInterval / time.Second)
			if v > 0 && v < minSec {
				v = minSec // floor: never poll faster than the server fetches
			}
			pl.Profiles[idx].AutoUpdateInterval = v
		}
		if err := s.saveProfiles(pl); err != nil {
			http.Error(w, fmt.Sprintf("save: %v", err), 500)
			return
		}
		writeJSON(w, map[string]any{
			"ok":              true,
			"channels":        pl.Profiles[idx].AutoUpdate,
			"intervalSeconds": pl.Profiles[idx].AutoUpdateInterval,
		})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleAutoUpdateToggle flips one channel's membership. Body {channel}.
// Returns {enabled, channels}.
func (s *Server) handleAutoUpdateToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	name := strings.TrimPrefix(strings.TrimSpace(req.Channel), "@")
	if name == "" {
		http.Error(w, "channel required", 400)
		return
	}
	pl, err := s.loadProfiles()
	if err != nil || pl == nil || pl.Active == "" {
		http.Error(w, "no active profile", 400)
		return
	}
	idx := -1
	for i, p := range pl.Profiles {
		if p.ID == pl.Active {
			idx = i
			break
		}
	}
	if idx < 0 {
		http.Error(w, "active profile not found", 400)
		return
	}
	current := pl.Profiles[idx].AutoUpdate
	on := false
	hit := -1
	for i, n := range current {
		if strings.TrimPrefix(strings.TrimSpace(n), "@") == name {
			hit = i
			break
		}
	}
	if hit >= 0 {
		current = append(current[:hit], current[hit+1:]...)
	} else {
		current = append(current, name)
		on = true
	}
	pl.Profiles[idx].AutoUpdate = normaliseAutoUpdateList(current)
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, fmt.Sprintf("save: %v", err), 500)
		return
	}
	writeJSON(w, map[string]any{
		"ok":       true,
		"channel":  name,
		"enabled":  on,
		"channels": pl.Profiles[idx].AutoUpdate,
	})
}

// normaliseAutoUpdateList strips @ + whitespace, drops empties, dedupes
// while preserving order.
func normaliseAutoUpdateList(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.TrimPrefix(strings.TrimSpace(raw), "@")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// handlePinnedChannelToggle flips one channel's pin membership. Body {channel}.
// Returns {pinned, channel, channels}.
func (s *Server) handlePinnedChannelToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	name := strings.TrimPrefix(strings.TrimSpace(req.Channel), "@")
	if name == "" {
		http.Error(w, "channel required", 400)
		return
	}
	s.profilesMu.Lock()
	defer s.profilesMu.Unlock()
	pl, err := s.loadProfilesExisting()
	if err != nil {
		http.Error(w, fmt.Sprintf("load: %v", err), 500)
		return
	}
	if pl == nil || pl.Active == "" {
		http.Error(w, "no active profile", 400)
		return
	}
	idx := -1
	for i, p := range pl.Profiles {
		if p.ID == pl.Active {
			idx = i
			break
		}
	}
	if idx < 0 {
		http.Error(w, "active profile not found", 400)
		return
	}
	current := pl.Profiles[idx].PinnedChannels
	pinned := false
	hit := -1
	for i, n := range current {
		if strings.TrimPrefix(strings.TrimSpace(n), "@") == name {
			hit = i
			break
		}
	}
	if hit >= 0 {
		current = append(current[:hit], current[hit+1:]...)
	} else {
		current = append(current, name)
		pinned = true
	}
	pl.Profiles[idx].PinnedChannels = normalisePinnedList(current)
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, fmt.Sprintf("save: %v", err), 500)
		return
	}
	writeJSON(w, map[string]any{
		"ok":       true,
		"channel":  name,
		"pinned":   pinned,
		"channels": pl.Profiles[idx].PinnedChannels,
	})
}

// normalisePinnedList strips @ + whitespace, drops empties, dedupes
// while preserving order.
func normalisePinnedList(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.TrimPrefix(strings.TrimSpace(raw), "@")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
