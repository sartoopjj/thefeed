package web

import (
	"bytes"
	"encoding/json"
	"hash/crc32"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func backupMultipart(t *testing.T, data []byte, password string, sections []string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("password", password)
	if sections != nil {
		sj, _ := json.Marshal(sections)
		_ = mw.WriteField("sections", string(sj))
	}
	fw, _ := mw.CreateFormFile("file", "backup.tfbak")
	_, _ = fw.Write(data)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestBackupRoundTrip(t *testing.T) {
	dir1 := t.TempDir()
	sc1, _ := loadSavedCrypto(dir1)
	sm1, _ := newMediaDiskCache(dir1+"/saved-media", 0)
	sm1.crypto = sc1
	s1 := &Server{dataDir: dir1, savedCrypto: sc1, savedMedia: sm1}
	s1.chat = newChatHub(s1, dir1)

	// Create a profile.
	pl := &ProfileList{
		Active: "p1",
		Profiles: []Profile{{
			ID:       "p1",
			Nickname: "Test Profile",
			Config:   Config{Domain: "test.example.com", Key: "abc123"},
		}},
	}
	if err := s1.saveProfiles(pl); err != nil {
		t.Fatal(err)
	}

	// Create a saved note.
	noteReq := httptest.NewRequest("POST", "/api/saved/note", strings.NewReader(`{"text":"backup me"}`))
	nw := httptest.NewRecorder()
	s1.handleSavedNote(nw, noteReq)
	if nw.Code != 200 {
		t.Fatalf("note status = %d", nw.Code)
	}

	// Create chat contacts.
	chatDir := filepath.Join(dir1, "chat")
	_ = os.MkdirAll(chatDir, 0o700)
	_ = writeFileAtomic(filepath.Join(chatDir, "contacts.json"), []byte(`{"alice":"Alice","bob":"Bob"}`), 0o600)
	s1.chat.mu.Lock()
	s1.chat.loadState()
	s1.chat.mu.Unlock()

	// Upload media.
	imgData := []byte("PNG-pretend-image")
	imgSize := int64(len(imgData))
	imgCRC := crc32.ChecksumIEEE(imgData)
	if err := sm1.Put(imgSize, imgCRC, imgData, "image/png"); err != nil {
		t.Fatal(err)
	}
	// Mark media as persisted in saved item.
	var noteOut savedItemOut
	_ = json.Unmarshal(nw.Body.Bytes(), &noteOut)
	s1.savedMu.Lock()
	st := s1.loadSaved()
	if len(st.Items) > 0 {
		st.Items[0].Media = []SavedMedia{{Tag: "[IMAGE]", Size: imgSize, CRC: imgCRC, Persisted: true}}
		_ = s1.writeSaved(st)
	}
	s1.savedMu.Unlock()

	// Write a bg_image.
	_ = os.WriteFile(filepath.Join(dir1, "bg_image"), []byte("FAKE_BG"), 0o600)

	// --- Export ---
	exportBody := `{"password":"test123","sections":["profiles","chat","saved","savedMedia","bgImage"]}`
	req := httptest.NewRequest("POST", "/api/backup/export", strings.NewReader(exportBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s1.handleBackupExport(w, req)
	if w.Code != 200 {
		t.Fatalf("export status = %d (%s)", w.Code, w.Body.String())
	}
	backupData := w.Body.Bytes()
	if string(backupData[:6]) != backupMagic {
		t.Fatal("bad magic")
	}

	// --- Preview ---
	pbody, pct := backupMultipart(t, backupData, "test123", nil)
	previewReq := httptest.NewRequest("POST", "/api/backup/preview", pbody)
	previewReq.Header.Set("Content-Type", pct)
	pw := httptest.NewRecorder()
	s1.handleBackupPreview(pw, previewReq)
	if pw.Code != 200 {
		t.Fatalf("preview status = %d (%s)", pw.Code, pw.Body.String())
	}
	var preview map[string]any
	_ = json.Unmarshal(pw.Body.Bytes(), &preview)
	if preview["profiles"] == nil {
		t.Fatal("preview missing profiles")
	}
	if preview["chat"] == nil {
		t.Fatal("preview missing chat")
	}
	if preview["saved"] == nil {
		t.Fatal("preview missing saved")
	}
	if preview["savedMedia"] == nil {
		t.Fatal("preview missing savedMedia")
	}
	if preview["bgImage"] == nil {
		t.Fatal("preview missing bgImage")
	}

	// --- Restore into fresh server ---
	dir2 := t.TempDir()
	sc2, _ := loadSavedCrypto(dir2)
	sm2, _ := newMediaDiskCache(dir2+"/saved-media", 0)
	sm2.crypto = sc2
	s2 := &Server{dataDir: dir2, savedCrypto: sc2, savedMedia: sm2}
	s2.chat = newChatHub(s2, dir2)

	sections := []string{"profiles", "chat", "saved", "savedMedia", "bgImage"}
	rbody, rct := backupMultipart(t, backupData, "test123", sections)
	restoreReq := httptest.NewRequest("POST", "/api/backup/restore", rbody)
	restoreReq.Header.Set("Content-Type", rct)
	rw := httptest.NewRecorder()
	s2.handleBackupRestore(rw, restoreReq)
	if rw.Code != 200 {
		t.Fatalf("restore status = %d (%s)", rw.Code, rw.Body.String())
	}

	// Verify profiles.
	pl2, err := s2.loadProfiles()
	if err != nil {
		t.Fatalf("load profiles: %v", err)
	}
	if len(pl2.Profiles) != 1 || pl2.Profiles[0].Nickname != "Test Profile" {
		t.Fatalf("profiles not restored: %+v", pl2)
	}
	if pl2.Active != "p1" {
		t.Fatalf("active profile = %q, want p1", pl2.Active)
	}

	// Verify saved.
	if s2.savedCount() != 1 {
		t.Fatalf("saved count = %d, want 1", s2.savedCount())
	}
	items := s2.savedList()
	if items[0].Text != "backup me" {
		t.Fatalf("saved text = %q", items[0].Text)
	}

	// Verify chat contacts.
	s2.chat.mu.Lock()
	if len(s2.chat.contacts) != 2 || s2.chat.contacts["alice"] != "Alice" {
		t.Fatalf("chat contacts = %v", s2.chat.contacts)
	}
	s2.chat.mu.Unlock()

	// Verify saved media.
	body, mime, ok := sm2.Get(imgSize, imgCRC)
	if !ok || !bytes.Equal(body, imgData) || mime != "image/png" {
		t.Fatalf("media not restored: ok=%v mime=%q", ok, mime)
	}

	// Verify bg_image.
	bg, err := os.ReadFile(filepath.Join(dir2, "bg_image"))
	if err != nil || string(bg) != "FAKE_BG" {
		t.Fatal("bg_image not restored")
	}
}

func TestBackupWrongPassword(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}

	req := httptest.NewRequest("POST", "/api/backup/export",
		strings.NewReader(`{"password":"right","sections":["profiles"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleBackupExport(w, req)
	if w.Code != 200 {
		t.Fatalf("export status = %d", w.Code)
	}

	body, ct := backupMultipart(t, w.Body.Bytes(), "wrong", nil)
	previewReq := httptest.NewRequest("POST", "/api/backup/preview", body)
	previewReq.Header.Set("Content-Type", ct)
	pw := httptest.NewRecorder()
	s.handleBackupPreview(pw, previewReq)
	if pw.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password preview = %d, want 401", pw.Code)
	}
}

func TestBackupLockedSavedRejected(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}

	// Set passphrase to lock after reload.
	noteReq := httptest.NewRequest("POST", "/api/saved/note", strings.NewReader(`{"text":"secret"}`))
	s.handleSavedNote(httptest.NewRecorder(), noteReq)
	if err := sc.setPassphrase("pw"); err != nil {
		t.Fatal(err)
	}

	// Reload → locked.
	s.savedCrypto, _ = loadSavedCrypto(dir)
	if !s.savedCrypto.locked {
		t.Fatal("should be locked")
	}

	// Export with saved should fail.
	req := httptest.NewRequest("POST", "/api/backup/export",
		strings.NewReader(`{"password":"backup","sections":["saved"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleBackupExport(w, req)
	if w.Code != http.StatusLocked {
		t.Fatalf("locked export = %d, want 423", w.Code)
	}

	// Export without saved should succeed.
	req2 := httptest.NewRequest("POST", "/api/backup/export",
		strings.NewReader(`{"password":"backup","sections":["profiles"]}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.handleBackupExport(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("profiles-only export = %d", w2.Code)
	}
}

func TestBackupSelectiveSections(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}
	s.chat = newChatHub(s, dir)

	// Create data in all sections.
	_ = s.saveProfiles(&ProfileList{
		Active:   "p1",
		Profiles: []Profile{{ID: "p1", Nickname: "Prof"}},
	})
	noteReq := httptest.NewRequest("POST", "/api/saved/note", strings.NewReader(`{"text":"note1"}`))
	s.handleSavedNote(httptest.NewRecorder(), noteReq)

	// Export only profiles.
	req := httptest.NewRequest("POST", "/api/backup/export",
		strings.NewReader(`{"password":"pw","sections":["profiles"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleBackupExport(w, req)
	if w.Code != 200 {
		t.Fatalf("export status = %d", w.Code)
	}

	// Preview should only show profiles.
	pbody, pct := backupMultipart(t, w.Body.Bytes(), "pw", nil)
	pr := httptest.NewRequest("POST", "/api/backup/preview", pbody)
	pr.Header.Set("Content-Type", pct)
	pw := httptest.NewRecorder()
	s.handleBackupPreview(pw, pr)
	if pw.Code != 200 {
		t.Fatalf("preview status = %d", pw.Code)
	}
	var prev map[string]any
	_ = json.Unmarshal(pw.Body.Bytes(), &prev)
	if prev["profiles"] == nil {
		t.Fatal("should have profiles")
	}
	if prev["saved"] != nil {
		t.Fatal("should NOT have saved")
	}
}

// A malformed upload must say what is actually wrong.
//
// Every way of failing to parse the form -- a truncated upload, the wrong
// content type, a body that is not a form at all -- used to be reported as
// "file too large", which sends whoever is debugging it looking for a size
// limit that is not the reason. A 1 KB backup posted with the wrong content
// type reported a size problem.
func TestBackupRestoreReportsTheRealReason(t *testing.T) {
	s := &Server{dataDir: t.TempDir(), clients: make(map[chan string]struct{})}

	// Not a form at all, and tiny.
	r := httptest.NewRequest(http.MethodPost, "/api/backup/restore",
		strings.NewReader("TFBAK1 not-a-form"))
	r.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	s.handleBackupRestore(rec, r)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a %d-byte body was reported as too large", len("TFBAK1 not-a-form"))
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for an unreadable upload", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unreadable") {
		t.Errorf("body = %q, want it to say the upload could not be read", rec.Body.String())
	}
}

// A backup file is something you are GIVEN, so every key in it is the
// sender's text. Joined straight onto a path, "../../../.claude/settings"
// walks out of the data directory and writes a .json anywhere -- hooks that
// run on the next start, an editor's settings, the app's own profiles.json.
// Restoring a backup someone sent you is the entire point of the format.
func TestRestoreChatRefusesToWriteOutsideItsDirectory(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	s.chat = newChatHub(s, dir)

	escapes := map[string]json.RawMessage{
		"../../../.claude/settings": json.RawMessage(`{"hooks":"pwned"}`),
		"../../profiles":            json.RawMessage(`{"pwned":true}`),
		"../escaped":                json.RawMessage(`{"pwned":true}`),
		"sub/nested":                json.RawMessage(`{"pwned":true}`),
		// ...and one legitimate entry, which must still land.
		"identity": json.RawMessage(`{"ok":true}`),
	}
	data, err := json.Marshal(escapes)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.restoreChat(data); err != nil {
		t.Fatalf("restoreChat: %v", err)
	}

	chatDir := filepath.Join(dir, "chat")
	// The one allowed name is written.
	if _, err := os.Stat(filepath.Join(chatDir, "identity.json")); err != nil {
		t.Errorf("the legitimate entry was not restored: %v", err)
	}
	// Nothing at all outside the chat directory.
	for _, p := range []string{
		filepath.Join(dir, "profiles.json"),
		filepath.Join(dir, "escaped.json"),
		filepath.Join(filepath.Dir(dir), "escaped.json"),
		filepath.Join(chatDir, "sub", "nested.json"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("a backup entry wrote outside its directory: %s", p)
		}
	}
	// And the whole tree holds nothing but the five known names.
	ents, err := os.ReadDir(chatDir)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, n := range chatBackupFiles {
		allowed[n+".json"] = true
	}
	for _, e := range ents {
		if !allowed[e.Name()] {
			t.Errorf("unexpected file in the chat directory: %s", e.Name())
		}
	}
}

// The restore list and the export list must stay the same list.
func TestChatBackupNamesMatchWhatRestoreAccepts(t *testing.T) {
	if len(chatBackupFiles) == 0 {
		t.Fatal("no chat backup names declared")
	}
	for _, n := range chatBackupFiles {
		if strings.ContainsAny(n, `/\.`) {
			t.Errorf("name %q is not a plain file name", n)
		}
	}
}

// Renaming the backup file must change nothing.
//
// The allow-list added for the path-traversal fix is on the KEYS INSIDE the
// archive, never on the file's own name -- so "my-backup.json", a second copy
// the system saved as "thefeed-backup (1).tfbak", or any name at all restores
// exactly the same. Worth pinning: a filename or extension check bolted on
// later would silently break every user who renamed or re-downloaded theirs.
func TestRestoreIgnoresTheFileName(t *testing.T) {
	// One real backup, made the ordinary way.
	src := t.TempDir()
	sc, _ := loadSavedCrypto(src)
	s1 := &Server{dataDir: src, savedCrypto: sc}
	s1.chat = newChatHub(s1, src)
	if err := s1.saveProfiles(&ProfileList{
		Active:   "p1",
		Profiles: []Profile{{ID: "p1", Nickname: "Renamed File Test", Config: Config{Domain: "d.example.com", Key: "k"}}},
	}); err != nil {
		t.Fatal(err)
	}
	chatDir := filepath.Join(src, "chat")
	_ = os.MkdirAll(chatDir, 0o700)
	_ = writeFileAtomic(filepath.Join(chatDir, "contacts.json"), []byte(`{"a":"Alice"}`), 0o600)
	s1.chat.mu.Lock()
	s1.chat.loadState()
	s1.chat.mu.Unlock()

	exp := httptest.NewRequest("POST", "/api/backup/export",
		strings.NewReader(`{"password":"pw","sections":["profiles","chat"]}`))
	ew := httptest.NewRecorder()
	s1.handleBackupExport(ew, exp)
	if ew.Code != 200 {
		t.Fatalf("export status = %d: %s", ew.Code, ew.Body)
	}
	archive := ew.Body.Bytes()

	// The same bytes under every name a user or an operating system might
	// give them -- including no name at all.
	for _, name := range []string{
		"thefeed-backup.tfbak",
		"thefeed-backup (1).tfbak",  // the OS's second copy
		"my phone before reinstall", // no extension, spaces
		"backup.json",               // renamed to something else entirely
		"نسخه-پشتیبان.tfbak",        // non-ASCII
	} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("password", "pw")
		sj, _ := json.Marshal([]string{"profiles", "chat"})
		_ = mw.WriteField("sections", string(sj))
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		fw.Write(archive)
		mw.Close()

		dst := t.TempDir()
		sc2, _ := loadSavedCrypto(dst)
		s2 := &Server{dataDir: dst, savedCrypto: sc2}
		s2.chat = newChatHub(s2, dst)

		req := httptest.NewRequest("POST", "/api/backup/restore", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		s2.handleBackupRestore(rec, req)
		if rec.Code != 200 {
			t.Errorf("restoring a file named %q failed: %d %s", name, rec.Code, rec.Body)
			continue
		}

		// And the contents really landed, not just a 200.
		pl, err := s2.loadProfiles()
		if err != nil || len(pl.Profiles) != 1 || pl.Profiles[0].Nickname != "Renamed File Test" {
			t.Errorf("file named %q restored no profile: %+v (%v)", name, pl, err)
		}
		raw, err := os.ReadFile(filepath.Join(dst, "chat", "contacts.json"))
		if err != nil || !strings.Contains(string(raw), "Alice") {
			t.Errorf("file named %q restored no contacts: %v", name, err)
		}
	}
}
