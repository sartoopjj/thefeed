package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServerWithProfiles writes profiles.json into a temp dir and
// returns a minimal *Server pointed at it. The clients map is a
// non-nil empty map so broadcast() doesn't blow up on a nil iteration.
func newTestServerWithProfiles(t *testing.T, pl *ProfileList) *Server {
	t.Helper()
	dir := t.TempDir()
	s := &Server{dataDir: dir, clients: map[chan string]struct{}{}}
	if pl != nil {
		if err := s.saveProfiles(pl); err != nil {
			t.Fatalf("save initial profiles: %v", err)
		}
	}
	return s
}

// loadProfilesT is the test-side reload helper.
func loadProfilesT(t *testing.T, s *Server) *ProfileList {
	t.Helper()
	pl, err := s.loadProfiles()
	if err != nil {
		t.Fatalf("loadProfiles: %v", err)
	}
	return pl
}

func TestPruneResolverFromLists(t *testing.T) {
	pl := &ProfileList{
		ActiveLists: []ActiveList{
			{Name: "Home", Resolvers: []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}},
			{Name: "Work", Resolvers: []string{"8.8.8.8:53", "9.9.9.9:53"}},
			{Name: "Empty"},
		},
	}
	if !pruneResolverFromLists(pl, "8.8.8.8:53") {
		t.Fatal("expected change reported")
	}
	got := pl.ActiveLists[0].Resolvers
	if len(got) != 2 || got[0] != "1.1.1.1:53" || got[1] != "9.9.9.9:53" {
		t.Errorf("Home list = %v, want [1.1.1.1:53 9.9.9.9:53]", got)
	}
	if len(pl.ActiveLists[1].Resolvers) != 1 {
		t.Errorf("Work list = %v, want 1 resolver", pl.ActiveLists[1].Resolvers)
	}
	// Pruning a resolver no one references is a no-op.
	if pruneResolverFromLists(pl, "10.0.0.1:53") {
		t.Error("expected no-op change=false for unknown resolver")
	}
	// Empty / nil inputs.
	if pruneResolverFromLists(nil, "x") {
		t.Error("nil profile should not report change")
	}
	if pruneResolverFromLists(pl, "") {
		t.Error("empty resolver should not report change")
	}
}

func TestPruneResolversFromListsBatch(t *testing.T) {
	pl := &ProfileList{
		ActiveLists: []ActiveList{
			{Name: "A", Resolvers: []string{"a", "b", "c"}},
			{Name: "B", Resolvers: []string{"d", "e"}},
		},
	}
	removed := map[string]bool{"a": true, "c": true, "e": true}
	if !pruneResolversFromLists(pl, removed) {
		t.Fatal("expected change reported")
	}
	if got := strings.Join(pl.ActiveLists[0].Resolvers, ","); got != "b" {
		t.Errorf("A list = %q, want %q", got, "b")
	}
	if got := strings.Join(pl.ActiveLists[1].Resolvers, ","); got != "d" {
		t.Errorf("B list = %q, want %q", got, "d")
	}
}

func TestFindListCaseInsensitive(t *testing.T) {
	pl := &ProfileList{
		ActiveLists: []ActiveList{
			{Name: "Home WiFi"},
			{Name: "office"},
		},
	}
	if got := findList(pl, "home wifi"); got == nil || got.Name != "Home WiFi" {
		t.Errorf("findList lowercase = %v", got)
	}
	if got := findList(pl, "  OFFICE  "); got == nil || got.Name != "office" {
		t.Errorf("findList trimmed/upper = %v", got)
	}
	if got := findList(pl, "missing"); got != nil {
		t.Errorf("findList missing = %v, want nil", got)
	}
	if got := findList(nil, "x"); got != nil {
		t.Errorf("findList nil pl = %v, want nil", got)
	}
}

func TestSanitizeListName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Home  ", "Home"},
		{"", ""},
		{"   ", ""},
		// 33 chars → trimmed to 32.
		{strings.Repeat("x", 33), strings.Repeat("x", 32)},
	}
	for _, c := range cases {
		if got := sanitizeListName(c.in); got != c.want {
			t.Errorf("sanitizeListName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMigrateActiveLists verifies that legacy installs (no ActiveLists,
// non-empty ResolverBank) get a "Default" list seeded from the bank
// when migrateActiveLists runs. We exercise it via a fake Server with
// just enough wiring.
func TestMigrateActiveListsFromBank(t *testing.T) {
	pl := &ProfileList{
		ResolverBank: []string{"1.1.1.1:53", "8.8.8.8:53"},
	}
	s := &Server{} // dataDir empty → loadLastScan returns nil
	if !s.migrateActiveLists(pl) {
		t.Fatal("expected migration to seed a list")
	}
	if len(pl.ActiveLists) != 1 || pl.ActiveLists[0].Name != defaultListName {
		t.Fatalf("ActiveLists = %v", pl.ActiveLists)
	}
	if pl.SelectedList != defaultListName {
		t.Errorf("SelectedList = %q, want %q", pl.SelectedList, defaultListName)
	}
	if len(pl.ActiveLists[0].Resolvers) != 2 {
		t.Errorf("Default list resolvers = %v", pl.ActiveLists[0].Resolvers)
	}
}

func TestMigrateActiveListsNoBankNoLists(t *testing.T) {
	pl := &ProfileList{}
	s := &Server{}
	if s.migrateActiveLists(pl) {
		t.Error("expected migration to be a no-op when nothing to seed")
	}
	if len(pl.ActiveLists) != 0 {
		t.Errorf("ActiveLists = %v, want empty", pl.ActiveLists)
	}
}

func TestMigrateActiveListsRepairsSelection(t *testing.T) {
	pl := &ProfileList{
		ActiveLists: []ActiveList{
			{Name: "Home", Resolvers: []string{"a"}},
		},
		SelectedList: "Missing",
	}
	s := &Server{}
	if !s.migrateActiveLists(pl) {
		t.Fatal("expected SelectedList repair to count as a change")
	}
	if pl.SelectedList != "Home" {
		t.Errorf("SelectedList = %q, want %q", pl.SelectedList, "Home")
	}
}

// ===== persistLastScanToProfiles =====

func TestPersistLastScanSeedsEmptyListAndBank(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists:  []ActiveList{{Name: "Home", Resolvers: nil}},
		SelectedList: "Home",
	})
	s.persistLastScanToProfiles([]string{"1.1.1.1:53", "8.8.8.8:53"})
	pl := loadProfilesT(t, s)
	if got := pl.ActiveLists[0].Resolvers; len(got) != 2 {
		t.Errorf("Home list = %v, want 2 entries", got)
	}
	if got := pl.ResolverBank; len(got) != 2 {
		t.Errorf("ResolverBank = %v, want 2 entries", got)
	}
}

func TestPersistLastScanLeavesPopulatedListAlone(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists:  []ActiveList{{Name: "Home", Resolvers: []string{"existing:53"}}},
		SelectedList: "Home",
		ResolverBank: []string{"existing:53"},
	})
	s.persistLastScanToProfiles([]string{"new:53"})
	pl := loadProfilesT(t, s)
	if got := pl.ActiveLists[0].Resolvers; len(got) != 1 || got[0] != "existing:53" {
		t.Errorf("Home list mutated = %v, expected unchanged", got)
	}
	if got := pl.ResolverBank; len(got) != 1 || got[0] != "existing:53" {
		t.Errorf("ResolverBank mutated = %v, expected unchanged", got)
	}
}

func TestPersistLastScanIgnoresEmptyInput(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists: []ActiveList{{Name: "Home"}}, SelectedList: "Home",
	})
	s.persistLastScanToProfiles(nil)
	s.persistLastScanToProfiles([]string{})
	pl := loadProfilesT(t, s)
	if len(pl.ActiveLists[0].Resolvers) != 0 {
		t.Errorf("expected list still empty, got %v", pl.ActiveLists[0].Resolvers)
	}
}

// ===== persistScanResultsToList =====

func TestPersistScanResultsPopulatesEmptyList(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists:  []ActiveList{{Name: "Home"}},
		SelectedList: "Home",
	})
	s.persistScanResultsToList([]string{"a:53", "b:53"})
	pl := loadProfilesT(t, s)
	if got := pl.ActiveLists[0].Resolvers; len(got) != 2 {
		t.Errorf("Home list = %v, want 2 entries", got)
	}
}

func TestPersistScanResultsKeepsPopulatedListByDefault(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists:  []ActiveList{{Name: "Home", Resolvers: []string{"keep:53", "stay:53"}}},
		SelectedList: "Home",
	})
	// rescanReplaceList is false → must NOT shrink the saved list
	// when the periodic checker happens to find fewer healthy.
	s.persistScanResultsToList([]string{"keep:53"})
	pl := loadProfilesT(t, s)
	if got := pl.ActiveLists[0].Resolvers; len(got) != 2 {
		t.Errorf("populated list got shrunk to %v, want both kept", got)
	}
}

func TestPersistScanResultsRescanOverwrites(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists:  []ActiveList{{Name: "Home", Resolvers: []string{"old1:53", "old2:53"}}},
		SelectedList: "Home",
	})
	s.rescanFlagMu.Lock()
	s.rescanReplaceList = true
	s.rescanFlagMu.Unlock()
	s.persistScanResultsToList([]string{"new:53"})
	pl := loadProfilesT(t, s)
	if got := pl.ActiveLists[0].Resolvers; len(got) != 1 || got[0] != "new:53" {
		t.Errorf("Home list = %v, want [new:53]", got)
	}
	// Flag should be one-shot.
	s.rescanFlagMu.Lock()
	cleared := !s.rescanReplaceList
	s.rescanFlagMu.Unlock()
	if !cleared {
		t.Error("rescanReplaceList not cleared after consume")
	}
}

func TestPersistScanResultsSeedsDefaultListOnFirstRun(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{})
	s.persistScanResultsToList([]string{"a:53", "b:53"})
	pl := loadProfilesT(t, s)
	if len(pl.ActiveLists) != 1 || pl.ActiveLists[0].Name != defaultListName {
		t.Fatalf("ActiveLists = %v, want one Default list", pl.ActiveLists)
	}
	if got := pl.ActiveLists[0].Resolvers; len(got) != 2 {
		t.Errorf("Default list = %v, want 2 entries", got)
	}
	if pl.SelectedList != defaultListName {
		t.Errorf("SelectedList = %q, want %q", pl.SelectedList, defaultListName)
	}
}

// ===== handleResolverListAdd =====

func TestHandleResolverListAdd(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists: []ActiveList{
			{Name: "Home", Resolvers: []string{"keep:53"}},
		},
		ResolverBank: []string{"keep:53", "new:53"},
		SelectedList: "Other",
	})
	body, _ := json.Marshal(map[string]any{
		"name":      "Home",
		"resolvers": []string{"new:53", "keep:53"}, // one new + one already in list
	})
	req := httptest.NewRequest(http.MethodPost, "/api/resolvers/lists/add", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleResolverListAdd(rec, req)
	if rec.Code != 200 {
		t.Fatalf("handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Added int `json:"added"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Added != 1 || resp.Count != 2 {
		t.Errorf("response = %+v, want added=1 count=2", resp)
	}
	pl := loadProfilesT(t, s)
	got := pl.ActiveLists[0].Resolvers
	if len(got) != 2 || got[0] != "keep:53" || got[1] != "new:53" {
		t.Errorf("Home list = %v, want [keep:53 new:53]", got)
	}
}

func TestHandleResolverListAddRejectsMissingList(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{})
	body, _ := json.Marshal(map[string]any{"name": "Nope", "resolvers": []string{"a"}})
	req := httptest.NewRequest(http.MethodPost, "/api/resolvers/lists/add", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleResolverListAdd(rec, req)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404 for missing list", rec.Code)
	}
}

func TestHandleResolverListAddRejectsEmptyInput(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists: []ActiveList{{Name: "Home"}},
	})
	cases := []map[string]any{
		{"name": "", "resolvers": []string{"a"}},  // empty name
		{"name": "Home", "resolvers": []string{}}, // empty list
	}
	for i, c := range cases {
		body, _ := json.Marshal(c)
		req := httptest.NewRequest(http.MethodPost, "/api/resolvers/lists/add", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleResolverListAdd(rec, req)
		if rec.Code != 400 {
			t.Errorf("case %d: status = %d, want 400", i, rec.Code)
		}
	}
}

// ===== writeListsInfo (with/without resolver addresses) =====

func TestWriteListsInfoIncludeResolvers(t *testing.T) {
	type listEntry struct {
		Name      string   `json:"name"`
		Count     int      `json:"count"`
		Resolvers []string `json:"resolvers"`
	}
	type listResp struct {
		Lists []listEntry `json:"lists"`
	}

	s := newTestServerWithProfiles(t, &ProfileList{
		ActiveLists: []ActiveList{
			{Name: "Home", Resolvers: []string{"a", "b"}},
		},
		SelectedList: "Home",
	})
	rec := httptest.NewRecorder()
	s.writeListsInfo(rec, true)
	var withAddrs listResp
	if err := json.Unmarshal(rec.Body.Bytes(), &withAddrs); err != nil {
		t.Fatal(err)
	}
	if len(withAddrs.Lists) != 1 || withAddrs.Lists[0].Count != 2 || len(withAddrs.Lists[0].Resolvers) != 2 {
		t.Errorf("with-resolvers response = %+v", withAddrs)
	}

	// Default (no flag) omits the addresses. Use a *separate* resp
	// var — Go's json.Unmarshal leaves untouched fields untouched
	// when reusing a populated struct, so reusing the previous
	// `withAddrs` would falsely show Resolvers carried over.
	rec = httptest.NewRecorder()
	s.writeListsInfo(rec)
	var noAddrs listResp
	if err := json.Unmarshal(rec.Body.Bytes(), &noAddrs); err != nil {
		t.Fatal(err)
	}
	if len(noAddrs.Lists) != 1 || len(noAddrs.Lists[0].Resolvers) != 0 {
		t.Errorf("default response leaked addresses: %+v", noAddrs)
	}
}

