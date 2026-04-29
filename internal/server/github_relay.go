package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// githubAPI is the canonical REST endpoint. Tests can override it.
var githubAPI = "https://api.github.com"

const flushBatchLimit = 100

// GitHubRelay uploads encrypted media to a GitHub repo. Domain and object
// names are HMAC'd; blobs are AES-256-GCM. Uploads are batched into one
// Git Data API commit per flush.
type GitHubRelay struct {
	cfg        GitHubRelayConfig
	passphrase string
	domain     string
	relayKey   [protocol.KeySize]byte
	branch     string

	client *http.Client

	mu        sync.Mutex
	known     map[string]*ghEntry
	pending   map[string]*pendingUpload
	statePath string
	dirty     bool

	// commitMu serialises ref-advancing operations so concurrent flushes
	// don't race on updateRef.
	commitMu sync.Mutex
}

type ghEntry struct {
	size     int64
	crc      uint32
	lastSeen time.Time
}

type pendingUpload struct {
	blob []byte
	size int64
	crc  uint32
}

// NewGitHubRelay returns nil when the config is incomplete.
func NewGitHubRelay(cfg GitHubRelayConfig, domain, passphrase string) *GitHubRelay {
	if !cfg.Active() || domain == "" || passphrase == "" {
		return nil
	}
	relayKey, err := protocol.DeriveRelayKey(passphrase)
	if err != nil {
		return nil
	}
	branch := cfg.Branch
	if branch == "" {
		branch = "main"
	}
	r := &GitHubRelay{
		cfg:        cfg,
		passphrase: passphrase,
		domain:     protocol.RelayDomainSegment(domain, passphrase),
		relayKey:   relayKey,
		branch:     branch,
		client:    &http.Client{Timeout: 2 * time.Minute},
		known:     make(map[string]*ghEntry),
		pending:   make(map[string]*pendingUpload),
		statePath: cfg.StatePath,
	}
	if r.statePath != "" {
		if err := r.loadState(); err != nil {
			log.Printf("[gh-relay] load state %s: %v", r.statePath, err)
		}
	}
	return r
}

type persistedEntry struct {
	Size     int64     `json:"size"`
	CRC      uint32    `json:"crc"`
	LastSeen time.Time `json:"lastSeen"`
}

func (g *GitHubRelay) loadState() error {
	f, err := os.Open(g.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	var raw map[string]persistedEntry
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, v := range raw {
		g.known[k] = &ghEntry{size: v.Size, crc: v.CRC, lastSeen: v.LastSeen}
	}
	log.Printf("[gh-relay] loaded %d entries from %s", len(raw), g.statePath)
	return nil
}

// saveStateLocked writes `known` to disk via a tmp+rename so a crash mid-write
// doesn't leave a truncated file. Caller must hold g.mu.
func (g *GitHubRelay) saveStateLocked() error {
	if g.statePath == "" {
		return nil
	}
	out := make(map[string]persistedEntry, len(g.known))
	for k, e := range g.known {
		out[k] = persistedEntry{Size: e.size, CRC: e.crc, LastSeen: e.lastSeen}
	}
	dir := filepath.Dir(g.statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "gh-relay-*.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	g.dirty = false
	return os.Rename(tmp.Name(), g.statePath)
}

// Repo returns the configured "owner/repo" so the discovery channel can
// expose it to clients without leaking the token.
func (g *GitHubRelay) Repo() string {
	if g == nil {
		return ""
	}
	return g.cfg.Repo
}

// MaxBytes is the per-file cap. 0 means no cap.
func (g *GitHubRelay) MaxBytes() int64 {
	if g == nil {
		return 0
	}
	return g.cfg.MaxBytes
}

// TTL returns the configured object lifetime.
func (g *GitHubRelay) TTL() time.Duration {
	if g == nil {
		return 0
	}
	return time.Duration(g.cfg.TTLMinutes) * time.Minute
}

// Domain is the HMAC'd path segment used inside the relay repo.
func (g *GitHubRelay) Domain() string {
	if g == nil {
		return ""
	}
	return g.domain
}

// Upload encrypts body and queues it for the next batched commit.
// ErrTooLarge if body exceeds the configured cap.
func (g *GitHubRelay) Upload(ctx context.Context, body []byte) error {
	if g == nil {
		return errors.New("github relay disabled")
	}
	if g.cfg.MaxBytes > 0 && int64(len(body)) > g.cfg.MaxBytes {
		return ErrTooLarge
	}

	size := int64(len(body))
	crc := crc32.ChecksumIEEE(body)
	key := protocol.RelayObjectName(size, crc, g.passphrase)

	g.mu.Lock()
	if e, ok := g.known[key]; ok {
		e.lastSeen = time.Now()
		g.dirty = true
		g.mu.Unlock()
		return nil
	}
	if _, ok := g.pending[key]; ok {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	blob, err := protocol.EncryptRelayBlob(g.relayKey, body)
	if err != nil {
		return fmt.Errorf("encrypt relay blob: %w", err)
	}

	g.mu.Lock()
	if e, ok := g.known[key]; ok {
		e.lastSeen = time.Now()
		g.dirty = true
		g.mu.Unlock()
		return nil
	}
	if _, ok := g.pending[key]; ok {
		g.mu.Unlock()
		return nil
	}
	g.pending[key] = &pendingUpload{blob: blob, size: size, crc: crc}
	overLimit := len(g.pending) >= flushBatchLimit
	g.mu.Unlock()

	if overLimit {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := g.flushPending(ctx); err != nil {
				log.Printf("[gh-relay] limit flush: %v", err)
			}
		}()
	}
	return nil
}

// Has reports whether the file is committed or queued for the next commit.
func (g *GitHubRelay) Has(size int64, crc uint32) bool {
	if g == nil {
		return false
	}
	key := protocol.RelayObjectName(size, crc, g.passphrase)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.known[key]; ok {
		return true
	}
	_, ok := g.pending[key]
	return ok
}

// Touch refreshes the lastSeen timestamp without re-uploading. Used when
// upstream re-delivers a file that's already in the relay.
func (g *GitHubRelay) Touch(size int64, crc uint32) {
	if g == nil {
		return
	}
	key := protocol.RelayObjectName(size, crc, g.passphrase)
	g.mu.Lock()
	if e, ok := g.known[key]; ok {
		e.lastSeen = time.Now()
		g.dirty = true
	}
	g.mu.Unlock()
}

// PruneStale removes every file in `known` whose lastSeen is older than
// cutoff. Selection happens INSIDE commitMu so concurrent prunes from
// different readers can't pick the same files and race the resulting
// commits (which used to produce 422 BadObjectState).
func (g *GitHubRelay) PruneStale(ctx context.Context, cutoff time.Time) (int, error) {
	if g == nil {
		return 0, nil
	}
	g.commitMu.Lock()
	defer g.commitMu.Unlock()

	g.mu.Lock()
	var entries []treeEntry
	var keys []string
	for k, e := range g.known {
		if e.lastSeen.Before(cutoff) {
			entries = append(entries, treeEntry{
				Path: g.domain + "/" + k,
				Mode: "100644",
				Type: "blob",
				SHA:  nil,
			})
			keys = append(keys, k)
		}
	}
	g.mu.Unlock()

	if len(entries) == 0 {
		return 0, nil
	}
	log.Printf("[gh-relay] starting prune of %d file(s)", len(entries))

	headSHA, err := g.getRef(ctx, g.branch)
	if err != nil {
		return 0, fmt.Errorf("get ref: %w", err)
	}
	parentTree, err := g.getCommitTree(ctx, headSHA)
	if err != nil {
		return 0, fmt.Errorf("get commit %s: %w", headSHA, err)
	}
	newTree, err := g.createTree(ctx, parentTree, entries)
	if err != nil {
		return 0, fmt.Errorf("create tree: %w", err)
	}
	msg := fmt.Sprintf("thefeed: prune %d file(s)", len(entries))
	commitSHA, err := g.createCommit(ctx, msg, newTree, []string{headSHA})
	if err != nil {
		return 0, fmt.Errorf("create commit: %w", err)
	}
	if err := g.updateRef(ctx, g.branch, commitSHA); err != nil {
		return 0, fmt.Errorf("update ref %s: %w", g.branch, err)
	}

	g.mu.Lock()
	for _, k := range keys {
		delete(g.known, k)
	}
	g.dirty = true
	if err := g.saveStateLocked(); err != nil {
		log.Printf("[gh-relay] save state after prune: %v", err)
	}
	g.mu.Unlock()
	return len(entries), nil
}

// --- Flush loop -------------------------------------------------------------

// Run waits for shutdown and flushes any remaining pending uploads on the
// way out. Flush + prune during normal operation are driven by
// Feed.AfterFetchCycle so they line up with the natural cadence of upstream
// fetches. A best-effort backstop tick handles the case where nothing has
// fetched in a long time (e.g. all channels were skipped from cache).
func (g *GitHubRelay) Run(ctx context.Context) {
	if g == nil {
		return
	}
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	saveTick := time.NewTicker(5 * time.Minute)
	defer saveTick.Stop()
	for {
		select {
		case <-saveTick.C:
			g.mu.Lock()
			if g.dirty && g.statePath != "" {
				if err := g.saveStateLocked(); err != nil {
					log.Printf("[gh-relay] periodic save: %v", err)
				}
			}
			g.mu.Unlock()

		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := g.flushPending(fctx); err != nil {
				log.Printf("[gh-relay] shutdown flush: %v", err)
			}
			cancel()
			g.mu.Lock()
			if g.dirty {
				if err := g.saveStateLocked(); err != nil {
					log.Printf("[gh-relay] shutdown save: %v", err)
				}
			}
			g.mu.Unlock()
			return
		case <-tick.C:
			if g.queueSize() == 0 {
				continue
			}
			fctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			if err := g.flushPending(fctx); err != nil {
				log.Printf("[gh-relay] backstop flush: %v", err)
			}
			cancel()
		}
	}
}

func (g *GitHubRelay) queueSize() int {
	g.mu.Lock()
	n := len(g.pending)
	g.mu.Unlock()
	return n
}

// Flush forces an immediate commit of any pending uploads. Safe to call
// from tests or graceful shutdown; does nothing if the queue is empty.
func (g *GitHubRelay) Flush(ctx context.Context) error {
	if g == nil {
		return nil
	}
	return g.flushPending(ctx)
}

// flushPending drains the pending map into a single Git commit via the Git
// Data API. On any error the batch is re-queued so the next tick retries.
func (g *GitHubRelay) flushPending(ctx context.Context) error {
	g.mu.Lock()
	if len(g.pending) == 0 {
		g.mu.Unlock()
		return nil
	}
	batch := g.pending
	g.pending = make(map[string]*pendingUpload)
	g.mu.Unlock()

	if err := g.commitBatch(ctx, batch); err != nil {
		// Re-queue. A peer goroutine may have queued newer entries with
		// the same key; prefer those.
		g.mu.Lock()
		for k, v := range batch {
			if _, exists := g.pending[k]; !exists {
				g.pending[k] = v
			}
		}
		g.mu.Unlock()
		return err
	}

	now := time.Now()
	g.mu.Lock()
	for k, p := range batch {
		g.known[k] = &ghEntry{size: p.size, crc: p.crc, lastSeen: now}
	}
	g.dirty = true
	if err := g.saveStateLocked(); err != nil {
		log.Printf("[gh-relay] save state: %v", err)
	}
	g.mu.Unlock()
	log.Printf("[gh-relay] committed %d file(s)", len(batch))
	return nil
}

// treeEntry is the Git Data API tree-item shape used by both upload
// (SHA = newly-created blob) and delete (SHA = nil → entry removed from
// the resulting tree).
type treeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"` // pointer so nil serialises as JSON `null`
}

// commitBatch performs the Git Data API dance:
//
//	GET ref → POST blobs → POST tree (with base_tree) → POST commit → PATCH ref.
//
// A single commit covers every file in the batch, regardless of count.
func (g *GitHubRelay) commitBatch(ctx context.Context, batch map[string]*pendingUpload) error {
	if len(batch) == 0 {
		return nil
	}
	g.commitMu.Lock()
	defer g.commitMu.Unlock()

	log.Printf("[gh-relay] starting upload of %d file(s)", len(batch))
	headSHA, err := g.getRef(ctx, g.branch)
	if err != nil {
		return fmt.Errorf("get ref: %w", err)
	}
	parentTree, err := g.getCommitTree(ctx, headSHA)
	if err != nil {
		return fmt.Errorf("get commit %s: %w", headSHA, err)
	}

	entries := make([]treeEntry, 0, len(batch))
	for objKey, p := range batch {
		blobSHA, err := g.createBlob(ctx, p.blob)
		if err != nil {
			return fmt.Errorf("create blob %s: %w", objKey, err)
		}
		s := blobSHA
		entries = append(entries, treeEntry{
			Path: g.domain + "/" + objKey,
			Mode: "100644",
			Type: "blob",
			SHA:  &s,
		})
	}

	newTree, err := g.createTree(ctx, parentTree, entries)
	if err != nil {
		return fmt.Errorf("create tree: %w", err)
	}
	msg := fmt.Sprintf("thefeed: upload %d file(s)", len(batch))
	commitSHA, err := g.createCommit(ctx, msg, newTree, []string{headSHA})
	if err != nil {
		return fmt.Errorf("create commit: %w", err)
	}
	if err := g.updateRef(ctx, g.branch, commitSHA); err != nil {
		return fmt.Errorf("update ref %s: %w", g.branch, err)
	}
	return nil
}

// --- Git Data API plumbing --------------------------------------------------

func (g *GitHubRelay) getRef(ctx context.Context, branch string) (string, error) {
	req, err := g.newReq(ctx, http.MethodGet, "/repos/"+g.cfg.Repo+"/git/ref/heads/"+branch, nil)
	if err != nil {
		return "", err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s — %s", resp.Status, string(body))
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

func (g *GitHubRelay) getCommitTree(ctx context.Context, commitSHA string) (string, error) {
	req, err := g.newReq(ctx, http.MethodGet, "/repos/"+g.cfg.Repo+"/git/commits/"+commitSHA, nil)
	if err != nil {
		return "", err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s — %s", resp.Status, string(body))
	}
	var out struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Tree.SHA, nil
}

func (g *GitHubRelay) createBlob(ctx context.Context, content []byte) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(content),
	})
	req, err := g.newReq(ctx, http.MethodPost, "/repos/"+g.cfg.Repo+"/git/blobs", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s — %s", resp.Status, string(raw))
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

func (g *GitHubRelay) createTree(ctx context.Context, baseTree string, entries any) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"base_tree": baseTree,
		"tree":      entries,
	})
	req, err := g.newReq(ctx, http.MethodPost, "/repos/"+g.cfg.Repo+"/git/trees", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s — %s", resp.Status, string(raw))
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

func (g *GitHubRelay) createCommit(ctx context.Context, message, treeSHA string, parents []string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"message": message,
		"tree":    treeSHA,
		"parents": parents,
	})
	req, err := g.newReq(ctx, http.MethodPost, "/repos/"+g.cfg.Repo+"/git/commits", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s — %s", resp.Status, string(raw))
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

func (g *GitHubRelay) updateRef(ctx context.Context, branch, commitSHA string) error {
	body, _ := json.Marshal(map[string]any{
		"sha":   commitSHA,
		"force": false,
	})
	req, err := g.newReq(ctx, http.MethodPatch, "/repos/"+g.cfg.Repo+"/git/refs/heads/"+branch, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s — %s", resp.Status, string(raw))
	}
	return nil
}

// --- HTTP plumbing ----------------------------------------------------------

func (g *GitHubRelay) newReq(ctx context.Context, method, urlPath string, body io.Reader) (*http.Request, error) {
	full := strings.TrimRight(githubAPI, "/") + urlPath
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "thefeed-server")
	return req, nil
}
