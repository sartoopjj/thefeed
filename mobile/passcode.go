package mobile

import "github.com/sartoopjj/thefeed/internal/web"

// HashPasscode returns the stored form of the app-lock passcode ("" on
// failure).
func HashPasscode(passcode string) string { return web.HashPasscode(passcode) }

// CheckPasscode returns "" when passcode does not match stored, otherwise the
// value to keep stored (an older form comes back upgraded).
func CheckPasscode(passcode, stored string) string { return web.CheckPasscode(passcode, stored) }
