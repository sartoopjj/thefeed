package telemirror

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store persists the user-added channel list. Defaults are pinned and
// always returned at the front of List() regardless of the file content.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, "telemirror_channels.json")}
}

type subsFile struct {
	Channels []string `json:"channels"`
}

func (s *Store) loadLocked() []string {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var f subsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil
	}
	return f.Channels
}

func (s *Store) saveLocked(chs []string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(subsFile{Channels: chs}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0600)
}

// List returns the full channel list with defaults pinned to the front.
func (s *Store) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.loadLocked()
	seen := make(map[string]bool, len(DefaultChannels)+len(user))
	out := make([]string, 0, len(DefaultChannels)+len(user))
	for _, d := range DefaultChannels {
		seen[strings.ToLower(d)] = true
		out = append(out, d)
	}
	for _, u := range user {
		clean := SanitizeUsername(u)
		if clean == "" || seen[strings.ToLower(clean)] {
			continue
		}
		seen[strings.ToLower(clean)] = true
		out = append(out, clean)
	}
	return out
}

func (s *Store) Add(username string) error {
	username = SanitizeUsername(username)
	if username == "" {
		return ErrEmptyUsername
	}
	if IsDefault(username) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.loadLocked()
	for _, u := range user {
		if strings.EqualFold(u, username) {
			return nil
		}
	}
	return s.saveLocked(append(user, username))
}

func (s *Store) Remove(username string) error {
	username = SanitizeUsername(username)
	if username == "" {
		return ErrEmptyUsername
	}
	if IsDefault(username) {
		return ErrPinnedChannel
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.loadLocked()
	out := user[:0]
	for _, u := range user {
		if !strings.EqualFold(u, username) {
			out = append(out, u)
		}
	}
	return s.saveLocked(out)
}
