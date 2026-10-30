package web

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const backupMagic = "TFBAK1"
const backupMaxBytes = 200 << 20

type backupPayload struct {
	Version   int            `json:"version"`
	CreatedAt int64          `json:"createdAt"`
	Sections  backupSections `json:"sections"`
}

type backupSections struct {
	Profiles   json.RawMessage   `json:"profiles,omitempty"`
	Chat       json.RawMessage   `json:"chat,omitempty"`
	Saved      json.RawMessage   `json:"saved,omitempty"`
	SavedMedia []backupMediaBlob `json:"savedMedia,omitempty"`
	BgImage    []byte            `json:"bgImage,omitempty"`
	// Telegram is the gotd session for the MTProto bridge. It holds the auth
	// key, so whoever has it IS the user on Telegram: it goes ONLY into an
	// encrypted backup, and is opt-in like every other section.
	Telegram []byte `json:"telegram,omitempty"`
	// TelegramCache is the cached conversations and messages, its own opt-in
	// item: it is bulky and rebuildable, so a user may well want the session
	// without it.
	//
	// Both travel DECRYPTED inside the backup, which is itself sealed with the
	// user's password. That is how Saved already does it, and it means the
	// device key never leaves the device and a restore works on any machine --
	// shipping the sealed bytes instead would hand the next device a file it
	// has no key for, which now reads as "start fresh", i.e. a silent logout.
	TelegramCache []byte `json:"telegramCache,omitempty"`
}

type backupMediaBlob struct {
	Size int64  `json:"size"`
	CRC  uint32 `json:"crc"`
	Mime string `json:"mime"`
	Data []byte `json:"data"`
}

func openBackup(data []byte, password string) (*backupPayload, error) {
	magicLen := len(backupMagic)
	if len(data) < magicLen+16+12+16 {
		return nil, errors.New("backup: file too short")
	}
	if string(data[:magicLen]) != backupMagic {
		return nil, errors.New("backup: invalid format")
	}
	salt := data[magicLen : magicLen+16]
	sealed := data[magicLen+16:]

	kek := deriveKEK(password, salt)
	plain, err := openBytes(kek, sealed)
	if err != nil {
		return nil, errBadPassphrase
	}
	var p backupPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, errors.New("backup: corrupt payload")
	}
	return &p, nil
}

func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Password string   `json:"password"`
		Sections []string `json:"sections"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		http.Error(w, "password required", http.StatusBadRequest)
		return
	}

	want := make(map[string]bool)
	for _, name := range req.Sections {
		want[name] = true
	}
	if len(want) == 0 {
		http.Error(w, "no sections selected", http.StatusBadRequest)
		return
	}

	if (want["saved"] || want["savedMedia"]) && s.savedCrypto != nil && s.savedCrypto.locked {
		http.Error(w, "saved store is locked", http.StatusLocked)
		return
	}

	var sec backupSections

	if want["profiles"] {
		if raw, err := os.ReadFile(filepath.Join(s.dataDir, "profiles.json")); err == nil {
			sec.Profiles = raw
		}
	}
	if want["chat"] {
		sec.Chat = s.collectChatBackup()
	}
	if want["saved"] {
		sec.Saved = s.collectSavedBackup()
	}
	if want["savedMedia"] {
		sec.SavedMedia = s.collectSavedMediaBackup()
	}
	if want["bgImage"] {
		if raw, err := os.ReadFile(filepath.Join(s.dataDir, "bg_image")); err == nil {
			sec.BgImage = raw
		}
	}
	if want["telegram"] {
		// Through the sealing layer, so what lands in the backup is the session
		// itself rather than ciphertext only this device can open.
		if plain, err := s.tel.sessionFile().LoadSession(r.Context()); err == nil {
			sec.Telegram = plain
		}
	}
	if want["telegramCache"] {
		if plain, err := s.tel.store().export(); err == nil {
			sec.TelegramCache = plain
		}
	}

	payload := backupPayload{
		Version:   1,
		CreatedAt: time.Now().Unix(),
		Sections:  sec,
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "marshal error", http.StatusInternalServerError)
		return
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		http.Error(w, "crypto error", http.StatusInternalServerError)
		return
	}
	kek := deriveKEK(req.Password, salt)
	sealed := sealBytes(kek, plain)

	out := make([]byte, 0, len(backupMagic)+len(salt)+len(sealed))
	out = append(out, backupMagic...)
	out = append(out, salt...)
	out = append(out, sealed...)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="thefeed-backup.tfbak"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.Write(out)
}

// chatBackupFiles are the only names the chat section ever contains. Restore
// checks against this same list, so the two cannot drift apart.
var chatBackupFiles = []string{"identity", "contacts", "threads", "servers", "settings"}

func (s *Server) collectChatBackup() json.RawMessage {
	chatDir := filepath.Join(s.dataDir, "chat")
	files := map[string]json.RawMessage{}
	for _, name := range chatBackupFiles {
		if raw, err := os.ReadFile(filepath.Join(chatDir, name+".json")); err == nil {
			files[name] = raw
		}
	}
	if len(files) == 0 {
		return nil
	}
	data, _ := json.Marshal(files)
	return data
}

func (s *Server) collectSavedBackup() json.RawMessage {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	data, _ := json.Marshal(st)
	return data
}

func (s *Server) collectSavedMediaBackup() []backupMediaBlob {
	if s.savedMedia == nil {
		return nil
	}
	s.savedMu.Lock()
	st := s.loadSaved()
	s.savedMu.Unlock()

	type mKey struct {
		size int64
		crc  uint32
	}
	seen := map[mKey]bool{}
	var keys []mKey
	for _, it := range st.Items {
		for _, m := range it.Media {
			k := mKey{m.Size, m.CRC}
			if m.Persisted && !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}

	var out []backupMediaBlob
	for _, k := range keys {
		body, mime, ok := s.savedMedia.Get(k.size, k.crc)
		if !ok {
			continue
		}
		out = append(out, backupMediaBlob{Size: k.size, CRC: k.crc, Mime: mime, Data: body})
	}
	return out
}

func (s *Server) handleBackupPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, backupMaxBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	pw := r.FormValue("password")
	if pw == "" {
		http.Error(w, "password required", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file required", http.StatusBadRequest)
		return
	}
	defer f.Close()

	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	p, err := openBackup(raw, pw)
	if err != nil {
		if errors.Is(err, errBadPassphrase) {
			http.Error(w, "wrong password", http.StatusUnauthorized)
			return
		}
		http.Error(w, "invalid backup file", http.StatusBadRequest)
		return
	}

	writeJSON(w, buildBackupPreview(p))
}

func buildBackupPreview(p *backupPayload) map[string]any {
	out := map[string]any{
		"version":   p.Version,
		"createdAt": p.CreatedAt,
	}

	if p.Sections.Profiles != nil {
		var pl ProfileList
		if json.Unmarshal(p.Sections.Profiles, &pl) == nil {
			names := make([]string, 0, len(pl.Profiles))
			for _, pr := range pl.Profiles {
				names = append(names, pr.Nickname)
			}
			out["profiles"] = map[string]any{"count": len(pl.Profiles), "names": names}
		}
	}

	if p.Sections.Chat != nil {
		var files map[string]json.RawMessage
		info := map[string]any{}
		if json.Unmarshal(p.Sections.Chat, &files) == nil {
			if _, ok := files["identity"]; ok {
				info["identity"] = true
			}
			if raw, ok := files["contacts"]; ok {
				var c map[string]string
				if json.Unmarshal(raw, &c) == nil {
					info["contacts"] = len(c)
				}
			}
			if raw, ok := files["threads"]; ok {
				var tf chatThreadsFile
				if json.Unmarshal(raw, &tf) == nil && tf.Threads != nil {
					info["threads"] = len(tf.Threads)
					msgs := 0
					for _, t := range tf.Threads {
						msgs += len(t.Msgs)
					}
					info["messages"] = msgs
				}
			}
		}
		out["chat"] = info
	}

	if p.Sections.Saved != nil {
		var st savedStore
		if json.Unmarshal(p.Sections.Saved, &st) == nil {
			out["saved"] = map[string]any{"count": len(st.Items)}
		}
	}

	if len(p.Sections.SavedMedia) > 0 {
		var total int64
		for _, e := range p.Sections.SavedMedia {
			total += int64(len(e.Data))
		}
		out["savedMedia"] = map[string]any{"count": len(p.Sections.SavedMedia), "bytes": total}
	}

	if len(p.Sections.BgImage) > 0 {
		out["bgImage"] = map[string]any{"bytes": len(p.Sections.BgImage)}
	}
	if len(p.Sections.Telegram) > 0 {
		// Byte count only: never echo any of the auth key back.
		out["telegram"] = map[string]any{"bytes": len(p.Sections.Telegram)}
	}
	if len(p.Sections.TelegramCache) > 0 {
		out["telegramCache"] = map[string]any{"bytes": len(p.Sections.TelegramCache)}
	}

	return out
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, backupMaxBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		// Only say "too large" when it was. A truncated upload, the wrong
		// content type or a body that is not a form at all are not size
		// problems, and reporting them as one misdirects the debugging.
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unreadable upload", http.StatusBadRequest)
		return
	}
	pw := r.FormValue("password")
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file required", http.StatusBadRequest)
		return
	}
	defer f.Close()

	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	p, err := openBackup(raw, pw)
	if err != nil {
		if errors.Is(err, errBadPassphrase) {
			http.Error(w, "wrong password", http.StatusUnauthorized)
			return
		}
		http.Error(w, "invalid backup file", http.StatusBadRequest)
		return
	}

	var sections []string
	if sj := r.FormValue("sections"); sj != "" {
		if err := json.Unmarshal([]byte(sj), &sections); err != nil {
			http.Error(w, "bad sections", http.StatusBadRequest)
			return
		}
	}
	want := map[string]bool{}
	for _, name := range sections {
		want[name] = true
	}
	if len(want) == 0 {
		http.Error(w, "no sections selected", http.StatusBadRequest)
		return
	}

	needUnlocked := (want["saved"] && p.Sections.Saved != nil) ||
		(want["savedMedia"] && len(p.Sections.SavedMedia) > 0)
	if needUnlocked && s.savedCrypto != nil && s.savedCrypto.locked {
		http.Error(w, "saved store is locked", http.StatusLocked)
		return
	}

	if want["profiles"] && p.Sections.Profiles != nil {
		if err := s.restoreProfiles(p.Sections.Profiles); err != nil {
			http.Error(w, "profiles: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if want["chat"] && p.Sections.Chat != nil {
		if err := s.restoreChat(p.Sections.Chat); err != nil {
			http.Error(w, "chat: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if want["saved"] && p.Sections.Saved != nil {
		if err := s.restoreSaved(p.Sections.Saved); err != nil {
			http.Error(w, "saved: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if want["savedMedia"] && len(p.Sections.SavedMedia) > 0 {
		s.restoreSavedMedia(p.Sections.SavedMedia)
	}
	if want["bgImage"] && len(p.Sections.BgImage) > 0 {
		_ = os.WriteFile(filepath.Join(s.dataDir, "bg_image"), p.Sections.BgImage, 0o600)
	}
	if want["telegram"] && len(p.Sections.Telegram) > 0 {
		// Restoring a session makes this device that Telegram account, so it is
		// written back THROUGH the sealing layer: sealed under this device's
		// own key, never left as plaintext on disk.
		if err := s.tel.sessionFile().StoreSession(r.Context(), p.Sections.Telegram); err != nil {
			log.Printf("backup: restoring the Telegram session failed: %v", err)
		}
	}
	if want["telegramCache"] && len(p.Sections.TelegramCache) > 0 {
		s.tel.store().importFrom(p.Sections.TelegramCache)
	}

	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) restoreProfiles(data json.RawMessage) error {
	var pl ProfileList
	if err := json.Unmarshal(data, &pl); err != nil {
		return err
	}
	s.profilesMu.Lock()
	defer s.profilesMu.Unlock()
	if err := s.saveProfiles(&pl); err != nil {
		return err
	}
	for _, p := range pl.Profiles {
		if p.ID == pl.Active {
			cfg := p.Config
			s.mu.Lock()
			s.config = &cfg
			s.mu.Unlock()
			_ = s.saveConfig(&cfg)
			if s.scanner != nil {
				_ = s.initFetcher()
				s.applySelectedList()
			}
			break
		}
	}
	return nil
}

func (s *Server) restoreChat(data json.RawMessage) error {
	var files map[string]json.RawMessage
	if err := json.Unmarshal(data, &files); err != nil {
		return err
	}
	chatDir := filepath.Join(s.dataDir, "chat")
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		return err
	}
	s.chat.mu.Lock()
	defer s.chat.mu.Unlock()
	// The keys come out of the uploaded file, so they are the attacker's
	// text. Joined straight onto a path, "../../../.claude/settings" walks
	// out of the data directory and writes anywhere a .json is read from --
	// hooks that run on the next start, an editor's settings, the app's own
	// profiles.json. The victim only has to restore a backup someone gave
	// them, which is exactly what a backup file is for.
	//
	// An allow-list, not a sanitiser: these five names are the whole format.
	allowed := make(map[string]bool, len(chatBackupFiles))
	for _, n := range chatBackupFiles {
		allowed[n] = true
	}
	for name, raw := range files {
		if !allowed[name] {
			s.addLog("[backup] restore: ignoring unexpected chat entry " + strconv.Quote(name))
			continue
		}
		_ = writeFileAtomic(filepath.Join(chatDir, name+".json"), raw, 0o600)
	}
	s.chat.loadState()
	return nil
}

func (s *Server) restoreSaved(data json.RawMessage) error {
	var st savedStore
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	if st.Items == nil {
		st.Items = []SavedItem{}
	}
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	return s.writeSaved(&st)
}

func (s *Server) restoreSavedMedia(entries []backupMediaBlob) {
	if s.savedMedia == nil {
		return
	}
	for _, e := range entries {
		if _, _, ok := s.savedMedia.Get(e.Size, e.CRC); ok {
			continue
		}
		_ = s.savedMedia.Put(e.Size, e.CRC, e.Data, e.Mime)
	}
}
