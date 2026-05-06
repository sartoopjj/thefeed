package telemirror

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageCachePutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := NewImageCache(filepath.Join(dir, "images"))

	url := "https://cdn4-telegram-org.translate.goog/file/abc.jpg"
	want := []byte("\xff\xd8\xff\xe0fake-jpeg")
	if err := c.Put(url, "image/jpeg", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ctype, ok := c.Get(url)
	if !ok {
		t.Fatalf("Get: miss after Put")
	}
	if ctype != "image/jpeg" {
		t.Errorf("ctype = %q, want image/jpeg", ctype)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func TestImageCacheSurvivesNewInstance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	first := NewImageCache(dir)
	url := "https://cdn1-telesco-pe.translate.goog/file/avatar.jpg"
	body := []byte("avatar-bytes")
	if err := first.Put(url, "image/jpeg", body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	second := NewImageCache(dir)
	got, ctype, ok := second.Get(url)
	if !ok {
		t.Fatalf("Get on fresh instance: miss")
	}
	if ctype != "image/jpeg" || !bytes.Equal(got, body) {
		t.Errorf("got (%q, %q), want (%q, %q)", ctype, got, "image/jpeg", body)
	}
}

func TestImageCacheGetMissOnEmpty(t *testing.T) {
	c := NewImageCache(t.TempDir())
	if _, _, ok := c.Get(""); ok {
		t.Errorf("Get(\"\") = ok, want miss")
	}
	if _, _, ok := c.Get("https://nope.translate.goog/x.jpg"); ok {
		t.Errorf("Get on empty cache = ok, want miss")
	}
}

func TestImageCachePutRejectsEmptyInput(t *testing.T) {
	c := NewImageCache(t.TempDir())
	if err := c.Put("", "image/jpeg", []byte("x")); err == nil {
		t.Errorf("Put with empty url succeeded, want error")
	}
	if err := c.Put("https://x.translate.goog/a", "image/jpeg", nil); err == nil {
		t.Errorf("Put with empty body succeeded, want error")
	}
}

func TestImageCacheClearWipesEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c := NewImageCache(dir)
	if err := c.Put("https://a.translate.goog/1", "image/jpeg", []byte("a")); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	if err := c.Put("https://b.translate.goog/2", "image/png", []byte("b")); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	c.Clear()
	if _, _, ok := c.Get("https://a.translate.goog/1"); ok {
		t.Errorf("entry 1 survived Clear")
	}
	if _, _, ok := c.Get("https://b.translate.goog/2"); ok {
		t.Errorf("entry 2 survived Clear")
	}
	// Clear on empty/missing dir must not panic.
	c.Clear()
	c2 := NewImageCache(filepath.Join(t.TempDir(), "never-created"))
	c2.Clear()
}

func TestImageCachePutByKeyStableAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	first := NewImageCache(dir)
	body := []byte("avatar-bytes-v1")
	if err := first.PutByKey("networkti", "image/jpeg", body); err != nil {
		t.Fatalf("PutByKey: %v", err)
	}
	second := NewImageCache(dir)
	got, ctype, ok := second.GetByKey("networkti")
	if !ok {
		t.Fatalf("GetByKey on fresh instance: miss")
	}
	if ctype != "image/jpeg" || !bytes.Equal(got, body) {
		t.Errorf("got (%q, %q), want (%q, %q)", ctype, got, "image/jpeg", body)
	}
}

func TestImageCacheKeyByKeyNormalisesCase(t *testing.T) {
	c := NewImageCache(t.TempDir())
	if err := c.PutByKey("NetworkTI", "image/jpeg", []byte("x")); err != nil {
		t.Fatalf("PutByKey: %v", err)
	}
	if _, _, ok := c.GetByKey("networkti"); !ok {
		t.Errorf("lower-case lookup missed")
	}
	if _, _, ok := c.GetByKey("NETWORKTI"); !ok {
		t.Errorf("upper-case lookup missed")
	}
}

func TestImageCacheKeyByKeyStripsPathEscape(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	c := NewImageCache(cacheDir)
	_ = c.PutByKey("../../etc/passwd", "image/jpeg", []byte("x"))
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasPrefix(p, cacheDir+string(filepath.Separator)) && p != cacheDir {
			t.Errorf("file written outside cache dir: %s", p)
		}
		return nil
	})
}

func TestImageCacheStaleEntryEvicted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c := NewImageCache(dir)
	url := "https://stale.translate.goog/x.jpg"
	if err := c.Put(url, "image/jpeg", []byte("old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	key := c.keyFor(url)
	mb, err := json.Marshal(imageMeta{
		URL:         url,
		ContentType: "image/jpeg",
		StoredAt:    time.Now().Add(-2 * ImageStaleTTL),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(c.metaPath(key), mb, 0600); err != nil {
		t.Fatalf("rewrite meta: %v", err)
	}
	if _, _, ok := c.Get(url); ok {
		t.Errorf("stale entry returned hit")
	}
	if _, err := os.Stat(c.bodyPath(key)); !os.IsNotExist(err) {
		t.Errorf("body still on disk: err=%v", err)
	}
	if _, err := os.Stat(c.metaPath(key)); !os.IsNotExist(err) {
		t.Errorf("meta still on disk: err=%v", err)
	}
}
