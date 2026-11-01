package web

import "testing"

// The Telegram intro, once read, stays read when the client comes up on a new
// port: the flag lives on the backend, not in the page's per-port storage.
func TestTelIntroSeenSurvivesANewPort(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	if got := getSettings(t, s)["telIntroSeen"]; got != false {
		t.Fatalf("fresh install: %v", got)
	}
	postSettings(t, s, `{"telIntroSeen":true}`)
	if got := getSettings(t, s)["telIntroSeen"]; got != true {
		t.Fatalf("after reading it: %v", got)
	}
	// Another setting saved later must not reset it.
	postSettings(t, s, `{"mirrorNoteOff":true}`)
	if got := getSettings(t, s)["telIntroSeen"]; got != true {
		t.Fatalf("lost to another save: %v", got)
	}
}
