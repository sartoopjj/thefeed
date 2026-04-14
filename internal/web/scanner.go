package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sartoopjj/thefeed/internal/client"
)

func (s *Server) handleScannerPresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	src := defaultScannerPresets
	var lines []string
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	writeJSON(w, lines)
}

func (s *Server) handleScannerStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	var req struct {
		Targets      []string `json:"targets"`
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

	// Determine which profile to apply to.
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

	var newResolvers []string
	if req.Mode == "overwrite" {
		newResolvers = resolvers
	} else {
		// Append — deduplicate.
		seen := make(map[string]bool)
		for _, r := range pl.Profiles[targetIdx].Config.Resolvers {
			seen[r] = true
			newResolvers = append(newResolvers, r)
		}
		for _, r := range resolvers {
			if !seen[r] {
				newResolvers = append(newResolvers, r)
			}
		}
	}

	pl.Profiles[targetIdx].Config.Resolvers = newResolvers
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, fmt.Sprintf("save profiles: %v", err), 500)
		return
	}

	// If this is the active profile, also update config + fetcher.
	if targetProfileID == pl.Active {
		s.mu.Lock()
		cfg := s.config
		s.mu.Unlock()
		if cfg != nil {
			cfg.Resolvers = newResolvers
			_ = s.saveConfig(cfg)
			s.mu.Lock()
			s.config = cfg
			s.mu.Unlock()
		}
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
			fetcher.SetActiveResolvers(newResolvers)
			s.saveLastScan(newResolvers)
		}
		if checker != nil && ctx != nil {
			checker.StartPeriodic(ctx)
		}
		go s.refreshMetadataOnly()
	}

	s.addLog(fmt.Sprintf("Scanner resolvers applied: %d resolvers (%s) to profile %s", len(resolvers), req.Mode, pl.Profiles[targetIdx].Nickname))
	writeJSON(w, map[string]any{"ok": true, "count": len(newResolvers)})
}
