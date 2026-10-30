package main

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// loadEnvFiles reads KEY=VALUE settings from the first file it finds, so a
// .env written next to the binary works when the server is started by hand.
//
// Only `docker compose` (env_file:) and systemd (EnvironmentFile=) used to
// read one; running the binary directly ignored it completely and fell back
// to defaults without saying so.
//
// A variable already present in the real environment is never overwritten --
// that is what keeps compose and systemd behaving exactly as before, and it
// means an explicit `FOO=bar thefeed-server` still wins over the file.
func loadEnvFiles(explicit, dataDir string) {
	var candidates []string
	if explicit != "" {
		candidates = []string{explicit}
	} else {
		candidates = []string{
			".env",
			filepath.Join(dataDir, ".env"),
			// The name the install script writes, so the systemd layout also
			// works when the same server is run by hand for a one-off.
			filepath.Join(dataDir, "thefeed.env"),
		}
	}
	for _, path := range candidates {
		n, err := loadEnvFile(path)
		if err != nil {
			if explicit != "" {
				// Asked for by name: silence would look like it was applied.
				log.Printf("[server] env file %s: %v", path, err)
			}
			continue
		}
		log.Printf("[server] read %d setting(s) from %s", n, path)
		return
	}
}

// loadEnvFile applies one file and reports how many variables it set.
func loadEnvFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `export FOO=bar` is how people write these by hand.
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val = strings.TrimSpace(val)
		// Quotes are part of the syntax, not the value.
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' ||
			val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		if _, already := os.LookupEnv(key); already {
			continue // the real environment wins
		}
		if err := os.Setenv(key, val); err == nil {
			n++
		}
	}
	return n, sc.Err()
}
