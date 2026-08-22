package commands

import "testing"

// M7: /login, /logout and /providers must be discoverable from the palette.
func TestRegistryIncludesAccountCommands(t *testing.T) {
	want := map[string]bool{"login": false, "logout": false, "providers": false}
	for _, c := range All() {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("command %q missing from registry", name)
		}
	}
}

func TestFilterRanksLoginFirstForLogPrefix(t *testing.T) {
	got := Filter("log")
	if len(got) < 2 {
		t.Fatalf("Filter(log) should match login and logout, got %d results", len(got))
	}
	if got[0].Name != "login" || got[1].Name != "logout" {
		t.Fatalf("Filter(log) should rank login, logout first, got %s, %s", got[0].Name, got[1].Name)
	}
}
