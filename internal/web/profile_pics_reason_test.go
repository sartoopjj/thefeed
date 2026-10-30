package web

import (
	"context"
	"os"
	"strings"
	"testing"
)

// "Avatars did not download" has to come with a reason.
//
// All three ways the GitHub path can be unavailable are on the server side,
// where the user cannot see them, and each used to leave the refresh silent:
// no error, no log, nothing on screen. The fetch itself swallowed a failed
// relay-info lookup the same way, so a broken relay was indistinguishable
// from a server that simply has none.
func TestProfilePicsRecordsWhyGitHubWasNotUsed(t *testing.T) {
	h := &profilePicsHub{}

	// No relay fetcher at all (a build without it).
	h.setProgressErr("")
	h.noteGitHubUnavailable(nil, false, 0)
	if got := h.progressErr(); !strings.Contains(got, "not available") {
		t.Errorf("missing fetcher reported as %q", got)
	}

	// The server published no GitHub relay for the bundle.
	h.setProgressErr("")
	h.noteGitHubUnavailable(func(context.Context, int64, uint32) ([]byte, error) { return nil, nil }, false, 10)
	if got := h.progressErr(); !strings.Contains(got, "did not publish") {
		t.Errorf("absent relay reported as %q", got)
	}

	// The bundle is there but empty.
	h.setProgressErr("")
	h.noteGitHubUnavailable(func(context.Context, int64, uint32) ([]byte, error) { return nil, nil }, true, 0)
	if got := h.progressErr(); !strings.Contains(got, "empty bundle") {
		t.Errorf("empty bundle reported as %q", got)
	}

	// And when everything is in place, nothing is reported.
	h.setProgressErr("")
	h.noteGitHubUnavailable(func(context.Context, int64, uint32) ([]byte, error) { return nil, nil }, true, 10)
	if got := h.progressErr(); got != "" {
		t.Errorf("a working setup reported %q", got)
	}
}

// The helper above is tested by behaviour; this checks it is actually WIRED
// into the refresh path. A source check is weaker than driving refresh for
// real, which would need a fake DNS directory served to a Fetcher -- but it
// does catch the call site being deleted, which is how this would regress.
func TestRefreshReportsWhenGitHubIsUnavailable(t *testing.T) {
	src, err := os.ReadFile("profile_pics.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (h *profilePicsHub) refresh(")
	if i < 0 {
		t.Fatal("refresh not found")
	}
	if !strings.Contains(body[i:], "h.noteGitHubUnavailable(") {
		t.Error("refresh no longer reports why the GitHub path was skipped; " +
			"avatars go missing with nothing on screen or in the log to say why")
	}
}
