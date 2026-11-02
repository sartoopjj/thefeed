package server

import (
	"fmt"
	"hash/crc32"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// expireAllMedia ages every cached file past its TTL and sweeps, as hours
// without a re-store would.
func expireAllMedia(t *testing.T, f *Feed) {
	t.Helper()
	mc := f.media
	mc.mu.Lock()
	for _, e := range mc.byKey {
		e.expiresAt = time.Now().Add(-time.Minute)
	}
	mc.mu.Unlock()
	mc.Sweep()
}

func avatarChannelsServe(f *Feed) (ok, total int) {
	for _, e := range f.ProfilePicsBundle().Entries {
		total++
		if e.DNSChannel == 0 {
			continue
		}
		if _, err := f.GetBlock(int(e.DNSChannel), 0); err == nil {
			ok++
		}
	}
	return ok, total
}

// A cycle that fetches nothing new (unchanged photos are not downloaded again)
// must still keep the advertised avatar channels alive.
func TestKeepProfilePicsOutlivesTheMediaTTL(t *testing.T) {
	f := newFeedWithMedia(t)
	f.SetProfilePics(map[string][]byte{"alice": fakeJPEG(1024), "bob": fakePNG(2048)})
	if ok, total := avatarChannelsServe(f); ok != total || total != 2 {
		t.Fatalf("fresh: %d of %d avatar channels serve", ok, total)
	}

	// Without a re-store the TTL takes them while the directory still points
	// at them: every avatar query is a name error.
	expireAllMedia(t, f)
	if ok, _ := avatarChannelsServe(f); ok != 0 {
		t.Fatalf("expected the swept channels to be gone, %d still serve", ok)
	}

	if n := f.KeepProfilePics(); n != 2 {
		t.Fatalf("kept %d, want 2", n)
	}
	if ok, total := avatarChannelsServe(f); ok != total || total != 2 {
		t.Fatalf("after keep: %d of %d avatar channels serve", ok, total)
	}
}

// Kept in time, the same channels simply live on.
func TestKeepProfilePicsExtendsTheTTL(t *testing.T) {
	f := newFeedWithMedia(t)
	f.SetProfilePics(map[string][]byte{"alice": fakeJPEG(1024)})
	ch := f.ProfilePicsBundle().Entries[0].DNSChannel
	mc := f.media
	mc.mu.Lock()
	for _, e := range mc.byKey {
		e.expiresAt = time.Now().Add(time.Second)
	}
	mc.mu.Unlock()
	f.KeepProfilePics()
	if got := f.ProfilePicsBundle().Entries[0].DNSChannel; got != ch {
		t.Fatalf("channel moved %d -> %d", ch, got)
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if e := mc.byChannel[ch]; e == nil || time.Until(e.expiresAt) < 30*time.Minute {
		t.Fatal("the avatar's life was not extended")
	}
}

// Nothing to keep: no empty bundle is published.
func TestKeepProfilePicsWithNoAvatars(t *testing.T) {
	f := newFeedWithMedia(t)
	if n := f.KeepProfilePics(); n != 0 || len(f.ProfilePicsBundle().Entries) != 0 {
		t.Fatalf("kept %d", n)
	}
}

// Every avatar reader goes through RefreshProfilePics, owning its own part of
// the bundle.
func TestAvatarReadersRefreshTheirOwnEntries(t *testing.T) {
	for file, want := range map[string]string{
		"profile_pic_telegram.go": "tr.feed.RefreshProfilePics(pics, current, false)",
		"public_profile_pics.go":  "pr.feed.RefreshProfilePics(pics, current, false)",
		"xpublic_profile_pics.go": "xr.feed.RefreshProfilePics(pics, current, true)",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s: no longer refreshes its avatars", file)
		}
		if strings.Contains(string(src), "MergeProfilePics(") {
			t.Errorf("%s: merges around the refresh", file)
		}
	}
}

func bundleNames(f *Feed) []string {
	var out []string
	for _, e := range f.ProfilePicsBundle().Entries {
		out = append(out, e.Username)
	}
	return out
}

// Directory and cache agree: each entry's channel holds that entry's bytes.
func directoryConsistent(t *testing.T, f *Feed) {
	t.Helper()
	mc := f.media
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	for _, e := range f.ProfilePicsBundle().Entries {
		if e.DNSChannel == 0 {
			continue
		}
		held, ok := mc.byChannel[e.DNSChannel]
		if !ok {
			t.Errorf("%s: channel %d serves nothing", e.Username, e.DNSChannel)
			continue
		}
		if held.crc32 != e.CRC {
			t.Errorf("%s: channel %d holds another picture", e.Username, e.DNSChannel)
		}
	}
}

// A channel taken off the list loses its avatar on the next cycle; another
// reader's entries are not its to drop.
func TestRefreshDropsChannelsGoneFromTheList(t *testing.T) {
	f := newFeedWithMedia(t)
	f.SetProfilePics(map[string][]byte{"a": fakeJPEG(1024), "b": fakeJPEG(1024), "x:c": fakeJPEG(1024)})
	total, dropped := f.RefreshProfilePics(nil, map[string]bool{"a": true}, false)
	if total != 2 || len(dropped) != 1 || dropped[0] != "b" {
		t.Fatalf("total %d dropped %v", total, dropped)
	}
	if got := strings.Join(bundleNames(f), ","); got != "a,x:c" {
		t.Fatalf("bundle %s", got)
	}
	directoryConsistent(t, f)
}

// A photo taken down in Telegram is taken down here.
func TestRefreshDropsAPhotoTakenDown(t *testing.T) {
	f := newFeedWithMedia(t)
	f.SetProfilePics(map[string][]byte{"a": fakeJPEG(1024), "b": fakeJPEG(1024)})
	_, dropped := f.RefreshProfilePics(map[string][]byte{"a": nil}, map[string]bool{"a": true, "b": true}, false)
	if len(dropped) != 1 || strings.Join(bundleNames(f), ",") != "b" {
		t.Fatalf("dropped %v, bundle %v", dropped, bundleNames(f))
	}
	// A channel that never had one is not a change.
	if _, dropped := f.RefreshProfilePics(map[string][]byte{"z": nil}, map[string]bool{"b": true, "z": true}, false); dropped != nil {
		t.Fatalf("dropped %v", dropped)
	}
}

// The same picture fetched again is not stored again.
func TestRefreshUnchangedIsNoCopy(t *testing.T) {
	a := fakeJPEG(20 * 1024)
	f := manyAvatars(t, 100, 20*1024)
	f.SetProfilePics(map[string][]byte{"a": a})
	cur := map[string]bool{"a": true}
	ch := f.ProfilePicsBundle().Entries[0].DNSChannel
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	f.RefreshProfilePics(map[string][]byte{"a": a}, cur, false)
	runtime.ReadMemStats(&m1)
	if got := m1.TotalAlloc - m0.TotalAlloc; got > 20*1024 {
		t.Fatalf("an unchanged avatar was stored again (%d bytes)", got)
	}
	if f.ProfilePicsBundle().Entries[0].DNSChannel != ch {
		t.Fatal("the directory was rebuilt")
	}
}

// Two channels swapping pictures: each ends up with its new one, never the
// other's.
func TestRefreshNeverMixesAvatarsUp(t *testing.T) {
	a, b := fakeJPEG(3000), fakePNG(5000)
	f := newFeedWithMedia(t)
	f.SetProfilePics(map[string][]byte{"a": a, "b": b})
	cur := map[string]bool{"a": true, "b": true}
	f.RefreshProfilePics(map[string][]byte{"a": b, "b": a}, cur, false)
	for _, e := range f.ProfilePicsBundle().Entries {
		want := map[string][]byte{"a": b, "b": a}[e.Username]
		if e.CRC != crc32.ChecksumIEEE(want) || e.Size != uint32(len(want)) {
			t.Errorf("%s: directory describes the wrong picture", e.Username)
		}
	}
	directoryConsistent(t, f)

	// Their channels evicted and handed to other files: the next cycle must
	// not leave the directory pointing at someone else's bytes.
	expireAllMedia(t, f)
	for i := 0; i < 4; i++ {
		f.media.Store(fmt.Sprintf("post-%d", i), "[IMAGE]", fakeJPEG(2000), "image/jpeg", "p.jpg")
	}
	f.RefreshProfilePics(nil, cur, false)
	directoryConsistent(t, f)
	if ok, total := avatarChannelsServe(f); ok != total || total != 2 {
		t.Fatalf("%d of %d avatar channels serve", ok, total)
	}
}

// A dropped avatar is no longer kept, so the cache lets it go.
func TestDroppedAvatarsAreReclaimed(t *testing.T) {
	f := newFeedWithMedia(t)
	f.SetProfilePics(map[string][]byte{"a": fakeJPEG(1024), "b": fakeJPEG(1024)})
	f.RefreshProfilePics(nil, map[string]bool{"a": true}, false)
	mc := f.media
	mc.mu.Lock()
	for k, e := range mc.byKey {
		if k == profilePicKey("b") {
			e.expiresAt = time.Now().Add(-time.Minute)
		}
	}
	mc.mu.Unlock()
	f.RefreshProfilePics(nil, map[string]bool{"a": true}, false)
	mc.Sweep()
	if _, ok := mc.Lookup(profilePicKey("b")); ok {
		t.Error("the dropped avatar is still held")
	}
	if _, ok := mc.Lookup(profilePicKey("a")); !ok {
		t.Error("the kept avatar went too")
	}
}

// manyAvatars is a feed with a default-sized DNS relay (no GitHub) and n
// avatars of size bytes: together far over the per-file cap.
func manyAvatars(t *testing.T, n, size int) *Feed {
	t.Helper()
	f := NewFeed([]string{"a"})
	f.SetMediaCache(NewMediaCache(MediaCacheConfig{MaxFileBytes: 100 * 1024, TTL: time.Hour, DNSRelayEnabled: true}))
	pics := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		pics[fmt.Sprintf("ch%03d", i)] = fakeJPEG(size)
	}
	f.SetProfilePics(pics)
	return f
}

// The bundle feeds only the GitHub relay. Too large to store, it must not take
// the per-avatar DNS channels down with it.
func TestProfilePicsGoOutWithoutTheBundle(t *testing.T) {
	f := manyAvatars(t, 8, 20*1024)
	b := f.ProfilePicsBundle()
	if len(b.Entries) != 8 {
		t.Fatalf("directory has %d entries, want 8", len(b.Entries))
	}
	if b.Header.BundleSize != 0 || b.Header.HasRelay(1) {
		t.Errorf("a bundle that was never stored is advertised: %+v", b.Header)
	}
	if ok, total := avatarChannelsServe(f); ok != total {
		t.Fatalf("%d of %d avatar channels serve", ok, total)
	}
}

// A quiet cycle extends the avatars' lives without copying them: 300 avatars of
// 20 KB (6 MB) cost less than one avatar's worth of allocation.
func TestKeepProfilePicsDoesNotCopy(t *testing.T) {
	f := manyAvatars(t, 300, 20*1024)
	before := f.ProfilePicsBundle().Entries[150].DNSChannel
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	n := f.KeepProfilePics()
	runtime.ReadMemStats(&b)
	if n != 300 {
		t.Fatalf("kept %d", n)
	}
	if got := b.TotalAlloc - a.TotalAlloc; got > 20*1024 {
		t.Fatalf("a quiet cycle allocated %d bytes", got)
	}
	if after := f.ProfilePicsBundle().Entries[150].DNSChannel; after != before {
		t.Fatalf("the directory was rebuilt (channel %d -> %d)", before, after)
	}
}

// An avatar too large for its own DNS channel is never in the cache, so it
// must not make every quiet cycle store the whole bundle again.
func TestKeepProfilePicsIgnoresAvatarsWithoutAChannel(t *testing.T) {
	f := NewFeed([]string{"a"})
	f.SetMediaCache(NewMediaCache(MediaCacheConfig{MaxFileBytes: 64 * 1024, TTL: time.Hour, DNSRelayEnabled: true}))
	f.SetProfilePics(map[string][]byte{"big": fakeJPEG(90 * 1024), "small": fakeJPEG(4 * 1024)})
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f.KeepProfilePics()
	runtime.ReadMemStats(&b)
	if got := b.TotalAlloc - a.TotalAlloc; got > 20*1024 {
		t.Fatalf("a quiet cycle stored everything again (%d bytes)", got)
	}
}
