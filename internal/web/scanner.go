package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
)

func (s *Server) handleScannerPresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	type preset struct {
		Name  string `json:"name"`
		Label string `json:"label"`
		Count int    `json:"count"`
	}
	writeJSON(w, map[string]any{
		"presets": []preset{
			{Name: "ir", Label: "Iran", Count: parseScannerPresetCount()},
		},
	})
}

// parseScannerPresetLines returns the parsed non-empty, non-comment lines from the preset.
func parseScannerPresetLines() []string {
	var lines []string
	for _, line := range strings.Split(defaultScannerPresets, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseScannerPresetCount() int {
	return len(parseScannerPresetLines())
}

func (s *Server) handleScannerStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	var req struct {
		Targets      []string `json:"targets"`
		Preset       string   `json:"preset"` // e.g. "ir" — server-side preset, avoids sending 50K IPs
		MaxIPs       int      `json:"maxIPs"`
		RateLimit    int      `json:"rateLimit"`
		Timeout      float64  `json:"timeout"`
		ExpandSubnet bool     `json:"expandSubnet"`
		QueryMode    string   `json:"queryMode"`
		ProfileID    string   `json:"profileId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	// Resolve preset into targets server-side.
	if req.Preset == "ir" && len(req.Targets) == 0 {
		req.Targets = parseScannerPresetLines()
	}

	if len(req.Targets) == 0 {
		http.Error(w, "targets required", 400)
		return
	}

	// Resolve profile config.
	pl, err := s.loadProfiles()
	if err != nil || pl == nil {
		http.Error(w, "no profiles configured", 400)
		return
	}

	var profileCfg *Config
	if req.ProfileID != "" {
		for _, p := range pl.Profiles {
			if p.ID == req.ProfileID {
				profileCfg = &p.Config
				break
			}
		}
	}
	if profileCfg == nil {
		// Fall back to active profile.
		for _, p := range pl.Profiles {
			if p.ID == pl.Active {
				profileCfg = &p.Config
				break
			}
		}
	}
	if profileCfg == nil {
		http.Error(w, "no profile found", 400)
		return
	}

	if profileCfg.Domain == "" || profileCfg.Key == "" {
		http.Error(w, "profile missing domain or passphrase", 400)
		return
	}

	queryMode := req.QueryMode
	if queryMode == "" {
		queryMode = profileCfg.QueryMode
	}

	cfg := client.ScannerConfig{
		Targets:      req.Targets,
		MaxIPs:       req.MaxIPs,
		RateLimit:    req.RateLimit,
		Timeout:      req.Timeout,
		ExpandSubnet: req.ExpandSubnet,
		QueryMode:    queryMode,
		Domain:       profileCfg.Domain,
		Passphrase:   profileCfg.Key,
	}

	// Cancel any in-progress resolver checker scan to avoid resource
	// contention (both the checker and scanner do DNS probes).
	s.mu.RLock()
	checker := s.checker
	s.mu.RUnlock()
	if checker != nil {
		checker.CancelCurrentScan()
	}

	s.scanner.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})

	if err := s.scanner.Start(cfg); err != nil {
		http.Error(w, fmt.Sprintf("start scanner: %v", err), 400)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleScannerStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.scanner.Stop()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleScannerPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.scanner.Pause()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleScannerResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.scanner.Resume()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleScannerProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	prog := s.scanner.Progress()
	writeJSON(w, prog)
}

func (s *Server) handleScannerApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	var req struct {
		Resolvers []string `json:"resolvers"`
		Mode      string   `json:"mode"` // "append" or "overwrite"
		ProfileID string   `json:"profileId"`
		// ListName picks which named active-list receives these
		// resolvers. Empty → currently-selected list, or "Default"
		// for legacy installs that haven't been migrated yet.
		ListName string `json:"listName,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	// If no resolvers explicitly provided, pull them from scanner results.
	resolvers := req.Resolvers
	if len(resolvers) == 0 {
		prog := s.scanner.Progress()
		for _, r := range prog.Results {
			resolvers = append(resolvers, r.IP)
		}
	}
	if len(resolvers) == 0 {
		http.Error(w, "no resolvers to apply", 400)
		return
	}

	// Make sure resolvers have :53 suffix.
	for i, r := range resolvers {
		if !strings.Contains(r, ":") {
			resolvers[i] = r + ":53"
		}
	}

	// Determine which profile to apply to (for logging purposes / active check).
	pl, _ := s.loadProfiles()
	if pl == nil {
		http.Error(w, "no profiles configured", 400)
		return
	}

	targetProfileID := req.ProfileID
	if targetProfileID == "" {
		targetProfileID = pl.Active
	}

	var targetIdx int = -1
	for i, p := range pl.Profiles {
		if p.ID == targetProfileID {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		http.Error(w, "profile not found", 400)
		return
	}

	// Update the shared resolver bank instead of per-profile resolvers.
	if req.Mode == "overwrite" {
		pl.ResolverBank = resolvers
	} else {
		// Append — deduplicate against existing bank.
		addToBank(pl, resolvers)
	}

	// Save under the requested list (or current selection / Default).
	listName := sanitizeListName(req.ListName)
	if listName == "" {
		listName = strings.TrimSpace(pl.SelectedList)
	}
	if listName == "" && len(pl.ActiveLists) > 0 {
		listName = pl.ActiveLists[0].Name
	}
	if listName == "" {
		listName = defaultListName
	}
	target := findList(pl, listName)
	if target == nil {
		pl.ActiveLists = append(pl.ActiveLists, ActiveList{Name: listName})
		target = &pl.ActiveLists[len(pl.ActiveLists)-1]
	}
	if req.Mode == "overwrite" {
		target.Resolvers = append([]string(nil), resolvers...)
	} else {
		seen := map[string]bool{}
		for _, r := range target.Resolvers {
			seen[r] = true
		}
		for _, r := range resolvers {
			if !seen[r] {
				target.Resolvers = append(target.Resolvers, r)
				seen[r] = true
			}
		}
	}
	target.LastUsed = time.Now().Unix()
	pl.SelectedList = target.Name

	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, fmt.Sprintf("save profiles: %v", err), 500)
		return
	}

	// If this is the active profile, re-init the fetcher with the updated bank.
	if targetProfileID == pl.Active {
		// Cancel any in-progress checker scan before re-initializing so the
		// old goroutine exits quickly and doesn't race with the new fetcher.
		s.mu.RLock()
		oldChecker := s.checker
		s.mu.RUnlock()
		if oldChecker != nil {
			oldChecker.CancelCurrentScan()
		}
		if err := s.initFetcher(); err != nil {
			http.Error(w, fmt.Sprintf("init fetcher: %v", err), 500)
			return
		}
		// The scanner already verified these resolvers, so skip the initial
		// health-check scan — set them as active directly, start only the
		// periodic checker, and fetch metadata immediately.
		s.mu.RLock()
		fetcher := s.fetcher
		checker := s.checker
		ctx := s.fetcherCtx
		s.mu.RUnlock()
		if fetcher != nil {
			// Pool AND active are the full saved list, not just the
			// freshly-scanned subset — append mode merges the new
			// resolvers into pre-existing list entries, and the user
			// expects all of them live (otherwise the Active panel
			// shows N while the tab badge shows N+M, which looks
			// like the apply lost the old ones).
			if target != nil && len(target.Resolvers) > 0 {
				fetcher.UpdateResolverPool(target.Resolvers)
				fetcher.SetActiveResolvers(target.Resolvers)
			} else {
				fetcher.SetActiveResolvers(resolvers)
			}
			s.saveLastScan(resolvers)
		}
		if checker != nil && ctx != nil {
			checker.StartPeriodic(ctx)
		}
		go s.refreshMetadataOnly()
	}

	s.addLog(fmt.Sprintf("Scanner resolvers applied: %d resolvers (%s) to profile %s", len(resolvers), req.Mode, pl.Profiles[targetIdx].Nickname))
	s.broadcast("event: update\ndata: \"resolver-lists\"\n\n")
	writeJSON(w, map[string]any{"ok": true, "count": len(pl.ResolverBank)})
}
