package web

import (
	"context"
	"os"
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// The old behaviour copied the SEALED file. On a device with a different key
// that is unreadable, and unreadable now means wiped -- a restore that signs
// the user out. This is the regression the decrypt-on-export design prevents.
func TestRestoringSealedBytesWouldLoseTheAccount(t *testing.T) {
	oldDir := t.TempDir()
	if err := sessionFor(t, oldDir).StoreSession(context.Background(), []byte(`{"auth":"x"}`)); err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(sessionFor(t, oldDir).Path)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the naive restore: drop the sealed bytes onto a fresh device.
	newDir := t.TempDir()
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionFor(t, newDir).Path, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionFor(t, newDir).LoadSession(context.Background()); err == nil {
		t.Fatal("sealed bytes from another device must not read as a valid session")
	}
}

// A backup is restored section by section, so the cache may arrive without the
// session or the other way round. Neither may break the other.
func TestSessionAndCacheRestoreIndependently(t *testing.T) {
	dir := t.TempDir()
	// Cache only.
	loadTelCache(dir).importFrom([]byte(`{"version":1,"dialogs":[{"peer":{"kind":"channel","id":"1","accessHash":"0","title":"c"}}]}`))
	if len(loadTelCache(dir).Dialogs()) != 1 {
		t.Fatal("a cache-only restore did not take")
	}
	if _, err := os.Stat(sessionFor(t, dir).Path); !os.IsNotExist(err) {
		t.Fatal("a cache-only restore invented a session")
	}

	// Then the session arrives.
	if err := sessionFor(t, dir).StoreSession(context.Background(), []byte(`{"auth":"later"}`)); err != nil {
		t.Fatal(err)
	}
	if len(loadTelCache(dir).Dialogs()) != 1 {
		t.Fatal("restoring the session dropped the cache")
	}
}

// Rubbish in a backup must not take the restore down with it.
func TestCorruptCacheBackupIsIgnored(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	c.PutDialogs([]tgbridge.Dialog{{Peer: tgbridge.Peer{Kind: "channel", ID: 1, Title: "keep"}}})
	c.importFrom([]byte("}{ not json"))
	if got := loadTelCache(dir).Dialogs(); len(got) != 1 || got[0].Peer.Title != "keep" {
		t.Fatalf("a corrupt backup disturbed the existing cache: %+v", got)
	}
}
