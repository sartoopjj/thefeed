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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// githubAPI is the canonical REST endpoint. Tests can override it.
var githubAPI = "https://api.github.com"

const flushBatchLimit = 100

// uploadConcurrency bounds simultaneous blob uploads. GitHub asks clients not
// to fire mutating requests at high concurrency (secondary rate limits), so
// keep this small — it is a few-at-a-time pipeline, not a fan-out.
const uploadConcurrency = 4

// GitHub caps writes at about 80 a minute and 500 an hour, and keeps
// refusing a client that goes over. Blobs are most of the writes, so they are
// paced under both: a burst, then one per blobInterval (about 450 an hour),
// which leaves room for the commits (three writes each).
const (
	blobBurst    = 40
	blobInterval = 8 * time.Second
)

// maxReadyObjects bounds the commit queue. Entries are SHA-only (~60 bytes),
// so this is a sanity backstop, not a memory concern.
const maxReadyObjects = 50000

// defaultRepoQuotaKB is GitHub's observed per-repo hard ceiling (~100 GB),
// used only to report headroom. GitHub doesn't expose the real quota.
const defaultRepoQuotaKB int64 = 100 * 1024 * 1024

// Warn thresholds as a fraction of the quota. Recovery needs a manual repo
// recreate, so the operator has to hear about it well before the wall.
const (
	repoWarnFraction   = 0.70
	repoUrgentFraction = 0.90
)

// Flush backoff after a failed commit; a quota-exhausted repo never
// self-heals, so retrying every cycle only burns API calls.
const (
	minFlushBackoff = 1 * time.Minute
	maxFlushBackoff = 30 * time.Minute
)

// recoveryProbeEvery is how often a relay parked in an operator-action state
// (repo full, token not granted) re-checks the remote. Without it the operator
// fixes the problem and then waits out maxFlushBackoff wondering why nothing
// happens; with it, uploads resume about a minute later.
const recoveryProbeEvery = time.Minute

// uploadErrLogEvery throttles the blob-upload failure log. These errors are
// per-file, so an outage would otherwise print one line per media item.
const uploadErrLogEvery = time.Minute

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
	ready     map[string]*readyObject
	statePath string
	dirty     bool

	// uploadSem bounds concurrent blob uploads; see uploadConcurrency.
	uploadSem chan struct{}
	// uploading holds the objects being uploaded now, so a second store of
	// the same file does not upload it twice.
	uploading map[string]bool

	// The blob pacer (see blobBurst): tokens left as of paceAt, and no blob
	// before pauseUntil, which GitHub sets when it asks us to back off.
	paceMu     sync.Mutex
	paceTokens float64
	paceAt     time.Time
	pauseUntil time.Time

	failStreak       int
	retryAfter       time.Time
	quotaExhausted   bool
	tokenDenied      bool
	lastFlushErr     string
	uploadFails      int
	lastUploadErrLog time.Time

	// lastProbeOK is the previous recovery-probe verdict. Only a false→true
	// transition clears the backoff, so a remote that reads fine but still
	// rejects writes doesn't retry (or re-log) every probe.
	lastProbeOK bool

	// readyGen is bumped whenever every queued SHA becomes invalid (the remote
	// repo was recreated). A flush that started before the bump must not
	// re-queue its batch: those blobs live in a repo that no longer exists.
	readyGen uint64

	// Repo size from the API; refreshed periodically, not per upload.
	repoSizeKB   int64
	repoQuotaKB  int64
	repoSizeAt   time.Time
	repoWarnedAt float64 // highest fraction already warned about

	// commitMu serialises ref-advancing operations so concurrent flushes
	// don't race on updateRef.
	commitMu sync.Mutex
}

type ghEntry struct {
	size     int64
	crc      uint32
	lastSeen time.Time
}

// readyObject is a blob already uploaded to GitHub, waiting only to be made
// reachable by a commit. Holding the SHA instead of the bytes is what keeps
// the queue at ~60 bytes per file rather than megabytes.
type readyObject struct {
	sha  string
	size int64
	crc  uint32
	// commitFails counts consecutive failed commit attempts. An uploaded blob
	// is unreachable until it lands in a commit, and GitHub eventually garbage
	// collects unreachable objects — once that happens the SHA is dead and
	// every later commit referencing it fails the same way. Give up after
	// maxCommitAttempts so a dead SHA can't wedge the queue forever.
	commitFails int
}

// maxCommitAttempts bounds how often a queued SHA is retried before it is
// dropped (the file is then re-uploaded the next time upstream offers it).
const maxCommitAttempts = 5

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
		cfg:         cfg,
		passphrase:  passphrase,
		domain:      protocol.RelayDomainSegment(domain, passphrase),
		relayKey:    relayKey,
		branch:      branch,
		client:      &http.Client{Timeout: 2 * time.Minute},
		known:       make(map[string]*ghEntry),
		ready:       make(map[string]*readyObject),
		uploadSem:   make(chan struct{}, uploadConcurrency),
		uploading:   make(map[string]bool),
		statePath:   cfg.StatePath,
		repoQuotaKB: cfg.QuotaKB,
	}
	if r.repoQuotaKB <= 0 {
		r.repoQuotaKB = defaultRepoQuotaKB
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

// Upload encrypts body, pushes the blob to GitHub immediately, and queues only
// the resulting SHA for the next batched commit. Uploading eagerly (rather than
// holding bytes until commit time) keeps the queue at ~60 bytes per file and
// lets uploads overlap; a git blob SHA is a hash of the content, so a retry
// after failure is idempotent and never grows the repo.
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
	if _, ok := g.ready[key]; ok || g.uploading[key] {
		g.mu.Unlock()
		return nil
	}
	// While backed off (typically a size-quota rejection) don't even encrypt:
	// the upload would fail anyway, and skipping it keeps memory flat.
	if time.Now().Before(g.retryAfter) {
		g.mu.Unlock()
		return nil
	}
	if len(g.ready) >= maxReadyObjects {
		g.mu.Unlock()
		return nil
	}
	g.uploading[key] = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.uploading, key)
		g.mu.Unlock()
	}()

	// Bounded concurrency. The caller is already a background goroutine, so
	// waiting here never delays the DNS path. Encrypt *after* the semaphore:
	// a goroutine waiting its turn would otherwise sit on a full ciphertext
	// copy on top of the plaintext it already holds.
	select {
	case g.uploadSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := g.pace(ctx); err != nil {
		<-g.uploadSem
		return err
	}
	blob, err := protocol.EncryptRelayBlob(g.relayKey, body)
	if err != nil {
		<-g.uploadSem
		return fmt.Errorf("encrypt relay blob: %w", err)
	}
	sha, err := g.createBlob(ctx, blob)
	if isRepoEmpty(err) {
		// The Git Data API refuses to create objects in a repo with no
		// commits, and we upload blobs before any commit — so a freshly
		// created (empty) repo would reject every blob forever, leaving
		// nothing queued and nothing to trigger the bootstrap. Bootstrap via
		// getRef, then retry the blob once. commitMu keeps concurrent uploads
		// from racing to create the first commit.
		g.commitMu.Lock()
		_, bootErr := g.getRef(ctx, g.branch)
		g.commitMu.Unlock()
		if bootErr == nil {
			if err = g.pace(ctx); err == nil {
				sha, err = g.createBlob(ctx, blob)
			}
		}
	}
	<-g.uploadSem
	if err != nil {
		// Only failures that need operator action (size quota, revoked token
		// grant) arm the global backoff. Arming it for a transient error (a
		// 502, a timeout on one large file) would make Upload silently drop
		// every file for the next 1–30 minutes — and relay-only files, which
		// are too big for DNS, have no fallback.
		var rl *rateLimitedError
		if errors.As(err, &rl) {
			g.pauseBlobs(rl.wait)
		}
		g.mu.Lock()
		if isQuotaExhausted(err) || isTokenAccessDenied(err) {
			g.noteFlushFailureLocked(err)
		} else {
			// Must log here: the media cache reports upload errors through a
			// debug-gated logger, and this branch deliberately skips the
			// backoff path, so without this a failing relay is completely
			// silent — no log, no failStreak, and the report reads "ok".
			// Throttled so a persistently broken remote doesn't flood the log.
			g.lastFlushErr = trimErrBody(err.Error())
			g.uploadFails++
			if now := time.Now(); now.Sub(g.lastUploadErrLog) >= uploadErrLogEvery {
				g.lastUploadErrLog = now
				log.Printf("[gh-relay] blob upload failed (%d since last report): %v", g.uploadFails, err)
				g.uploadFails = 0
			}
		}
		g.mu.Unlock()
		return fmt.Errorf("create blob: %w", err)
	}

	g.mu.Lock()
	if _, ok := g.known[key]; ok {
		g.mu.Unlock()
		return nil
	}
	g.lastFlushErr = ""
	g.ready[key] = &readyObject{sha: sha, size: size, crc: crc}
	overLimit := len(g.ready) >= flushBatchLimit
	g.mu.Unlock()

	if overLimit {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := g.flushPending(ctx, false); err != nil {
				log.Printf("[gh-relay] limit flush: %v", err)
			}
		}()
	}
	return nil
}

// RelayStatus is a snapshot of relay health for the hourly report.
type RelayStatus struct {
	Repo         string  `json:"repo"`
	RepoSizeKB   int64   `json:"repoSizeKB"`
	QuotaKB      int64   `json:"quotaKB"`
	PercentUsed  float64 `json:"percentUsed"`
	PendingFiles int     `json:"pendingFiles"`
	KnownObjects int     `json:"knownObjects"`
	Quota403     bool    `json:"quotaExhausted"`
	TokenDenied  bool    `json:"tokenDenied"`
	FailStreak   int     `json:"failStreak"`
	LastError    string  `json:"lastError,omitempty"`
}

// Status reports relay health. Uses the last polled repo size; call
// RefreshRepoSize to update it.
func (g *GitHubRelay) Status() *RelayStatus {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	quota := g.repoQuotaKB
	if quota <= 0 {
		quota = defaultRepoQuotaKB
	}
	var pct float64
	if quota > 0 && g.repoSizeKB > 0 {
		pct = float64(g.repoSizeKB) / float64(quota) * 100
	}
	return &RelayStatus{
		Repo:         g.cfg.Repo,
		RepoSizeKB:   g.repoSizeKB,
		QuotaKB:      quota,
		PercentUsed:  pct,
		PendingFiles: len(g.ready),
		KnownObjects: len(g.known),
		Quota403:     g.quotaExhausted,
		TokenDenied:  g.tokenDenied,
		FailStreak:   g.failStreak,
		LastError:    g.lastFlushErr,
	}
}

// RefreshRepoSize polls the repo's size and warns as it approaches the quota.
// Only needs metadata:read, which every token carries. Deleting files can't
// reclaim space (git history keeps every blob), so the warning tells the
// operator to recreate the repo before uploads start failing.
func (g *GitHubRelay) RefreshRepoSize(ctx context.Context) error {
	if g == nil {
		return nil
	}
	req, err := g.newReq(ctx, http.MethodGet, "/repos/"+g.cfg.Repo, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("repo metadata: %s — %s", resp.Status, ghErrorBody(resp))
	}
	var out struct {
		Size int64 `json:"size"` // KB
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}

	g.mu.Lock()
	g.repoSizeKB = out.Size
	g.repoSizeAt = time.Now()
	quota := g.repoQuotaKB
	if quota <= 0 {
		quota = defaultRepoQuotaKB
	}
	frac := float64(out.Size) / float64(quota)
	repo := g.cfg.Repo
	warn := ""
	switch {
	case frac >= repoUrgentFraction && g.repoWarnedAt < repoUrgentFraction:
		g.repoWarnedAt = repoUrgentFraction
		warn = "urgent"
	case frac >= repoWarnFraction && g.repoWarnedAt < repoWarnFraction:
		g.repoWarnedAt = repoWarnFraction
		warn = "warn"
	case frac < repoWarnFraction:
		g.repoWarnedAt = 0
	}
	g.mu.Unlock()

	if warn != "" {
		log.Printf("[gh-relay] repo %s is %.0f%% of its %d GB size budget (%d GB used). "+
			"Pruning cannot shrink it — git history keeps every blob. Delete and recreate the repo "+
			"before uploads start failing; the relay re-uploads on demand and clients fall back to DNS meanwhile.",
			repo, frac*100, quota/(1024*1024), out.Size/(1024*1024))
	}
	return nil
}

// verifyRemoteIndex drops the object index when this server's folder is gone
// from the remote. bootstrapEmptyRepo only fires for an empty repo or a missing
// branch, so a repo recreated *with* a README (non-empty, same branch) would
// otherwise keep a stale index: Upload would dedup against files that no longer
// exist and clients would never receive them. Only a definitive 404 resets;
// transport errors leave the index alone.
func (g *GitHubRelay) verifyRemoteIndex(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	n := len(g.known)
	g.mu.Unlock()
	if n == 0 {
		return nil
	}
	req, err := g.newReq(ctx, http.MethodGet,
		"/repos/"+g.cfg.Repo+"/contents/"+g.domain+"?ref="+g.branch, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// GitHub answers 404 (not 403) for a repo the token cannot see, so a
		// missing folder and a token that lost access are indistinguishable
		// here. Ask about the repo itself before discarding the index.
		ok, err := g.repoAccessible(ctx)
		if err != nil {
			return fmt.Errorf("verify relay folder: %w", err)
		}
		if !ok {
			return fmt.Errorf("cannot see repo %q: it was deleted, renamed, or the token no longer grants "+
				"access to it. A fine-grained token is scoped to a repository ID, so recreating a repo with the "+
				"same name revokes access — re-grant this repo in the token settings (classic ghp_ tokens are "+
				"unaffected). Keeping the object index until access returns", g.cfg.Repo)
		}
		g.forgetKnownAfterRemoteReset()
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("verify relay folder: %s — %s", resp.Status, ghErrorBody(resp))
	}
	return nil
}

// probeRecovery clears the backoff as soon as the remote looks usable again,
// so a re-granted token or a recreated repo resumes in about a minute instead
// of waiting out maxFlushBackoff. It runs only while parked in a state that
// needs operator action, and costs one read per probe.
//
// Only a false→true transition clears: a remote that is readable but still
// refuses writes settles back into the normal backoff after one retry instead
// of hammering the API every probe.
func (g *GitHubRelay) probeRecovery(ctx context.Context) {
	if g == nil {
		return
	}
	g.mu.Lock()
	parked := g.quotaExhausted || g.tokenDenied
	prev := g.lastProbeOK
	g.mu.Unlock()
	if !parked {
		return
	}

	// RefreshRepoSize is the probe: it fails for a repo we cannot see and
	// otherwise tells us whether the size problem is gone.
	healthy := g.RefreshRepoSize(ctx) == nil

	g.mu.Lock()
	defer g.mu.Unlock()
	if healthy && g.repoQuotaKB > 0 && g.repoSizeKB >= g.repoQuotaKB {
		healthy = false // readable, but still over the size budget
	}
	g.lastProbeOK = healthy
	if !healthy || prev {
		return
	}
	g.failStreak = 0
	g.retryAfter = time.Time{}
	g.quotaExhausted = false
	g.tokenDenied = false
	log.Printf("[gh-relay] %s is reachable again — clearing the backoff and resuming uploads", g.cfg.Repo)
}

// repoAccessible reports whether the token can currently see the repo.
func (g *GitHubRelay) repoAccessible(ctx context.Context) (bool, error) {
	req, err := g.newReq(ctx, http.MethodGet, "/repos/"+g.cfg.Repo, nil)
	if err != nil {
		return false, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("repo lookup: %s — %s", resp.Status, ghErrorBody(resp))
	}
	return true, nil
}

// isTokenAccessDenied reports GitHub's "resource not accessible by personal
// access token" rejection: the token authenticates but is not granted this
// repo. A fine-grained token is scoped to a repository ID, so recreating a repo
// revokes the grant even though the name is unchanged. (A token that cannot see
// the repo at all gets 404 instead — see verifyRemoteIndex.)
func isTokenAccessDenied(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not accessible by personal access token")
}

// isRepoEmpty reports GitHub's 409 "Git Repository is empty": the Git Data API
// will not create objects until the repo has a first commit. Self-healing —
// see the bootstrap retry in Upload.
func isRepoEmpty(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "repository is empty")
}

// isQuotaExhausted reports GitHub's "above its size quota" rejection, which
// needs a manual repo recreate and never clears on its own.
func isQuotaExhausted(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "above its size quota")
}

// noteFlushFailureLocked records a failed flush and arms the backoff timer.
// Caller must hold g.mu.
func (g *GitHubRelay) noteFlushFailureLocked(err error) {
	g.failStreak++
	if err != nil {
		g.lastFlushErr = trimErrBody(err.Error())
	}
	backoff := minFlushBackoff << uint(min(g.failStreak-1, 8))
	if backoff > maxFlushBackoff || backoff <= 0 {
		backoff = maxFlushBackoff
	}
	if isQuotaExhausted(err) {
		backoff = maxFlushBackoff
		if !g.quotaExhausted {
			g.quotaExhausted = true
			log.Printf("[gh-relay] REPO FULL: GitHub rejected the commit because %q is above its size quota. "+
				"The relay cannot upload or even prune until this is fixed (create a fresh repo, or rotate/reset the "+
				"existing one) — queued uploads are being dropped to protect memory, and clients will fall back to the "+
				"DNS media path meanwhile.", g.cfg.Repo)
		}
	}
	if isTokenAccessDenied(err) {
		backoff = maxFlushBackoff
		if !g.tokenDenied {
			g.tokenDenied = true
			log.Printf("[gh-relay] TOKEN CANNOT WRITE %q: GitHub says the resource is not accessible by this personal "+
				"access token. A fine-grained token is bound to a repository ID, so recreating the repo revokes its grant "+
				"even though the name is unchanged — open the token settings and give this repository Contents: "+
				"read and write again. Media keeps flowing over DNS until then.", g.cfg.Repo)
		}
	}
	g.retryAfter = time.Now().Add(backoff)
	log.Printf("[gh-relay] flush failed (streak=%d), next attempt in %s", g.failStreak, backoff)
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
	_, ok := g.ready[key]
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
func (g *GitHubRelay) PruneStale(ctx context.Context, cutoff time.Time) (n int, err error) {
	if g == nil {
		return 0, nil
	}
	g.commitMu.Lock()
	defer g.commitMu.Unlock()

	// A failed prune arms the same backoff as a failed flush — otherwise a
	// quota-exhausted repo retries a huge tree every fetch cycle.
	defer func() {
		if err != nil {
			g.mu.Lock()
			g.noteFlushFailureLocked(err)
			g.mu.Unlock()
		}
	}()

	// Same backoff gate as flushPending: pruning also needs a commit, so a
	// quota-exhausted repo rejects it every cycle (17k-file trees each time).
	g.mu.Lock()
	if time.Now().Before(g.retryAfter) {
		g.mu.Unlock()
		return 0, nil
	}
	g.mu.Unlock()

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
	msg := fmt.Sprintf("prune %d", len(entries))
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
	sizeTick := time.NewTicker(30 * time.Minute)
	defer sizeTick.Stop()
	probeTick := time.NewTicker(recoveryProbeEvery)
	defer probeTick.Stop()

	// At startup: confirm our objects are still on the remote (the repo may
	// have been recreated while we were down), then poll the size so the
	// first hourly report already has one.
	go func() {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := g.verifyRemoteIndex(sctx); err != nil {
			log.Printf("[gh-relay] verify remote index: %v", err)
		}
		if err := g.RefreshRepoSize(sctx); err != nil {
			log.Printf("[gh-relay] repo size: %v", err)
		}
	}()

	for {
		select {
		case <-sizeTick.C:
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := g.RefreshRepoSize(sctx); err != nil {
				log.Printf("[gh-relay] repo size: %v", err)
			}
			cancel()

		case <-probeTick.C:
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			g.probeRecovery(pctx)
			cancel()

		case <-saveTick.C:
			g.mu.Lock()
			if g.dirty && g.statePath != "" {
				if err := g.saveStateLocked(); err != nil {
					log.Printf("[gh-relay] periodic save: %v", err)
				}
			}
			g.mu.Unlock()

		case <-ctx.Done():
			// Ignore the backoff gate: this is the last chance to make blobs
			// already sitting in the repo reachable by a commit.
			fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := g.flushPending(fctx, true); err != nil {
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
			if err := g.flushPending(fctx, false); err != nil {
				log.Printf("[gh-relay] backstop flush: %v", err)
			}
			cancel()
		}
	}
}

func (g *GitHubRelay) queueSize() int {
	g.mu.Lock()
	n := len(g.ready)
	g.mu.Unlock()
	return n
}

// Flush forces an immediate commit of any pending uploads. Safe to call
// from tests or graceful shutdown; does nothing if the queue is empty.
func (g *GitHubRelay) Flush(ctx context.Context) error {
	if g == nil {
		return nil
	}
	return g.flushPending(ctx, false)
}

// flushPending drains the pending map into a single Git commit via the Git
// Data API. On any error the batch is re-queued so the next tick retries.
// ignoreBackoff skips the backoff gate; used by the shutdown flush, which is
// the last chance to make already-uploaded blobs reachable.
func (g *GitHubRelay) flushPending(ctx context.Context, ignoreBackoff bool) error {
	g.mu.Lock()
	if len(g.ready) == 0 {
		g.mu.Unlock()
		return nil
	}
	// Backoff gate: a quota-exhausted or unreachable remote fails every
	// attempt, and each attempt rebuilds the whole tree. Wait it out
	// instead of hammering the API every few minutes.
	if now := time.Now(); !ignoreBackoff && now.Before(g.retryAfter) {
		g.mu.Unlock()
		return nil
	}
	batch := g.ready
	gen := g.readyGen
	g.ready = make(map[string]*readyObject)
	g.mu.Unlock()

	if err := g.commitBatch(ctx, batch); err != nil {
		g.mu.Lock()
		// If the remote was recreated mid-flush, every SHA in this batch
		// points into a repo that no longer exists — re-queuing them would
		// fail identically forever, and Upload's dedup would block the
		// re-upload. Drop them instead; the files come back on demand.
		if gen != g.readyGen {
			log.Printf("[gh-relay] dropping %d queued object(s): the remote repo was recreated mid-flush", len(batch))
			g.noteFlushFailureLocked(err)
			g.mu.Unlock()
			return err
		}
		// Re-queue the SHAs (cheap — no file bytes) so the next tick retries.
		// A peer goroutine may have queued the same key meanwhile; keep that.
		dropped := 0
		for k, v := range batch {
			if _, exists := g.ready[k]; exists {
				continue
			}
			v.commitFails++
			if v.commitFails >= maxCommitAttempts {
				dropped++
				continue
			}
			g.ready[k] = v
		}
		if dropped > 0 {
			log.Printf("[gh-relay] dropping %d queued object(s) after %d failed commit attempts; "+
				"they will be re-uploaded the next time upstream offers the file", dropped, maxCommitAttempts)
		}
		g.noteFlushFailureLocked(err)
		g.mu.Unlock()
		return err
	}

	now := time.Now()
	g.mu.Lock()
	g.failStreak = 0
	g.retryAfter = time.Time{}
	g.quotaExhausted = false
	g.tokenDenied = false
	g.lastFlushErr = ""
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

// commitBatch makes already-uploaded blobs reachable:
//
//	GET ref → POST tree (with base_tree) → POST commit → PATCH ref.
//
// Blobs were pushed by Upload, so this is three calls regardless of how many
// files the batch references.
func (g *GitHubRelay) commitBatch(ctx context.Context, batch map[string]*readyObject) error {
	if len(batch) == 0 {
		return nil
	}
	g.commitMu.Lock()
	defer g.commitMu.Unlock()

	log.Printf("[gh-relay] committing %d file(s)", len(batch))
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
		s := p.sha
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
	msg := fmt.Sprintf("upload %d", len(batch))
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		bodyStr := string(body)
		// Detect "empty repo" by status + body message together. Don't
		// trust status alone — GitHub uses 404 for missing branch,
		// 409 for "Git Repository is empty.", and 409 also for other
		// conflicts we don't want to silently bootstrap on top of.
		if (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict) &&
			strings.Contains(bodyStr, "Repository is empty") {
			return g.bootstrapEmptyRepo(ctx, branch)
		}
		// Branch missing on a non-empty repo: caller can decide.
		if resp.StatusCode == http.StatusNotFound {
			return g.bootstrapEmptyRepo(ctx, branch)
		}
		return "", fmt.Errorf("%s — %s", resp.Status, trimErrBody(bodyStr))
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

// bootstrapEmptyRepo initializes a fresh repo via the Contents API,
// which is the only endpoint that works without an existing Git ref.
// PUT'ing a single file auto-creates the branch with the initial commit;
// after that the Git Data API works normally for batched uploads.
// Returns the new commit SHA so the caller can use it as the parent.
func (g *GitHubRelay) bootstrapEmptyRepo(ctx context.Context, branch string) (string, error) {
	log.Printf("[gh-relay] bootstrapping empty repo on branch %s", branch)
	payload := map[string]any{
		"message": "init",
		"content": base64.StdEncoding.EncodeToString([]byte{'\n'}),
		"branch":  branch,
	}
	body, _ := json.Marshal(payload)
	req, err := g.newReq(ctx, http.MethodPut, "/repos/"+g.cfg.Repo+"/contents/.gitkeep", bytes.NewReader(body))
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
		return "", fmt.Errorf("bootstrap put: %s — %s", resp.Status, string(raw))
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("bootstrap decode: %w", err)
	}
	if out.Commit.SHA == "" {
		return "", errors.New("bootstrap: no commit SHA in response")
	}
	// First commit means the remote holds no relay objects; if local state
	// still lists some, the repo was recreated or wiped under us.
	g.forgetKnownAfterRemoteReset()
	return out.Commit.SHA, nil
}

// forgetKnownAfterRemoteReset clears the object index after the remote started
// over: stale entries make Upload dedup against missing files and PruneStale
// delete paths that no longer exist. Queued SHAs are dropped for the same
// reason — those blobs lived in the repo that just disappeared, so committing
// them would fail forever while Upload's dedup blocked the re-upload.
func (g *GitHubRelay) forgetKnownAfterRemoteReset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := len(g.known)
	// Bump unconditionally: a flush in progress must not re-queue its batch
	// even when the index happened to be empty.
	g.readyGen++
	g.ready = make(map[string]*readyObject)
	if n == 0 {
		return
	}
	g.known = make(map[string]*ghEntry)
	g.dirty = true
	if err := g.saveStateLocked(); err != nil {
		log.Printf("[gh-relay] save state after remote reset: %v", err)
	}
	log.Printf("[gh-relay] remote no longer holds our objects but local state listed %d — "+
		"the repo was recreated or wiped, so the index has been reset; files will be re-uploaded on demand", n)
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
		return "", fmt.Errorf("%s — %s", resp.Status, ghErrorBody(resp))
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
		err := fmt.Errorf("%s — %s", resp.Status, ghErrorBody(resp))
		if wait, ok := rateLimitWait(resp, err.Error()); ok {
			return "", &rateLimitedError{wait: wait, err: err}
		}
		return "", err
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

// rateLimitedError is GitHub telling us to slow down, and for how long.
type rateLimitedError struct {
	wait time.Duration
	err  error
}

func (e *rateLimitedError) Error() string { return e.err.Error() }
func (e *rateLimitedError) Unwrap() error { return e.err }

// rateLimitWait reads how long GitHub wants us to wait: Retry-After, else the
// primary limit's reset time, else a minute for a secondary-limit refusal.
func rateLimitWait(resp *http.Response, msg string) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second, true
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if d := time.Until(time.Unix(reset, 0)); d > 0 {
				return d, true
			}
		}
	}
	if strings.Contains(strings.ToLower(msg), "rate limit") {
		return time.Minute, true
	}
	return 0, false
}

// pace waits for a blob slot: a burst, then one per blobInterval, and none
// while GitHub has asked us to back off.
func (g *GitHubRelay) pace(ctx context.Context) error {
	for {
		g.paceMu.Lock()
		now := time.Now()
		if g.paceAt.IsZero() {
			g.paceTokens = blobBurst
		} else {
			g.paceTokens = min(blobBurst, g.paceTokens+float64(now.Sub(g.paceAt))/float64(blobInterval))
		}
		g.paceAt = now
		var wait time.Duration
		switch {
		case now.Before(g.pauseUntil):
			wait = g.pauseUntil.Sub(now)
		case g.paceTokens >= 1:
			g.paceTokens--
			g.paceMu.Unlock()
			return nil
		default:
			wait = time.Duration((1 - g.paceTokens) * float64(blobInterval))
		}
		g.paceMu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// pauseBlobs stops all blob uploads for d.
func (g *GitHubRelay) pauseBlobs(d time.Duration) {
	g.paceMu.Lock()
	if until := time.Now().Add(d); until.After(g.pauseUntil) {
		g.pauseUntil = until
	}
	g.paceMu.Unlock()
	log.Printf("[gh-relay] GitHub asked to slow down: uploads paused for %s", d.Round(time.Second))
}

// Wants reports whether an upload of this file would be taken now: the relay
// lacks it (not committed, not queued, not being uploaded), it fits the size
// cap, and no backoff is refusing uploads.
func (g *GitHubRelay) Wants(size int64, crc uint32) bool {
	if g == nil || (g.cfg.MaxBytes > 0 && size > g.cfg.MaxBytes) {
		return false
	}
	key := protocol.RelayObjectName(size, crc, g.passphrase)
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.retryAfter) {
		return false
	}
	_, known := g.known[key]
	_, ready := g.ready[key]
	return !known && !ready && !g.uploading[key]
}

func (g *GitHubRelay) createTree(ctx context.Context, baseTree string, entries any) (string, error) {
	payload := map[string]any{"tree": entries}
	if baseTree != "" {
		payload["base_tree"] = baseTree
	}
	body, _ := json.Marshal(payload)
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
		return "", fmt.Errorf("%s — %s", resp.Status, ghErrorBody(resp))
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
	if parents == nil {
		parents = []string{}
	}
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
		return "", fmt.Errorf("%s — %s", resp.Status, ghErrorBody(resp))
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
		return fmt.Errorf("%s — %s", resp.Status, ghErrorBody(resp))
	}
	return nil
}

// --- HTTP plumbing ----------------------------------------------------------

// ghErrorBody reads a short, log-safe error body from a non-2xx GitHub
// response. GitHub's 5xx pages ("Unicorn") are full HTML documents — we
// don't want them in the log. Truncate to 200 chars and replace HTML
// blobs with a one-line summary.
func ghErrorBody(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return trimErrBody(string(raw))
}

func trimErrBody(s string) string {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html") {
		return "(HTML response — GitHub backend issue, retry later)"
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func (g *GitHubRelay) newReq(ctx context.Context, method, urlPath string, body io.Reader) (*http.Request, error) {
	full := strings.TrimRight(githubAPI, "/") + urlPath
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "git-client/1.0")
	return req, nil
}
