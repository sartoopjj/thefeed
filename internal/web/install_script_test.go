package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Re-running the installer is how the server is updated, so anything it
// writes back hard-coded is a setting the operator silently loses every time.
// The X RSS (nitter) list was written that way: every update reset it to the
// default, whatever the operator had configured.
func TestInstallScriptPreservesOperatorSettings(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Skip("install script not present")
	}
	src := string(raw)

	// Each THEFEED_* the script writes into the env file must take its value
	// from a shell variable, not a literal -- otherwise an update overwrites
	// what is already there.
	assign := regexp.MustCompile(`(?m)^(THEFEED_[A-Z0-9_]+)=(.*)$`)
	// These are genuinely derived per install rather than carried over.
	computed := map[string]bool{
		"THEFEED_NO_TELEGRAM": true,
	}
	for _, m := range assign.FindAllStringSubmatch(src, -1) {
		name, val := m[1], strings.TrimSpace(m[2])
		if computed[name] {
			continue
		}
		if !strings.Contains(val, "$") {
			t.Errorf("%s is written back as the literal %q; every update "+
				"discards whatever the operator had set", name, val)
		}
	}

	// And the list in question is actually read back on update.
	if !strings.Contains(src, "env_get THEFEED_X_RSS_INSTANCES") {
		t.Error("the X RSS list is not read from the existing env file on update")
	}
}
