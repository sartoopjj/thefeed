package main

import (
	"os"
	"regexp"
	"testing"
)

// The install script shows each messenger limit's default when asking for it.
// A shell script cannot read them from here, so it repeats them; this keeps
// the two from drifting, which would offer operators a number the server does
// not actually use.
func TestInstallScriptChatDefaultsMatchServer(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	server := map[string]string{}
	for _, m := range regexp.MustCompile(`chatIntEnv\(\w+, (\d+), "(THEFEED_CHAT_\w+)"\)`).FindAllStringSubmatch(string(src), -1) {
		server[m[2]] = m[1]
	}
	asked := regexp.MustCompile(`"(THEFEED_CHAT_\w+)\|[^|]*\|(\d+)"`).FindAllStringSubmatch(string(script), -1)
	if len(asked) == 0 {
		t.Fatal("no messenger limits found in install.sh")
	}
	for _, m := range asked {
		key, def := m[1], m[2]
		want, ok := server[key]
		if !ok {
			t.Errorf("install.sh asks for %s, which the server does not read", key)
			continue
		}
		if def != want {
			t.Errorf("install.sh offers %s=%s by default, the server defaults to %s", key, def, want)
		}
	}
}
