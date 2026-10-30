package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Writing a .env and running the server by hand used to do nothing: only
// `docker compose` (env_file:) and systemd (EnvironmentFile=) read one, and
// the binary fell back to its defaults without saying so.
func TestEnvFileIsReadButNeverOverridesTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	os.WriteFile(path, []byte(`
# a comment, and a blank line follow

THEFEED_DOMAIN=from-file.example
export THEFEED_KEY="quoted-pass"
THEFEED_LISTEN='single-quoted'
NOT_AN_ASSIGNMENT
THEFEED_ALREADY_SET=from-file
`), 0o600)

	// Something already in the real environment must not be replaced: that is
	// what keeps compose and systemd behaving exactly as before.
	t.Setenv("THEFEED_ALREADY_SET", "from-environment")
	for _, k := range []string{"THEFEED_DOMAIN", "THEFEED_KEY", "THEFEED_LISTEN"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}

	n, err := loadEnvFile(path)
	if err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if n != 3 {
		t.Errorf("set %d variables, want 3", n)
	}
	for k, want := range map[string]string{
		"THEFEED_DOMAIN":      "from-file.example",
		"THEFEED_KEY":         "quoted-pass",   // quotes are syntax, not value
		"THEFEED_LISTEN":      "single-quoted", // and so are single quotes
		"THEFEED_ALREADY_SET": "from-environment",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// A file that is not there is not an error: most runs have no .env.
func TestMissingEnvFileIsNotAnError(t *testing.T) {
	if _, err := loadEnvFile(filepath.Join(t.TempDir(), "nope.env")); err == nil {
		t.Error("a missing file should report so, for the --env-file case")
	}
	// loadEnvFiles must simply move on.
	loadEnvFiles("", filepath.Join(t.TempDir(), "absent"))
}
