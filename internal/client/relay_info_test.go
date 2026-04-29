package client

import "testing"

func TestParseRelayInfo(t *testing.T) {
	cases := []struct {
		name string
		data string
		want RelayInfo
	}{
		{"empty", "", RelayInfo{}},
		{"gh only", "gh=owner/repo\n", RelayInfo{GitHubRepo: "owner/repo"}},
		{"unknown keys ignored", "gh=owner/repo\nfuture=meh\n", RelayInfo{GitHubRepo: "owner/repo"}},
		{"trims whitespace", "  gh = a/b \n", RelayInfo{GitHubRepo: "a/b"}},
		{"random noise tolerated", "weird text\nbut: fine\ngh=x/y\n", RelayInfo{GitHubRepo: "x/y"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseRelayInfo([]byte(c.data))
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
