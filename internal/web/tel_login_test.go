package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tgerr"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// fakeAuther stands in for the bridge in the login steps.
type fakeAuther struct {
	sent      tgbridge.CodeSent
	pattern   string
	err       error
	byEmail   *bool
	setupArgs []string
	verifyArg []string
}

func (f *fakeAuther) SendCode(context.Context, string) (tgbridge.CodeSent, error) {
	return f.sent, f.err
}
func (f *fakeAuther) ResendCode(context.Context, string, string) (tgbridge.CodeSent, error) {
	return f.sent, f.err
}
func (f *fakeAuther) SignIn(_ context.Context, _, _, _ string, byEmail bool) error {
	f.byEmail = &byEmail
	return f.err
}
func (f *fakeAuther) SetupLoginEmail(_ context.Context, phone, hash, email string) (string, error) {
	f.setupArgs = []string{phone, hash, email}
	return f.pattern, f.err
}
func (f *fakeAuther) VerifyLoginEmail(_ context.Context, phone, hash, code string) (tgbridge.CodeSent, error) {
	f.verifyArg = []string{phone, hash, code}
	return f.sent, f.err
}
func (f *fakeAuther) PasswordHint(context.Context) string { return "pet" }

// withFakeAuth makes the login steps run against f, as if connected.
func withFakeAuth(t *testing.T, f *fakeAuther) {
	t.Helper()
	linked, call := telAuthLinked, telAuthCall
	t.Cleanup(func() { telAuthLinked, telAuthCall = linked, call })
	telAuthLinked = func(*telHub) bool { return true }
	telAuthCall = func(_ context.Context, _ *telHub, _ bool, fn func(telAuther) error) error { return fn(f) }
}

func telAuthPost(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleTelAuth(rec, telJSONReq("/api/tel/auth", body))
	var r map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%s: %v (%q)", body, err, rec.Body.String())
	}
	return r
}

func (s *Server) logText() string {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	return strings.Join(s.logLines, "\n")
}

// Where the code went reaches the UI, and the log, without the number.
func TestTelCodeReportsDelivery(t *testing.T) {
	s := newTelTestServer(t)
	f := &fakeAuther{sent: tgbridge.CodeSent{Hash: "h1", Via: "email", EmailPattern: "a***@b***.com"}}
	withFakeAuth(t, f)
	r := telAuthPost(t, s, `{"action":"code","phone":"+15550001"}`)
	if r["ok"] != true || r["codeHash"] != "h1" || r["via"] != "email" || r["emailPattern"] != "a***@b***.com" {
		t.Fatalf("code: %v", r)
	}
	log := s.logText()
	if !strings.Contains(log, "code via email") || strings.Contains(log, "5550001") || strings.Contains(log, "b***") {
		t.Fatalf("log: %q", log)
	}
	// No code at all: Telegram wants a login email first.
	f.sent = tgbridge.CodeSent{Hash: "h1", Via: "setupemail"}
	if r := telAuthPost(t, s, `{"action":"resend","phone":"+1","codeHash":"h1"}`); r["via"] != "setupemail" {
		t.Fatalf("resend: %v", r)
	}
	if !strings.Contains(s.logText(), "resend: code via setupemail") {
		t.Fatalf("log: %q", s.logText())
	}
}

// Sign-in passes the code in the field matching where it came from.
func TestTelSignInByEmailFlag(t *testing.T) {
	s := newTelTestServer(t)
	f := &fakeAuther{}
	withFakeAuth(t, f)
	for via, want := range map[string]bool{"email": true, "app": false, "sms": false, "": false} {
		f.byEmail = nil
		r := telAuthPost(t, s, `{"action":"signin","phone":"+1","code":"12345","codeHash":"h","via":"`+via+`"}`)
		if r["ok"] != true || f.byEmail == nil || *f.byEmail != want {
			t.Fatalf("via %q: %v, byEmail %v", via, r, f.byEmail)
		}
	}
	// Two-step verification is the next step, not a failure, for an email
	// code too.
	f.err = auth.ErrPasswordAuthNeeded
	if r := telAuthPost(t, s, `{"action":"signin","phone":"+1","code":"1","codeHash":"h","via":"email"}`); r["needPassword"] != true || r["hint"] != "pet" {
		t.Fatalf("2FA: %v", r)
	}
	f.err = tgbridge.ErrSignUpRequired
	if r := telAuthPost(t, s, `{"action":"signin","phone":"+1","code":"1","codeHash":"h","via":"email"}`); r["reason"] != "tel_err_no_account" {
		t.Fatalf("no account: %v", r)
	}
	f.err = &auth.SignUpRequired{}
	if r := telAuthPost(t, s, `{"action":"signin","phone":"+1","code":"1","codeHash":"h"}`); r["reason"] != "tel_err_no_account" {
		t.Fatalf("no account, phone path: %v", r)
	}
}

// The add-an-email step: a code to that email, then the login code.
func TestTelEmailSetupSteps(t *testing.T) {
	s := newTelTestServer(t)
	f := &fakeAuther{pattern: "u***@m***.org"}
	withFakeAuth(t, f)
	r := telAuthPost(t, s, `{"action":"emailsetup","phone":" +100 ","codeHash":"h1","email":" u@m.org "}`)
	if r["ok"] != true || r["emailPattern"] != "u***@m***.org" {
		t.Fatalf("emailsetup: %v", r)
	}
	if strings.Join(f.setupArgs, "|") != "+100|h1|u@m.org" {
		t.Fatalf("setup args %v", f.setupArgs)
	}
	f.sent = tgbridge.CodeSent{Hash: "h2", Via: "email", EmailPattern: "u***@m***.org"}
	r = telAuthPost(t, s, `{"action":"emailverify","phone":"+100","codeHash":"h1","code":" 777 "}`)
	if r["ok"] != true || r["codeHash"] != "h2" || r["via"] != "email" {
		t.Fatalf("emailverify: %v", r)
	}
	if strings.Join(f.verifyArg, "|") != "+100|h1|777" {
		t.Fatalf("verify args %v", f.verifyArg)
	}
	// Telegram signed in without a further code.
	f.sent = tgbridge.CodeSent{Done: true}
	if r := telAuthPost(t, s, `{"action":"emailverify","phone":"+100","codeHash":"h1","code":"777"}`); r["ok"] != true || r["done"] != true || r["codeHash"] != nil {
		t.Fatalf("done: %v", r)
	}
}

// Telegram's refusals in the email steps read as something the user can act on.
func TestTelEmailErrors(t *testing.T) {
	s := newTelTestServer(t)
	f := &fakeAuther{}
	withFakeAuth(t, f)
	for _, c := range []struct{ action, tgErr, want string }{
		{"emailsetup", "EMAIL_INVALID", "tel_err_email_invalid"},
		{"emailsetup", "EMAIL_NOT_ALLOWED", "tel_err_email_invalid"},
		{"emailverify", "CODE_INVALID", "tel_err_code_invalid"},
		{"emailverify", "EMAIL_VERIFY_EXPIRED", "tel_err_code_expired"},
		{"signin", "EMAIL_CODE_INVALID", "tel_err_code_invalid"},
		{"emailsetup", "FLOOD_WAIT", "tel_err_flood"},
	} {
		f.err = tgerr.New(400, c.tgErr)
		r := telAuthPost(t, s, `{"action":"`+c.action+`","phone":"+1","codeHash":"h","email":"a@b.c","code":"1","via":"email"}`)
		if r["ok"] != false || r["reason"] != c.want {
			t.Errorf("%s %s: %v", c.action, c.tgErr, r)
		}
	}
}

// Nothing connected: the email steps fail like every other step.
func TestTelEmailStepsNeedConnection(t *testing.T) {
	s := newTelTestServer(t)
	for _, a := range []string{"emailsetup", "emailverify"} {
		if r := telAuthPost(t, s, `{"action":"`+a+`","phone":"+1","codeHash":"h"}`); r["reason"] != "tel_err_not_connected" {
			t.Fatalf("%s: %v", a, r)
		}
	}
}
