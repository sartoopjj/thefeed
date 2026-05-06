package web

import "testing"

// TestSelectedListSurvivesSave round-trips a profiles.json with a
// SelectedList set and asserts it loads back unchanged. Regression
// for "after I changed profile, the resolver list wasn't the one I
// picked" — the live selection must persist across saves.
func TestSelectedListSurvivesSave(t *testing.T) {
	s := newTestServerWithProfiles(t, &ProfileList{
		Active: "p1",
		Profiles: []Profile{
			{ID: "p1"}, {ID: "p2"},
		},
		ActiveLists: []ActiveList{
			{Name: "Home", Resolvers: []string{"1.1.1.1:53"}},
			{Name: "Office", Resolvers: []string{"8.8.8.8:53"}},
		},
		SelectedList: "Office",
	})
	pl := loadProfilesT(t, s)
	pl.Active = "p2"
	if err := s.saveProfiles(pl); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := loadProfilesT(t, s)
	if got.SelectedList != "Office" {
		t.Errorf("SelectedList = %q after profile switch save, want Office", got.SelectedList)
	}
}
