package tui

import (
	"strings"
	"testing"
)

func TestExpandPastes(t *testing.T) {
	m := model{pastes: []string{"line1\nline2\nline3", "second block"}}
	in := "смотри [paste 3 lines] и ещё [paste 2 lines] конец"
	out := m.expandPastes(in)
	if !strings.Contains(out, "line1\nline2\nline3") || !strings.Contains(out, "second block") {
		t.Fatalf("paste not expanded: %q", out)
	}
	if strings.Contains(out, "[paste") {
		t.Errorf("labels should be gone after expansion: %q", out)
	}
}

// Labels expand in order of appearance: the i-th label maps to the i-th paste.
func TestExpandPastesInOrder(t *testing.T) {
	m := model{pastes: []string{"AAA", "BBB"}}
	out := m.expandPastes("[paste 2 lines] then [paste 5 lines]")
	if out != "AAA then BBB" {
		t.Fatalf("unexpected order/expansion: %q", out)
	}
}

// A label with no corresponding stored paste is left untouched.
func TestExpandPastesExtraLabelKept(t *testing.T) {
	m := model{pastes: []string{"only one"}}
	out := m.expandPastes("[paste 2 lines] and [paste 9 lines]")
	if !strings.Contains(out, "only one") || !strings.Contains(out, "[paste 9 lines]") {
		t.Errorf("extra label should be left as-is: %q", out)
	}
}

func TestExpandPastesNoneStored(t *testing.T) {
	m := model{}
	in := "literal [paste 3 lines] stays"
	if out := m.expandPastes(in); out != in {
		t.Errorf("with no stored pastes, text should be unchanged: %q", out)
	}
}
