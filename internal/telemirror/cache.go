package telemirror

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cache TTLs.
const (
	FreshTTL = 10 * time.Minute
	StaleTTL = 24 * time.Hour
)

// Cache is a per-channel disk cache backed by a small in-memory map.
type Cache struct {
	dir string

	mu  sync.Mutex
	mem map[string]*FetchResult
}

func NewCache(dir string) *Cache {
	return &Cache{dir: dir, mem: make(map[string]*FetchResult)}
}

func (c *Cache) path(username string) string {
	return filepath.Join(c.dir, strings.ToLower(SanitizeUsername(username))+".json")
}

// Get returns (entry, fresh). fresh=false means the entry is older than
// FreshTTL but still within StaleTTL, so the caller can serve it while
// refreshing in the background.
func (c *Cache) Get(username string) (*FetchResult, bool) {
	username = strings.ToLower(SanitizeUsername(username))
	if username == "" {
		return nil, false
	}

	c.mu.Lock()
	if r, ok := c.mem[username]; ok && r != nil {
		age := time.Since(r.FetchedAt)
		if age <= StaleTTL {
			c.mu.Unlock()
			return r, age < FreshTTL
		}
		delete(c.mem, username)
	}
	c.mu.Unlock()

	b, err := os.ReadFile(c.path(username))
	if err != nil {
		return nil, false
	}
	var r FetchResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, false
	}
	if time.Since(r.FetchedAt) > StaleTTL {
		return nil, false
	}
	c.mu.Lock()
	c.mem[username] = &r
	c.mu.Unlock()
	return &r, time.Since(r.FetchedAt) < FreshTTL
}

func (c *Cache) Put(username string, r *FetchResult) error {
	username = strings.ToLower(SanitizeUsername(username))
	if username == "" || r == nil {
		return ErrEmptyUsername
	}
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return err
	}
	r.FetchedAt = time.Now()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.path(username), b, 0600); err != nil {
		return err
	}
	c.mu.Lock()
	c.mem[username] = r
	c.mu.Unlock()
	return nil
}

// Clear drops all in-memory and on-disk entries.
func (c *Cache) Clear() {
	c.mu.Lock()
	c.mem = make(map[string]*FetchResult)
	c.mu.Unlock()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(c.dir, e.Name()))
	}
}
