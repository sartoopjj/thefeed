package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Every configurable flag must also be settable from the environment, so the
// server runs the same from compose, a unit file, or a shell. The one-shot
// actions are deliberately excluded: an environment variable that makes the
// server print something and exit would be a trap in a unit file nobody
// re-reads.
func TestServerFlagsHaveEnvVars(t *testing.T) {
	src, err := exec.Command("cat", "main.go").Output()
	if err != nil {
		t.Skip("cannot read main.go")
	}
	body := string(src)

	// Configuration that an operator sets to run the server.
	for _, pair := range [][2]string{
		{"data-dir", "THEFEED_DATA_DIR"},
		{"listen", "THEFEED_LISTEN"},
		{"channels", "THEFEED_CHANNELS"},
		{"private-channels", "THEFEED_PRIVATE_CHANNELS"},
		{"x-accounts", "THEFEED_X_ACCOUNTS"},
		{"session", "THEFEED_SESSION"},
		{"no-telegram", "THEFEED_NO_TELEGRAM"},
	} {
		if !strings.Contains(body, `"`+pair[1]+`"`) {
			t.Errorf("--%s has no %s", pair[0], pair[1])
		}
	}

	// A flag typed on the command line must win, and that has to be decided
	// by whether it was GIVEN -- comparing against the default cannot tell
	// "--data-dir ./data" from not passing it at all.
	if !strings.Contains(body, "flag.Visit(") {
		t.Error("precedence is not decided by which flags were actually given")
	}

	// And the one-shot actions stay off the environment.
	for _, env := range []string{"THEFEED_VERSION", "THEFEED_PRINT_CONFIG", "THEFEED_REPORT", "THEFEED_LOGIN_ONLY"} {
		if strings.Contains(body, `"`+env+`"`) {
			t.Errorf("%s makes a one-shot action settable from the environment", env)
		}
	}
}
