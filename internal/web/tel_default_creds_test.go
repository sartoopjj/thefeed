package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gotd/td/tgerr"

	"github.com/sartoopjj/thefeed/internal/telcreds"
)

const (
	bundledID   = 7654321
	bundledHash = "bundledbundledbundledbundled0000"
)

// withBundled stands in for a release build that carries a default pair.
func withBundled(t *testing.T) {
	t.Helper()
	blob, key, err := telcreds.Seal(bundledID, bundledHash)
	if err != nil {
		t.Fatal(err)
	}
	prev := telDefaultBlob
	telDefaultBlob = blob
	t.Cleanup(func() { telDefaultBlob = prev })
	t.Cleanup(telcreds.SetBuildKeyForTest(key))
}

// newTelCredsServer is a test server with a profile list, as every real
// install has by the time the Telegram tab can be opened.
func newTelCredsServer(t *testing.T) *Server {
	t.Helper()
	s := newTelTestServer(t)
	if err := s.saveProfiles(&ProfileList{}); err != nil {
		t.Fatal(err)
	}
	return s
}

func telInfoRaw(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleTelInfo(rec, httptest.NewRequest(http.MethodGet, "/api/tel/info", nil))
	if rec.Code != 200 {
		t.Fatalf("info: %d", rec.Code)
	}
	return rec.Body.String()
}

func telSaveCreds(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleTelCredentials(rec, httptest.NewRequest(http.MethodPost, "/api/tel/credentials", strings.NewReader(body)))
	return rec.Code, rec.Body.String()
}

// The bundled pair connects, but never leaves the process: not in the page's
// info, not on disk.
func TestTelBundledCredentialsAreUsedButNeverShown(t *testing.T) {
	withBundled(t)
	s := newTelCredsServer(t)

	id, hash, isDefault := s.tel.apiCredentials()
	if id != bundledID || hash != bundledHash || !isDefault {
		t.Fatalf("bridge gets %d %q default=%v", id, hash, isDefault)
	}
	raw := telInfoRaw(t, s)
	for _, leak := range []string{bundledHash, strconv.Itoa(bundledID), bundledHash[:10]} {
		if strings.Contains(raw, leak) {
			t.Fatalf("info reveals %q: %s", leak, raw)
		}
	}
	for _, want := range []string{`"hasCredentials":true`, `"defaultCredentials":true`, `"defaultAvailable":true`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("info lacks %s: %s", want, raw)
		}
	}
	if strings.Contains(raw, `"apiHash"`) || strings.Contains(raw, `"apiId"`) {
		t.Fatalf("info carries credential fields while the default is in use: %s", raw)
	}
}

// The user's own pair wins over the default, is echoed back so a typo can be
// fixed, and leaving it for the default signs out and forgets it.
func TestTelOwnCredentialsOverrideAndCanBeDropped(t *testing.T) {
	withBundled(t)
	s := newTelCredsServer(t)

	code, body := telSaveCreds(t, s, `{"apiId":111,"apiHash":"ownhash"}`)
	if code != 200 || !strings.Contains(body, `"signedOut":true`) {
		t.Fatalf("saving own pair: %d %s (leaving the shared key must sign out)", code, body)
	}
	if id, hash, isDefault := s.tel.apiCredentials(); id != 111 || hash != "ownhash" || isDefault {
		t.Fatalf("own pair not used: %d %q default=%v", id, hash, isDefault)
	}
	raw := telInfoRaw(t, s)
	if !strings.Contains(raw, `"apiHash":"ownhash"`) || !strings.Contains(raw, `"defaultCredentials":false`) {
		t.Fatalf("own pair not echoed: %s", raw)
	}

	code, body = telSaveCreds(t, s, `{"apiId":111,"apiHash":"ownhash"}`)
	if code != 200 || !strings.Contains(body, `"signedOut":false`) {
		t.Fatalf("an unchanged save must not sign out: %d %s", code, body)
	}

	code, body = telSaveCreds(t, s, `{"useDefault":true}`)
	if code != 200 || !strings.Contains(body, `"signedOut":true`) {
		t.Fatalf("back to default: %d %s", code, body)
	}
	if _, _, isDefault := s.tel.apiCredentials(); !isDefault {
		t.Fatal("still on the own pair after choosing the default")
	}
	disk, err := os.ReadFile(filepath.Join(s.dataDir, "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"ownhash", bundledHash} {
		if strings.Contains(string(disk), leak) {
			t.Fatalf("profiles.json still holds %q", leak)
		}
	}

	code, body = telSaveCreds(t, s, `{"useDefault":true}`)
	if code != 200 || !strings.Contains(body, `"signedOut":false`) {
		t.Fatalf("choosing the default again must not sign out: %d %s", code, body)
	}
}

// A build without the secret: no default, the UI asks for a pair, and asking
// for "the default" is refused rather than silently clearing the user's own.
func TestTelWithoutBundledCredentials(t *testing.T) {
	prev := telDefaultBlob
	telDefaultBlob = ""
	t.Cleanup(func() { telDefaultBlob = prev })
	s := newTelCredsServer(t)

	raw := telInfoRaw(t, s)
	for _, want := range []string{`"hasCredentials":false`, `"defaultCredentials":false`, `"defaultAvailable":false`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("info lacks %s: %s", want, raw)
		}
	}
	if code, _ := telSaveCreds(t, s, `{"apiId":222,"apiHash":"mine"}`); code != 200 {
		t.Fatal("saving own pair failed")
	}
	if code, _ := telSaveCreds(t, s, `{"useDefault":true}`); code != http.StatusBadRequest {
		t.Fatalf("useDefault with no default: %d, want 400", code)
	}
	if id, hash, _ := s.tel.apiCredentials(); id != 222 || hash != "mine" {
		t.Fatalf("own pair lost by a refused request: %d %q", id, hash)
	}
}

// Entering a pair on a fresh install, before any profile list exists, must
// work rather than fail on the missing file.
func TestTelCredentialsOnAFreshInstall(t *testing.T) {
	s := newTelTestServer(t)
	if code, body := telSaveCreds(t, s, `{"apiId":333,"apiHash":"fresh"}`); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("saving on a fresh install: %d %s", code, body)
	}
	if id, hash, _ := s.tel.apiCredentials(); id != 333 || hash != "fresh" {
		t.Fatalf("not stored: %d %q", id, hash)
	}
}

// A pair Telegram refuses can only be found out at sign-in. When it is the
// built-in one the user typed nothing, so the message must say so instead of
// asking them to edit fields that are empty.
func TestTelRejectedKeyIsNamedForWhatItIs(t *testing.T) {
	refused := tgerr.New(400, "API_ID_INVALID")
	flooded := tgerr.New(400, "API_ID_PUBLISHED_FLOOD")

	withBundled(t)
	s := newTelCredsServer(t)
	for _, err := range []error{refused, flooded} {
		if got := s.tel.statusKey(err); got != "tel_err_default_creds_rejected" {
			t.Errorf("built-in key, %v: %q", err, got)
		}
	}
	s.tel.setErr(refused)
	if raw := telInfoRaw(t, s); !strings.Contains(raw, "tel_err_default_creds_rejected") {
		t.Errorf("info does not carry the reason: %s", raw)
	}

	if code, _ := telSaveCreds(t, s, `{"apiId":555,"apiHash":"own"}`); code != 200 {
		t.Fatal("saving own pair failed")
	}
	if got := s.tel.statusKey(refused); got != "tel_err_bad_credentials" {
		t.Errorf("own key: %q, want the edit-your-key message", got)
	}
	if got := s.tel.statusKey(tgerr.New(400, "PHONE_CODE_INVALID")); got != "tel_err_code_invalid" {
		t.Errorf("other errors must keep their own message: %q", got)
	}
}
