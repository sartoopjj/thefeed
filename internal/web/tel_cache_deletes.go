package web

import (
	"strconv"
	"strings"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// Deleted messages. Telegram's API terms forbid keeping on screen what was
// deleted for everyone or by a self-destruct timer, so deletes are applied to
// the cache and remembered (tombstones) for the page to apply to its own copy.

// telThreadKind splits a thread key into its kind and id.
func telThreadKind(key string) (string, int64) {
	kind, id, ok := strings.Cut(key, ":")
	if !ok {
		return "", 0
	}
	n, _ := strconv.ParseInt(id, 10, 64)
	return kind, n
}

// boxThreadLocked says whether a thread's ids are the account's own box ids:
// private chats, bots and basic groups. A supergroup carries an access hash; a
// group whose row is unknown is neither, so nothing is removed from it.
func (c *telCache) boxThreadLocked(key string) bool {
	kind, _ := telThreadKind(key)
	switch tgbridge.PeerKind(kind) {
	case tgbridge.PeerUser, tgbridge.PeerBot:
		return true
	case tgbridge.PeerGroup:
		p, ok := c.peerLocked(key)
		return ok && p.AccessHash == 0
	}
	return false
}

// channelThreadLocked says whether a thread is the channel or supergroup id.
func (c *telCache) channelThreadLocked(key string, channelID int64) bool {
	kind, id := telThreadKind(key)
	if id != channelID {
		return false
	}
	switch tgbridge.PeerKind(kind) {
	case tgbridge.PeerChannel, tgbridge.PeerForum:
		return true
	case tgbridge.PeerGroup:
		p, ok := c.peerLocked(key)
		return ok && p.AccessHash != 0
	}
	return false
}

// peerLocked finds a conversation by key: a row, or a channel's direct
// messages, which have no row until they get one from Telegram.
func (c *telCache) peerLocked(key string) (tgbridge.Peer, bool) {
	for _, d := range c.data.Dialogs {
		if telCacheKey(d.Peer) == key {
			return d.Peer, true
		}
	}
	for _, p := range c.data.Directs {
		if telCacheKey(p) == key {
			return p, true
		}
	}
	return tgbridge.Peer{}, false
}

// ApplyDeletes removes deleted messages from the threads they belong to and
// remembers their ids. Returns how many cached messages went.
func (c *telCache) ApplyDeletes(dels []tgbridge.Deletion) int {
	if c == nil || len(dels) == 0 {
		return 0
	}
	c.mu.Lock()
	removed := 0
	for _, d := range dels {
		if d.ChannelID == 0 {
			c.data.BoxDeleted = addTombstones(c.data.BoxDeleted, d.IDs)
		}
		for key, t := range c.data.Threads {
			if d.ChannelID == 0 && !c.boxThreadLocked(key) {
				continue
			}
			if d.ChannelID != 0 && !c.channelThreadLocked(key, d.ChannelID) {
				continue
			}
			gone := intSet(d.IDs)
			kept := make([]tgbridge.Message, 0, len(t.Msgs))
			for _, m := range t.Msgs {
				if gone[m.ID] {
					removed++
					continue
				}
				kept = append(kept, m)
			}
			t.Msgs = kept
			if d.ChannelID != 0 {
				t.Deleted = addTombstones(t.Deleted, d.IDs)
			}
			c.data.Threads[key] = t
			c.rePreviewLocked(key, gone, kept)
		}
	}
	c.mu.Unlock()
	c.save()
	return removed
}

// rePreviewLocked moves a row's preview off a deleted message to the newest
// one still held, or clears it.
func (c *telCache) rePreviewLocked(key string, gone map[int]bool, kept []tgbridge.Message) {
	for i := range c.data.Dialogs {
		row := &c.data.Dialogs[i]
		if telCacheKey(row.Peer) != key || !gone[row.TopMsg] {
			continue
		}
		row.Preview = ""
		for j := len(kept) - 1; j >= 0; j-- {
			if !kept[j].Service {
				row.TopMsg, row.When = kept[j].ID, kept[j].Date
				if kept[j].Expires == 0 {
					row.Preview = kept[j].Text
				}
				break
			}
		}
		return
	}
}

// Deleted is the ids the page must drop from this conversation.
func (c *telCache) Deleted(key string) []int {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.deletedLocked(key)...)
}

func (c *telCache) deletedLocked(key string) []int {
	if c.boxThreadLocked(key) {
		return c.data.BoxDeleted
	}
	return c.data.Threads[key].Deleted
}

func addTombstones(list, ids []int) []int {
	have := intSet(list)
	for _, id := range ids {
		if !have[id] {
			list = append(list, id)
			have[id] = true
		}
	}
	if len(list) > telCacheMaxTombstones {
		list = list[len(list)-telCacheMaxTombstones:]
	}
	return list
}

func intSet(ids []int) map[int]bool {
	out := make(map[int]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}
