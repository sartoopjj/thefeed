package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub stubs the slice of GitHub's REST API the relay uses:
//   - Git Data API (refs / commits / blobs / trees) for batched uploads
//   - Contents API (list / delete) for PruneStale
type fakeGitHub struct {
	mu      sync.Mutex
	files   map[string][]byte // repoPath → ciphertext (committed)
	commits int               // number of commits created (rate-limit metric)
	blobs   int               // blob create count
	deletes int               // contents-api deletions

	// Tree state — dumb counter; we don't model real Git history.
	headSHA  string
	treeSHA  string
	nextSeq  int
}

func (f *fakeGitHub) sha(prefix string) string {
	f.nextSeq++
	return prefix + "-" + strconv.Itoa(f.nextSeq)
}

func (f *fakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/")

		// --- Git Data API ---------------------------------------------------
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "git/ref/heads/"):
			if f.headSHA == "" {
				f.headSHA = f.sha("commit")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": map[string]any{"sha": f.headSHA},
			})
			return

		case r.Method == http.MethodGet && strings.HasPrefix(path, "git/commits/"):
			if f.treeSHA == "" {
				f.treeSHA = f.sha("tree")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tree": map[string]any{"sha": f.treeSHA},
			})
			return

		case r.Method == http.MethodPost && path == "git/blobs":
			var body struct{ Content string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.blobs++
			s := f.sha("blob")
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": s})
			return

		case r.Method == http.MethodPost && path == "git/trees":
			// SHA is *string so null serialises as JSON null and decodes back to nil.
			var body struct {
				BaseTree string `json:"base_tree"`
				Tree     []struct {
					Path string  `json:"path"`
					SHA  *string `json:"sha"`
				} `json:"tree"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, e := range body.Tree {
				if e.SHA == nil {
					delete(f.files, e.Path)
					f.deletes++
				} else {
					f.files[e.Path] = []byte("committed")
				}
			}
			f.treeSHA = f.sha("tree")
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": f.treeSHA})
			return

		case r.Method == http.MethodPost && path == "git/commits":
			f.commits++
			f.headSHA = f.sha("commit")
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": f.headSHA})
			return

		case r.Method == http.MethodPatch && strings.HasPrefix(path, "git/refs/heads/"):
			w.WriteHeader(http.StatusOK)
			return
		}

		// --- Contents API (used only for the directory listing in PruneStale) ---
		if r.Method == http.MethodGet {
			repoPath := strings.TrimPrefix(path, "contents/")
			items := []map[string]any{}
			prefix := repoPath + "/"
			for k, v := range f.files {
				if strings.HasPrefix(k, prefix) {
					items = append(items, map[string]any{
						"path": k, "sha": "sha-" + k, "type": "file", "size": len(v),
					})
				}
			}
			_ = json.NewEncoder(w).Encode(items)
		}
	})
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, func()) {
	f := &fakeGitHub{files: map[string][]byte{}}
	srv := httptest.NewServer(f.handler())
	prev := githubAPI
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = prev; srv.Close() })
	return f, srv.Close
}

func TestGitHubRelayUploadAndDedup(t *testing.T) {
	fk, _ := newFakeGitHub(t)
	r := NewGitHubRelay(GitHubRelayConfig{Enabled: true, Token: "tok", Repo: "owner/repo", MaxBytes: 1 << 20, TTLMinutes: 60}, "feed.example.com", "test-passphrase")
	if r == nil {
		t.Fatal("relay should activate with full config")
	}

	body := []byte("hello relay world")
	if err := r.Upload(context.Background(), body); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// Second upload of the same content must dedup before reaching GitHub.
	if err := r.Upload(context.Background(), body); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	// Force the batch to commit synchronously.
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if fk.commits != 1 {
		t.Errorf("commits = %d, want 1 (one batch)", fk.commits)
	}
	if fk.blobs != 1 {
		t.Errorf("blobs = %d, want 1 (dedup before flush)", fk.blobs)
	}
	if !r.Has(int64(len(body)), crc32.ChecksumIEEE(body)) {
		t.Errorf("Has should return true after upload")
	}
	// A third Flush with no new uploads must be a no-op (no new commit).
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("noop flush: %v", err)
	}
	if fk.commits != 1 {
		t.Errorf("commits after noop flush = %d, want 1", fk.commits)
	}
}

func TestGitHubRelayMaxBytes(t *testing.T) {
	newFakeGitHub(t)
	r := NewGitHubRelay(GitHubRelayConfig{Enabled: true, Token: "tok", Repo: "owner/repo", MaxBytes: 16, TTLMinutes: 60}, "ex.test", "pp")
	err := r.Upload(context.Background(), bytes.Repeat([]byte("x"), 32))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestGitHubRelayPruneStale(t *testing.T) {
	fk, _ := newFakeGitHub(t)
	r := NewGitHubRelay(GitHubRelayConfig{Enabled: true, Token: "tok", Repo: "owner/repo", MaxBytes: 1 << 20, TTLMinutes: 1}, "ex.test", "pp")
	if err := r.Upload(context.Background(), []byte("stays")); err != nil {
		t.Fatalf("upload stays: %v", err)
	}
	if err := r.Upload(context.Background(), []byte("goes")); err != nil {
		t.Fatalf("upload goes: %v", err)
	}
	// Commit the batch so PruneStale can find files in the listing.
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// Roll back the lastSeen of the "goes" entry so PruneStale removes it.
	// "stays" is 5 bytes, "goes" is 4 — match by size.
	r.mu.Lock()
	for _, e := range r.known {
		if e.size == 4 {
			e.lastSeen = time.Now().Add(-2 * time.Hour)
		}
	}
	r.mu.Unlock()

	commitsBefore := fk.commits
	removed, err := r.PruneStale(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if fk.deletes != 1 {
		t.Errorf("tree-deletes = %d, want 1", fk.deletes)
	}
	if got := fk.commits - commitsBefore; got != 1 {
		t.Errorf("prune commits = %d, want 1 (single batched commit)", got)
	}
}

// TestGitHubRelayStatePersistence: known map survives a fresh relay
// instance pointed at the same statePath.
func TestGitHubRelayStatePersistence(t *testing.T) {
	newFakeGitHub(t)
	dir := t.TempDir()
	statePath := dir + "/gh_relay_state.json"

	cfg := GitHubRelayConfig{Enabled: true, Token: "tok", Repo: "owner/repo", MaxBytes: 1 << 20, TTLMinutes: 60, StatePath: statePath}
	r1 := NewGitHubRelay(cfg, "ex.test", "pp")
	if err := r1.Upload(context.Background(), []byte("survive me")); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := r1.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	body := []byte("survive me")
	if !r1.Has(int64(len(body)), crc32.ChecksumIEEE(body)) {
		t.Fatal("r1 should know the file after flush")
	}

	r2 := NewGitHubRelay(cfg, "ex.test", "pp")
	if !r2.Has(int64(len(body)), crc32.ChecksumIEEE(body)) {
		t.Fatal("r2 should have loaded the file from statePath")
	}
}

