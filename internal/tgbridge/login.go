package tgbridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ErrSignUpRequired: the number has no Telegram account. Creating one is left
// to the official app.
var ErrSignUpRequired = errors.New("tgbridge: no account for this number")

// CodeSent is Telegram's answer to a request for a login code.
type CodeSent struct {
	Hash string
	// Via is where the code went (see deliveryOf).
	Via string
	// EmailPattern is the masked address an email code went to, e.g.
	// "s•••@g•••.com".
	EmailPattern string
	// Done: Telegram signed the session in without wanting a code.
	Done bool
}

// codeSentOf reads an auth.SentCode, whichever kind it is.
func codeSentOf(sent tg.AuthSentCodeClass) (CodeSent, error) {
	switch c := sent.(type) {
	case *tg.AuthSentCode:
		out := CodeSent{Hash: c.PhoneCodeHash, Via: deliveryOf(c.Type)}
		if e, ok := c.Type.(*tg.AuthSentCodeTypeEmailCode); ok {
			out.EmailPattern = e.EmailPattern
		}
		return out, nil
	case *tg.AuthSentCodeSuccess:
		return CodeSent{Done: true}, nil
	default:
		return CodeSent{}, fmt.Errorf("tgbridge: unexpected sent-code type %T", sent)
	}
}

// deliveryOf names where Telegram actually put the code. Without it the user is
// told only that a code was sent, and the commonest case by far -- Telegram
// delivering it INSIDE an app the user is already signed in to, not by SMS --
// looks exactly like a code that never arrived.
//
// "setupemail" is not a delivery at all: no code was sent, and Telegram wants
// a login email added first (SetupLoginEmail). "firebase" is an SMS only the
// official mobile apps can receive.
func deliveryOf(t tg.AuthSentCodeTypeClass) string {
	switch t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return "app"
	case *tg.AuthSentCodeTypeSMS, *tg.AuthSentCodeTypeSMSWord, *tg.AuthSentCodeTypeSMSPhrase:
		return "sms"
	case *tg.AuthSentCodeTypeFragmentSMS:
		return "fragment"
	case *tg.AuthSentCodeTypeFirebaseSMS:
		return "firebase"
	case *tg.AuthSentCodeTypeCall:
		return "call"
	case *tg.AuthSentCodeTypeFlashCall, *tg.AuthSentCodeTypeMissedCall:
		return "missedcall"
	case *tg.AuthSentCodeTypeEmailCode:
		return "email"
	case *tg.AuthSentCodeTypeSetUpEmailRequired:
		return "setupemail"
	default:
		return ""
	}
}

// signInByEmail completes the code step for a code that came by email: it goes
// in email_verification, not phone_code.
func signInByEmail(ctx context.Context, api *tg.Client, phone, code, codeHash string) error {
	res, err := api.AuthSignIn(ctx, &tg.AuthSignInRequest{
		PhoneNumber:       phone,
		PhoneCodeHash:     codeHash,
		EmailVerification: &tg.EmailVerificationCode{Code: code},
	})
	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		return auth.ErrPasswordAuthNeeded
	}
	if err != nil {
		return err
	}
	if _, ok := res.(*tg.AuthAuthorizationSignUpRequired); ok {
		return ErrSignUpRequired
	}
	return nil
}

// loginSetupPurpose ties an email step to the login in progress.
func loginSetupPurpose(phone, codeHash string) *tg.EmailVerifyPurposeLoginSetup {
	return &tg.EmailVerifyPurposeLoginSetup{PhoneNumber: phone, PhoneCodeHash: codeHash}
}

// SetupLoginEmail answers "setupemail": Telegram sends a code to the email
// the user gives, to prove they can read it. Returns the masked address.
func (b *Bridge) SetupLoginEmail(ctx context.Context, phone, codeHash, email string) (string, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return "", err
	}
	return setupLoginEmail(ctx, api, phone, codeHash, email)
}

func setupLoginEmail(ctx context.Context, api *tg.Client, phone, codeHash, email string) (string, error) {
	sent, err := api.AccountSendVerifyEmailCode(ctx, &tg.AccountSendVerifyEmailCodeRequest{
		Purpose: loginSetupPurpose(phone, codeHash),
		Email:   email,
	})
	if err != nil {
		return "", err
	}
	return sent.EmailPattern, nil
}

// VerifyLoginEmail checks the code SetupLoginEmail sent. On success the email
// becomes the account's login email and Telegram sends the LOGIN code, which
// SignIn takes next.
func (b *Bridge) VerifyLoginEmail(ctx context.Context, phone, codeHash, code string) (CodeSent, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return CodeSent{}, err
	}
	return verifyLoginEmail(ctx, api, phone, codeHash, code)
}

func verifyLoginEmail(ctx context.Context, api *tg.Client, phone, codeHash, code string) (CodeSent, error) {
	res, err := api.AccountVerifyEmail(ctx, &tg.AccountVerifyEmailRequest{
		Purpose:      loginSetupPurpose(phone, codeHash),
		Verification: &tg.EmailVerificationCode{Code: code},
	})
	if err != nil {
		return CodeSent{}, err
	}
	login, ok := res.(*tg.AccountEmailVerifiedLogin)
	if !ok {
		return CodeSent{}, fmt.Errorf("tgbridge: unexpected verify-email answer %T", res)
	}
	return codeSentOf(login.SentCode)
}
