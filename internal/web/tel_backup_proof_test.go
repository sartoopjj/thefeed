package web

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

func TestBackupRestoreReSealsOnTheNewDevice(t *testing.T) {
	const pw = "pw-for-proof"
	secret := []byte(`{"auth_key":"SENTINEL-SESSION-VALUE"}`)

	a := t.TempDir()
	src := &Server{dataDir: a}
	src.tel = newTelHub(src, a)
	if err := src.tel.sessionFile().StoreSession(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	p := tgbridge.Peer{Kind: "channel", ID: 7, Title: "SENTINEL-CHANNEL"}
	src.tel.store().PutDialogs([]tgbridge.Dialog{{Peer: p, Unread: 2, Preview: "SENTINEL-PREVIEW"}})

	body, _ := json.Marshal(map[string]any{"password": pw,
		"sections": []string{"telegram", "telegramCache"}})
	rec := httptest.NewRecorder()
	src.handleBackupExport(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body)))
	blob := rec.Body.Bytes()

	// 1. The backup FILE must not reveal anything: it is password-encrypted.
	for _, s := range []string{"SENTINEL-SESSION-VALUE", "SENTINEL-CHANNEL", "SENTINEL-PREVIEW"} {
		if bytes.Contains(blob, []byte(s)) {
			t.Errorf("backup file leaks %q in the clear", s)
		}
	}

	// 2. Inside it, decrypted with the password, the session is plain -- which
	//    is what lets another device re-seal it with ITS key.
	p2, err := openBackup(blob, pw)
	if err != nil {
		t.Fatal(err)
	}
	if string(p2.Sections.Telegram) != string(secret) {
		t.Fatalf("inside the backup the session is %q, want the plain session", p2.Sections.Telegram)
	}

	// 3. Restore into a DIFFERENT device.
	b := t.TempDir()
	dst := &Server{dataDir: b}
	dst.tel = newTelHub(dst, b)

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("password", pw)
	_ = mw.WriteField("sections", `["telegram","telegramCache"]`)
	fw, _ := mw.CreateFormFile("file", "b.bin")
	fw.Write(blob)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/y", &form)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec2 := httptest.NewRecorder()
	dst.handleBackupRestore(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("restore %d: %s", rec2.Code, rec2.Body.String())
	}

	// 4. On disk at the destination it must be SEALED again.
	raw, err := os.ReadFile(dst.tel.sessionFile().Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("SENTINEL-SESSION-VALUE")) {
		t.Fatal("the restored session is sitting in the clear on disk")
	}

	// 5. THE POINT: the destination can actually READ it back.
	got, err := dst.tel.sessionFile().LoadSession(context.Background())
	if err != nil {
		t.Fatalf("the destination cannot decrypt what it restored: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("decrypted to %q, want %q", got, secret)
	}

	// 6. And it is sealed under the DESTINATION's key, not carried over.
	ka, _ := os.ReadFile(a + "/tel/keys/devicekey")
	kb, _ := os.ReadFile(b + "/tel/keys/devicekey")
	if bytes.Equal(ka, kb) {
		t.Fatal("both devices ended up with the same key; the key travelled")
	}

	// 7. The cache too.
	ds := loadTelCache(dst.tel.dir).Dialogs()
	if len(ds) != 1 || ds[0].Preview != "SENTINEL-PREVIEW" || ds[0].Unread != 2 {
		t.Fatalf("cache did not survive: %+v", ds)
	}
	cacheRaw, _ := os.ReadFile(telCachePath(dst.tel.dir))
	if strings.Contains(string(cacheRaw), "SENTINEL-PREVIEW") {
		t.Fatal("the restored cache is readable on disk")
	}
}
