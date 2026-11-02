package server

import (
	"os"
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

// Every avatar reader keeps the bundle on a cycle with nothing new. The
// Telegram one skips unchanged photos, so after its first cycle that is every
// cycle.
func TestAvatarReadersKeepOnQuietCycles(t *testing.T) {
	for file, recv := range map[string]string{
		"profile_pic_telegram.go": "tr",
		"public_profile_pics.go":  "pr",
		"xpublic_profile_pics.go": "xr",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		want := "if len(pics) == 0 {\n\t\t" + recv + ".feed.KeepProfilePics()\n\t\treturn\n\t}"
		if !strings.Contains(string(src), want) {
			t.Errorf("%s: a quiet cycle no longer keeps the avatars", file)
		}
	}
}
