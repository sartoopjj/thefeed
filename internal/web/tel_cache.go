package web

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// Telegram message cache.
//
// Sealed with the same key as the session (see tel_crypto.go), because it holds
// the same private content. It exists for two reasons:
//
//   - the section opens instantly instead of spending a minute of queries
//     rebuilding a list the user already saw;
//   - a poll can then ask only "how many are new" rather than fetching every
//     conversation's last message, since the text to show beside each row is
//     already here.
//
// Unlike the session this is DISPOSABLE. Anything unreadable is dropped and
// rebuilt from Telegram: the cost is queries, not data. The session takes the
// opposite rule, and conflating the two is how a cache bug turns into a logout.
const (
	telCacheVersion = 1
	// Bounds, so a long-lived install cannot grow without limit. Chosen to
	// cover what a user actually scrolls back through, not whole histories.
	telCacheMaxThreads   = 300
	telCacheMaxPerThread = 60
)

type telThreadEntry struct {
	Msgs []tgbridge.Message `json:"msgs"`
	// Touched orders eviction: the least recently read thread goes first.
	Touched int64 `json:"touched"`
}

type telCacheData struct {
	Version int                       `json:"version"`
	Dialogs []tgbridge.Dialog         `json:"dialogs,omitempty"`
	Threads map[string]telThreadEntry `json:"threads,omitempty"`
	// DialogsComplete records that Telegram itself said there is no page
	// after the last one we fetched. It is the ONLY reliable end marker:
	// DialogTotal counts every dialog the account has, including rows this
	// client cannot map (a dialog whose entity did not come with the page is
	// dropped), so "held < total" can be true for ever. That is what left a
	// list sitting on "197 of 198" with a button that fetched nothing.
	DialogsComplete bool `json:"dialogsComplete,omitempty"`
	// DialogTotal is how many conversations Telegram says the account has, as
	// of the last live page that said so. Kept because the cache otherwise has
	// no way to know it is partial: holding forty rows of three hundred looks
	// exactly like holding all forty of forty, and the difference decides
	// whether the list offers to fetch the rest.
	DialogTotal int `json:"dialogTotal,omitempty"`
	// Cursor is how far through Telegram's update stream we have read. Kept
	// with the conversations because the two only make sense together: a
	// cursor without the messages it refers to would claim everything is
	// current while the list is empty.
	Cursor tgbridge.UpdateState `json:"cursor,omitzero"`
}

type telCache struct {
	mu   sync.Mutex
	path string
	tc   *telCrypto
	data telCacheData
	seq  int64 // monotonic stand-in for a clock, for eviction order
}

// telCacheKey names a conversation the same way the frontend does, so the two
// sides agree without translating.
func telCacheKey(p tgbridge.Peer) string { return fmt.Sprintf("%s:%d", p.Kind, p.ID) }

func telCachePath(dir string) string { return filepath.Join(dir, "cache.bin") }

// loadTelCache reads the cache, or returns an empty one. It never fails: a
// cache that cannot be read is a cache that has to be refilled, which costs
// queries and nothing else.
func loadTelCache(dir string) *telCache {
	c := &telCache{path: telCachePath(dir)}
	c.data = telCacheData{Version: telCacheVersion, Threads: map[string]telThreadEntry{}}

	tc, err := loadTelCrypto(dir)
	if err != nil {
		log.Printf("tel cache: no key (%v) — running without a cache", err)
		return c
	}
	c.tc = tc

	raw, err := os.ReadFile(c.path)
	if err != nil || len(raw) == 0 {
		return c
	}
	plain, err := tc.open(raw)
	if err != nil {
		// Locked, or sealed under a key we no longer have. Drop it rather than
		// locking: refilling is cheap compared with being stuck.
		log.Printf("tel cache: unreadable (%v) — starting empty", err)
		return c
	}
	var data telCacheData
	if err := json.Unmarshal(plain, &data); err != nil || data.Version != telCacheVersion {
		return c
	}
	if data.Threads == nil {
		data.Threads = map[string]telThreadEntry{}
	}
	c.data = data
	for _, t := range data.Threads {
		if t.Touched > c.seq {
			c.seq = t.Touched
		}
	}
	return c
}

// save writes the cache back, sealed. A locked key means no write: plaintext
// here would leak exactly what the sealing is for.
func (c *telCache) save() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saveLocked()
}

func (c *telCache) saveLocked() {
	if c.tc == nil {
		return
	}
	data, err := json.Marshal(c.data)
	if err != nil {
		return
	}
	sealed, err := c.tc.seal(data)
	if err != nil {
		return // locked
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	if err := writeFileAtomic(c.path, sealed, 0o600); err != nil {
		log.Printf("tel cache: write failed: %v", err)
	}
}

// Dialogs returns the cached chat list, newest activity first.
func (c *telCache) Dialogs() []tgbridge.Dialog {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tgbridge.Dialog(nil), c.data.Dialogs...)
}

// PutDialogs replaces the cached list. Only a full first page does this; a
// later page would otherwise truncate the list it was extending.
func (c *telCache) PutDialogs(ds []tgbridge.Dialog) {
	if c == nil || len(ds) == 0 {
		return
	}
	c.mu.Lock()
	c.data.Dialogs = append([]tgbridge.Dialog(nil), ds...)
	c.mu.Unlock()
	c.save()
}

// DialogTotal is how many conversations Telegram last said the account has,
// or 0 when it has never said.
func (c *telCache) DialogTotal() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.DialogTotal
}

// SetDialogTotal records Telegram's count. Only a live page knows it; a zero
// is "did not say", never "the account has none", so it is not stored.
func (c *telCache) SetDialogTotal(n int) {
	if c == nil || n <= 0 {
		return
	}
	c.mu.Lock()
	changed := c.data.DialogTotal != n
	c.data.DialogTotal = n
	c.mu.Unlock()
	if changed {
		c.save()
	}
}

// MergeDialogs adds rows from a LATER page, keeping what is already held.
// PutDialogs replaces the list, which is right for a first page and drops
// every page after it -- so a page the user paid DNS queries for is paid for
// again on the next start.
func (c *telCache) MergeDialogs(ds []tgbridge.Dialog) int {
	if c == nil || len(ds) == 0 {
		return 0
	}
	c.mu.Lock()
	have := make(map[string]int, len(c.data.Dialogs))
	for i := range c.data.Dialogs {
		have[telCacheKey(c.data.Dialogs[i].Peer)] = i
	}
	added := 0
	for _, d := range ds {
		if i, ok := have[telCacheKey(d.Peer)]; ok {
			// Already listed: take the fresher row, since this page read it
			// from Telegram just now.
			c.data.Dialogs[i] = d
			continue
		}
		have[telCacheKey(d.Peer)] = len(c.data.Dialogs)
		c.data.Dialogs = append(c.data.Dialogs, d)
		added++
	}
	c.mu.Unlock()
	c.save()
	return added
}

// SetDialogsComplete records whether Telegram said the list ends here.
func (c *telCache) SetDialogsComplete(done bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	changed := c.data.DialogsComplete != done
	c.data.DialogsComplete = done
	c.mu.Unlock()
	if changed {
		c.save()
	}
}

// DialogsComplete reports whether the whole list is held.
func (c *telCache) DialogsComplete() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.DialogsComplete
}

// Thread returns the cached messages for a conversation, oldest first.
func (c *telCache) Thread(key string) []tgbridge.Message {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.data.Threads[key]
	if !ok {
		return nil
	}
	return append([]tgbridge.Message(nil), t.Msgs...)
}

// MergeThread folds new messages in by id, keeping the newest telCacheMaxPerThread.
func (c *telCache) MergeThread(key string, msgs []tgbridge.Message) {
	if c == nil || len(msgs) == 0 {
		return
	}
	c.mu.Lock()
	t := c.data.Threads[key]
	byID := make(map[int]tgbridge.Message, len(t.Msgs)+len(msgs))
	for _, m := range t.Msgs {
		byID[m.ID] = m
	}
	// New wins: an edited message keeps its id and must replace the old text.
	for _, m := range msgs {
		byID[m.ID] = m
	}
	merged := make([]tgbridge.Message, 0, len(byID))
	for _, m := range byID {
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	if len(merged) > telCacheMaxPerThread {
		merged = merged[len(merged)-telCacheMaxPerThread:]
	}
	c.seq++
	c.data.Threads[key] = telThreadEntry{Msgs: merged, Touched: c.seq}
	c.evictLocked()
	c.mu.Unlock()
	c.save()
}

// ForgetPeer drops one conversation: its row and its messages. Used when
// Telegram says the account is no longer in it, so a channel left on another
// device stops being listed here without re-reading the whole list.
func (c *telCache) ForgetPeer(keys []string) int {
	if c == nil || len(keys) == 0 {
		return 0
	}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	c.mu.Lock()
	kept := c.data.Dialogs[:0]
	removed := 0
	for _, d := range c.data.Dialogs {
		if drop[telCacheKey(d.Peer)] {
			removed++
			continue
		}
		kept = append(kept, d)
	}
	c.data.Dialogs = kept
	for k := range drop {
		if _, ok := c.data.Threads[k]; ok {
			delete(c.data.Threads, k)
			removed++
		}
	}
	c.mu.Unlock()
	if removed > 0 {
		c.save()
	}
	return removed
}

// ApplyReads records how far the other side has read in each conversation.
// This is the second tick: without it a message read after the short
// post-send polling window stayed on one tick for ever.
func (c *telCache) ApplyReads(reads []tgbridge.PeerRead) int {
	if c == nil || len(reads) == 0 {
		return 0
	}
	c.mu.Lock()
	changed := 0
	for _, r := range reads {
		key := telCacheKey(r.Peer)
		for i := range c.data.Dialogs {
			if telCacheKey(c.data.Dialogs[i].Peer) != key {
				continue
			}
			// Only ever forwards. A stale update must not un-read messages.
			if r.MaxID > c.data.Dialogs[i].ReadOutbox {
				c.data.Dialogs[i].ReadOutbox = r.MaxID
				changed++
			}
			break
		}
	}
	c.mu.Unlock()
	if changed > 0 {
		c.save()
	}
	return changed
}

// ApplyInReads records how far THIS account has read each conversation, from
// a read on another device. The only thing that clears an unread badge for a
// chat opened on the phone: a difference carries read marks, never
// conversation rows, so without this the badge stays up until something pays
// for the whole list again.
//
// The count is Telegram's own still-unread number, not len(messages) minus
// something -- a conversation never scrolled has messages we have not got.
//
// Reports how many rows changed, so a no-op costs no write.
func (c *telCache) ApplyInReads(reads []tgbridge.PeerInRead) int {
	if c == nil || len(reads) == 0 {
		return 0
	}
	c.mu.Lock()
	changed := 0
	for _, r := range reads {
		key := telCacheKey(r.Peer)
		for i := range c.data.Dialogs {
			if telCacheKey(c.data.Dialogs[i].Peer) != key {
				continue
			}
			// The mark only ever moves forwards. An update overtaken by a
			// later one must not un-read what has since been read.
			if r.MaxID > c.data.Dialogs[i].ReadInbox {
				c.data.Dialogs[i].ReadInbox = r.MaxID
				changed++
			}
			// The count follows the mark, in whichever direction Telegram
			// says: it is the authority on it, and this is how a badge comes
			// down to zero. Gated on the mark having moved so a stale update
			// cannot raise a count that a newer one already settled.
			if r.MaxID >= c.data.Dialogs[i].ReadInbox && r.Unread != c.data.Dialogs[i].Unread {
				c.data.Dialogs[i].Unread = r.Unread
				changed++
			}
			break
		}
	}
	c.mu.Unlock()
	if changed > 0 {
		c.save()
	}
	return changed
}

// ApplyMutes records notification settings changed on another device.
//
// Unlike a read mark this moves in both directions -- muting and unmuting are
// equally real -- so the only rule is that the conversation has to be one we
// already list. The flag also decides whether an arriving message is allowed
// to make a sound, so a stale one is heard, not just seen.
func (c *telCache) ApplyMutes(mutes []tgbridge.PeerMute) int {
	if c == nil || len(mutes) == 0 {
		return 0
	}
	c.mu.Lock()
	changed := 0
	for _, m := range mutes {
		key := telCacheKey(m.Peer)
		for i := range c.data.Dialogs {
			if telCacheKey(c.data.Dialogs[i].Peer) != key {
				continue
			}
			if c.data.Dialogs[i].Muted != m.Muted {
				c.data.Dialogs[i].Muted = m.Muted
				changed++
			}
			break
		}
	}
	c.mu.Unlock()
	if changed > 0 {
		c.save()
	}
	return changed
}

// ApplyStats refreshes view and reaction counts on messages already held,
// leaving everything else untouched. A post read in its first minute keeps the
// counts it had then unless something comes back to update them, which is what
// this is: the numbers move for days after the text stops changing.
//
// Reports how many rows actually changed, so a caller can skip a write and a
// re-render when Telegram says the same thing twice.
func (c *telCache) ApplyStats(key string, stats []tgbridge.MessageStats) int {
	if c == nil || len(stats) == 0 {
		return 0
	}
	c.mu.Lock()
	t, ok := c.data.Threads[key]
	if !ok {
		c.mu.Unlock()
		return 0
	}
	byID := make(map[int]tgbridge.MessageStats, len(stats))
	for _, s := range stats {
		byID[s.ID] = s
	}
	changed := 0
	for i := range t.Msgs {
		s, ok := byID[t.Msgs[i].ID]
		if !ok {
			continue
		}
		// A missing count is Telegram declining to say, not a drop to zero:
		// overwriting with it would wipe numbers we already have.
		if s.Views > t.Msgs[i].Views {
			t.Msgs[i].Views = s.Views
			changed++
		}
		if len(s.Reactions) > 0 && !sameReactions(t.Msgs[i].Reactions, s.Reactions) {
			t.Msgs[i].Reactions = s.Reactions
			changed++
		}
	}
	if changed > 0 {
		c.data.Threads[key] = t
	}
	c.mu.Unlock()
	if changed > 0 {
		c.save()
	}
	return changed
}

func sameReactions(a, b []tgbridge.Reaction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// evictLocked drops the least recently touched threads past the cap.
func (c *telCache) evictLocked() {
	if len(c.data.Threads) <= telCacheMaxThreads {
		return
	}
	keys := make([]string, 0, len(c.data.Threads))
	for k := range c.data.Threads {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return c.data.Threads[keys[i]].Touched < c.data.Threads[keys[j]].Touched
	})
	for _, k := range keys[:len(c.data.Threads)-telCacheMaxThreads] {
		delete(c.data.Threads, k)
	}
}

// export returns the cache as plain JSON, for a backup that is itself
// encrypted. Nothing here is key material: it is conversations and messages.
func (c *telCache) export() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("tel cache: unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data.Dialogs) == 0 && len(c.data.Threads) == 0 {
		return nil, fmt.Errorf("tel cache: empty")
	}
	return json.Marshal(c.data)
}

// importFrom replaces the cache from a backup, re-sealing under THIS device's
// key. Bad input is ignored rather than fatal: a cache is rebuildable, and a
// restore must not fail over it.
func (c *telCache) importFrom(plain []byte) {
	if c == nil {
		return
	}
	var data telCacheData
	if err := json.Unmarshal(plain, &data); err != nil || data.Version != telCacheVersion {
		log.Printf("tel cache: backup not restored (unreadable or wrong version)")
		return
	}
	if data.Threads == nil {
		data.Threads = map[string]telThreadEntry{}
	}
	c.mu.Lock()
	c.data = data
	// Pick the eviction clock back up past the restored threads, exactly as
	// loading from disk does. Without it the counter restarts at zero while
	// every restored thread carries a much larger Touched, so the next thread
	// the user opens is stamped lowest and is the first one evicted -- the one
	// they are reading.
	for _, t := range data.Threads {
		if t.Touched > c.seq {
			c.seq = t.Touched
		}
	}
	c.evictLocked()
	c.mu.Unlock()
	c.save()
}

// Cursor returns the stored update cursor.
func (c *telCache) Cursor() tgbridge.UpdateState {
	if c == nil {
		return tgbridge.UpdateState{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Cursor
}

// SetCursor records how far the update stream has been read.
func (c *telCache) SetCursor(st tgbridge.UpdateState) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.data.Cursor = st
	c.mu.Unlock()
	c.save()
}

// ApplyNew folds a difference into the cache: the messages into their threads,
// and the row each belongs to to the top with its preview and unread count.
//
// Returns how many arrivals are worth INTERRUPTING for. A muted conversation
// still counts unread and still moves up the list -- it just does not raise a
// notification, which is the only thing muting is for.
func (c *telCache) ApplyNew(groups []tgbridge.PeerMessages) int {
	if c == nil {
		return 0
	}
	notifiable := 0
	for _, g := range groups {
		key := telCacheKey(g.Peer)
		c.MergeThread(key, g.Messages)

		var last tgbridge.Message
		fresh := 0
		for _, m := range g.Messages {
			if m.ID > last.ID {
				last = m
			}
			if !m.Out && !m.Service {
				fresh++
			}
		}
		if fresh > 0 && !c.muted(key) {
			notifiable += fresh
		}
		c.bumpRow(g.Peer, last, fresh)
	}
	c.save()
	return notifiable
}

// muted reports the cached notification setting for a conversation. A chat we
// have never listed is treated as audible: better one notification too many
// than silently swallowing the first message from a new contact.
func (c *telCache) muted(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.data.Dialogs {
		if telCacheKey(c.data.Dialogs[i].Peer) == key {
			return c.data.Dialogs[i].Muted
		}
	}
	return false
}

// bumpRow updates (or inserts) one conversation's row and moves it to the top,
// the way an arriving message reorders the list.
func (c *telCache) bumpRow(p tgbridge.Peer, last tgbridge.Message, unread int) {
	key := telCacheKey(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	at := -1
	for i := range c.data.Dialogs {
		if telCacheKey(c.data.Dialogs[i].Peer) == key {
			at = i
			break
		}
	}
	// Never listed: everything about this row except the message that just
	// arrived is a guess, and the notification setting most of all.
	row := tgbridge.Dialog{Peer: p, NotifyUnknown: true}
	if at >= 0 {
		row = c.data.Dialogs[at]
		c.data.Dialogs = append(c.data.Dialogs[:at], c.data.Dialogs[at+1:]...)
	}
	// The peer from a difference is fresher than the cached one (a renamed
	// channel, new rights), so it replaces it. Muted is NOT in the peer and
	// is carried over: a difference says nothing about notification settings,
	// and losing the flag here would make a muted channel start ringing.
	row.Peer = p
	if last.ID > 0 {
		row.TopMsg, row.When = last.ID, last.Date
		if last.Text != "" {
			row.Preview = last.Text
		}
	}
	row.Unread += unread
	c.data.Dialogs = append([]tgbridge.Dialog{row}, c.data.Dialogs...)
}

// UnknownNotify lists conversations whose notification setting the cache has
// never been told, newest first. These are rows a difference created: it names
// a conversation and carries its message, but says nothing about whether the
// user silenced it.
func (c *telCache) UnknownNotify(max int) []tgbridge.Peer {
	if c == nil || max <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []tgbridge.Peer
	for i := range c.data.Dialogs {
		if !c.data.Dialogs[i].NotifyUnknown {
			continue
		}
		out = append(out, c.data.Dialogs[i].Peer)
		if len(out) >= max {
			break
		}
	}
	return out
}

// ClearNotifyUnknown marks conversations as asked-about, whatever came back.
// A peer Telegram was asked for and did not describe is not a dialog it will
// ever describe, so leaving it marked would buy one wasted call per poll for
// as long as the row exists.
func (c *telCache) ClearNotifyUnknown(keys []string) {
	if c == nil || len(keys) == 0 {
		return
	}
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	c.mu.Lock()
	changed := 0
	for i := range c.data.Dialogs {
		if c.data.Dialogs[i].NotifyUnknown && want[telCacheKey(c.data.Dialogs[i].Peer)] {
			c.data.Dialogs[i].NotifyUnknown = false
			changed++
		}
	}
	c.mu.Unlock()
	if changed > 0 {
		c.save()
	}
}

// SetMuted records a conversation's notification setting locally, so the list
// reflects a change before Telegram's own copy comes back round.
func (c *telCache) SetMuted(key string, muted bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for i := range c.data.Dialogs {
		if telCacheKey(c.data.Dialogs[i].Peer) == key {
			c.data.Dialogs[i].Muted = muted
			break
		}
	}
	c.mu.Unlock()
	c.save()
}

// Forget drops everything. Used when the account signs out: the next account
// must not inherit the previous one's conversations.
func (c *telCache) Forget() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.data = telCacheData{Version: telCacheVersion, Threads: map[string]telThreadEntry{}}
	c.mu.Unlock()
	_ = os.Remove(c.path)
}
