package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The UI must FETCH nothing from the internet.
//
// This is built for networks that are being shut down. An external asset there
// does not merely fail -- it fails slowly, blocking first paint behind a
// timeout, and it reaches out to a third party from a tool whose whole purpose
// is not being observed. A Google Fonts @import shipped in saved.css for
// exactly this reason: nobody noticed, because on a working connection it
// looks fine.
//
// Links the user CLICKS are not fetches and are allowed; so are XML namespace
// URIs, which are identifiers rather than addresses.
func TestFrontendFetchesNothingExternal(t *testing.T) {
	// Things that make the browser go and get something.
	fetchers := []*regexp.Regexp{
		regexp.MustCompile(`@import\s+url\(\s*['"]?(https?:)?//[^'")]+`),
		regexp.MustCompile(`url\(\s*['"]?(https?:)?//[^'")]+`),
		regexp.MustCompile(`<(?:script|img|iframe|audio|video|source|embed)[^>]+src\s*=\s*['"](https?:)?//`),
		regexp.MustCompile(`<link[^>]+href\s*=\s*['"](https?:)?//`),
		regexp.MustCompile(`@font-face[^}]*url\(\s*['"]?(https?:)?//`),
	}
	// Namespace declarations: identifiers, never fetched.
	nsAttr := regexp.MustCompile(`xmlns(:\w+)?\s*=|\bsodipodi:|\binkscape:`)

	root := filepath.Join("static")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".html", ".css", ".js", ".svg", ".mjs":
		default:
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if nsAttr.MatchString(line) {
				continue
			}
			for _, re := range fetchers {
				if m := re.FindString(line); m != "" {
					t.Errorf("%s:%d fetches an external asset: %s\n"+
						"the UI has to work with the network down, and must not "+
						"announce itself to a third party", path, i+1, strings.TrimSpace(m))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}
