package web

import (
	"encoding/json"
	"net/http"
	"path/filepath"
)

// Binding the device passcode to the Telegram data key.
//
// The session file IS the Telegram account and the cache holds every message
// fetched, both sealed under a data key. In device mode that key sits in a
// file beside the data, so anyone holding the files holds the key: it protects
// against a careless backup, not against someone with the phone.
//
// Where the user has set a passcode there is something better to wrap it
// with. These endpoints re-wrap the SAME key under an Argon2id key derived
// from it -- the key is never regenerated, so an existing session survives
// and nobody is silently signed out.
//
// The native lock screen drives them, because it is the only place that ever
// sees the passcode in the clear; the WebView never does. On a phone they are
// reachable only with the per-launch HTTP Basic secret (see mobile.Password),
// so another app on the device cannot call them.

// telDir is where the Telegram data and its keyring live. Same path telHub
// uses, taken from the same constant so the two cannot drift.
func (s *Server) telDir() string { return filepath.Join(s.dataDir, telDirName) }

// handleTelLock dispatches the Telegram lock endpoints:
//
//	GET  /api/tel/lock         -> {mode, locked}
//	POST /api/tel/lock         -> wrap the key under this passcode
//	POST /api/tel/unlock       -> open it for this run
//	POST /api/tel/lock/remove  -> back to a device-held key
func (s *Server) handleTelLock(w http.ResponseWriter, r *http.Request) {
	dir := s.telDir()
	if r.Method == http.MethodGet {
		tc, err := loadTelCrypto(dir)
		if err != nil {
			http.Error(w, "encryption unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"mode": tc.mode, "locked": tc.locked})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Passphrase == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tc, err := loadTelCrypto(dir)
	if err != nil {
		http.Error(w, "encryption unavailable", http.StatusInternalServerError)
		return
	}
	// Already passphrase-wrapped and not open: re-wrapping would need the key
	// we do not have, and regenerating it would orphan the session. Unlock
	// first. (Not an error for the caller -- the lock screen calls unlock.)
	if tc.locked {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	if err := tc.setPassphrase(req.Passphrase); err != nil {
		http.Error(w, "set passphrase failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "mode": "passphrase"})
}

// handleTelUnlock opens a passcode-wrapped store for the rest of this run.
//
// Idempotent on a device-mode store: a lock screen that was set up before
// this existed, or one whose key never got wrapped, must not fail here or the
// native side would refuse to let the user in.
func (s *Server) handleTelUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Passphrase == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	dir := s.telDir()
	if tc, err := loadTelCrypto(dir); err == nil && tc.mode != "passphrase" {
		// Nothing to open. Wrap it now, so a passcode set before this feature
		// existed starts protecting the key from here on.
		if err := tc.setPassphrase(req.Passphrase); err != nil {
			s.addLog("[tel] could not bind the passcode to the data key: " + err.Error())
		}
		writeJSON(w, map[string]any{"ok": true, "mode": "passphrase"})
		return
	}
	un, err := unlockTelCrypto(dir, req.Passphrase)
	if err == errBadPassphrase {
		http.Error(w, "wrong passphrase", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "unlock failed", http.StatusInternalServerError)
		return
	}
	_ = un // the key is remembered for the process by unlockTelCrypto
	writeJSON(w, map[string]any{"ok": true, "mode": "passphrase"})
}

// handleTelLockRemove goes back to a device-held key, keeping the same data
// key so the session and cache survive.
func (s *Server) handleTelLockRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tc, err := loadTelCrypto(s.telDir())
	if err != nil {
		http.Error(w, "encryption unavailable", http.StatusInternalServerError)
		return
	}
	if tc.mode == "device" {
		writeJSON(w, map[string]any{"ok": true, "mode": "device"})
		return
	}
	if tc.locked {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	if err := tc.removePassphrase(); err != nil {
		http.Error(w, "remove failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "mode": "device"})
}
