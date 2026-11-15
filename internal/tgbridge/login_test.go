package tgbridge

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// fakeTG answers each call with whatever fn returns, through the real TL
// encoding, and records the requests.
type fakeTG struct {
	fn   func(req bin.Encoder) (bin.Encoder, error)
	reqs []bin.Encoder
}

func (f *fakeTG) Invoke(_ context.Context, in bin.Encoder, out bin.Decoder) error {
	f.reqs = append(f.reqs, in)
	res, err := f.fn(in)
	if err != nil {
		return err
	}
	var b bin.Buffer
	if err := res.Encode(&b); err != nil {
		return err
	}
	return out.Decode(&b)
}

func fakeAPI(fn func(req bin.Encoder) (bin.Encoder, error)) (*tg.Client, *fakeTG) {
	f := &fakeTG{fn: fn}
	return tg.NewClient(f), f
}

// Every way Telegram can deliver a code has a name, and the two that are not
// a normal delivery are told apart.
func TestDeliveryOf(t *testing.T) {
	for _, c := range []struct {
		t    tg.AuthSentCodeTypeClass
		want string
	}{
		{&tg.AuthSentCodeTypeApp{}, "app"},
		{&tg.AuthSentCodeTypeSMS{}, "sms"},
		{&tg.AuthSentCodeTypeCall{}, "call"},
		{&tg.AuthSentCodeTypeMissedCall{}, "missedcall"},
		{&tg.AuthSentCodeTypeFragmentSMS{}, "fragment"},
		{&tg.AuthSentCodeTypeEmailCode{}, "email"},
		// No code was sent at all: a login email has to be added first.
		{&tg.AuthSentCodeTypeSetUpEmailRequired{}, "setupemail"},
		// Only the official apps can receive these.
		{&tg.AuthSentCodeTypeFirebaseSMS{}, "firebase"},
	} {
		if got := deliveryOf(c.t); got != c.want {
			t.Errorf("%T = %q, want %q", c.t, got, c.want)
		}
	}
}

// An email code carries the masked address, so the user knows which inbox.
func TestCodeSentOfEmail(t *testing.T) {
	got, err := codeSentOf(&tg.AuthSentCode{
		PhoneCodeHash: "h1",
		Type:          &tg.AuthSentCodeTypeEmailCode{EmailPattern: "a***@b***.com", Length: 6},
	})
	if err != nil || got != (CodeSent{Hash: "h1", Via: "email", EmailPattern: "a***@b***.com"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := codeSentOf(&tg.AuthSentCodeSuccess{}); err != nil || !got.Done {
		t.Fatalf("success: %+v, %v", got, err)
	}
}

// An email code goes in email_verification; sent as a phone code, Telegram
// rejects it.
func TestSignInByEmailRequest(t *testing.T) {
	api, f := fakeAPI(func(bin.Encoder) (bin.Encoder, error) {
		return &tg.AuthAuthorization{User: &tg.User{ID: 1}}, nil
	})
	if err := signInByEmail(context.Background(), api, "+100", "12345", "h1"); err != nil {
		t.Fatal(err)
	}
	req, ok := f.reqs[0].(*tg.AuthSignInRequest)
	if !ok {
		t.Fatalf("sent %T", f.reqs[0])
	}
	ev, ok := req.EmailVerification.(*tg.EmailVerificationCode)
	if !ok || ev.Code != "12345" || req.PhoneCode != "" || req.PhoneNumber != "+100" || req.PhoneCodeHash != "h1" {
		t.Fatalf("request %+v", req)
	}
	// And on the wire too: the email field present, the phone one absent.
	var b bin.Buffer
	if err := req.Encode(&b); err != nil {
		t.Fatal(err)
	}
	var back tg.AuthSignInRequest
	if err := back.Decode(&b); err != nil {
		t.Fatal(err)
	}
	if _, has := back.GetPhoneCode(); has {
		t.Fatal("phone_code set on the wire")
	}
	if _, has := back.GetEmailVerification(); !has {
		t.Fatal("email_verification missing on the wire")
	}
}

// Two-step verification and a number without an account come back as the
// same errors the phone path gives.
func TestSignInByEmailOutcomes(t *testing.T) {
	api, _ := fakeAPI(func(bin.Encoder) (bin.Encoder, error) {
		return nil, tgerr.New(401, "SESSION_PASSWORD_NEEDED")
	})
	if err := signInByEmail(context.Background(), api, "+1", "1", "h"); !errors.Is(err, auth.ErrPasswordAuthNeeded) {
		t.Fatalf("2FA: %v", err)
	}
	api, _ = fakeAPI(func(bin.Encoder) (bin.Encoder, error) {
		return &tg.AuthAuthorizationSignUpRequired{}, nil
	})
	if err := signInByEmail(context.Background(), api, "+1", "1", "h"); !errors.Is(err, ErrSignUpRequired) {
		t.Fatalf("no account: %v", err)
	}
	api, _ = fakeAPI(func(bin.Encoder) (bin.Encoder, error) {
		return nil, tgerr.New(400, "PHONE_CODE_INVALID")
	})
	if err := signInByEmail(context.Background(), api, "+1", "1", "h"); !tgerr.Is(err, "PHONE_CODE_INVALID") {
		t.Fatalf("wrong code: %v", err)
	}
}

// The setup email step: a code to the address given, tied to this login.
func TestSetupLoginEmail(t *testing.T) {
	api, f := fakeAPI(func(bin.Encoder) (bin.Encoder, error) {
		return &tg.AccountSentEmailCode{EmailPattern: "u***@m***.org", Length: 6}, nil
	})
	got, err := setupLoginEmail(context.Background(), api, "+100", "h1", "u@m.org")
	if err != nil || got != "u***@m***.org" {
		t.Fatalf("got %q, %v", got, err)
	}
	req := f.reqs[0].(*tg.AccountSendVerifyEmailCodeRequest)
	p, ok := req.Purpose.(*tg.EmailVerifyPurposeLoginSetup)
	if !ok || p.PhoneNumber != "+100" || p.PhoneCodeHash != "h1" || req.Email != "u@m.org" {
		t.Fatalf("request %+v purpose %+v", req, req.Purpose)
	}
}

// Verifying the email gives the next step: the real login code, with its own
// hash, which replaces the old one.
func TestVerifyLoginEmail(t *testing.T) {
	api, f := fakeAPI(func(bin.Encoder) (bin.Encoder, error) {
		return &tg.AccountEmailVerifiedLogin{
			Email: "u@m.org",
			SentCode: &tg.AuthSentCode{
				PhoneCodeHash: "h2",
				Type:          &tg.AuthSentCodeTypeEmailCode{EmailPattern: "u***@m***.org", Length: 6},
			},
		}, nil
	})
	got, err := verifyLoginEmail(context.Background(), api, "+100", "h1", "777")
	if err != nil || got != (CodeSent{Hash: "h2", Via: "email", EmailPattern: "u***@m***.org"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	req := f.reqs[0].(*tg.AccountVerifyEmailRequest)
	p, _ := req.Purpose.(*tg.EmailVerifyPurposeLoginSetup)
	v, _ := req.Verification.(*tg.EmailVerificationCode)
	if p == nil || p.PhoneCodeHash != "h1" || v == nil || v.Code != "777" {
		t.Fatalf("request %+v", req)
	}
	// An answer for some other purpose is not a login step.
	api, _ = fakeAPI(func(bin.Encoder) (bin.Encoder, error) {
		return &tg.AccountEmailVerified{Email: "u@m.org"}, nil
	})
	if _, err := verifyLoginEmail(context.Background(), api, "+1", "h", "1"); err == nil {
		t.Fatal("non-login answer accepted")
	}
}
