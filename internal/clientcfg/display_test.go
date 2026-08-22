package clientcfg

import (
	"os"
	"testing"

	"golang.org/x/term"
)

// TestShouldUseANSI_NO_COLOR pins the half of the ANSI decision that is
// observable under `go test`. The other half — stdout being a terminal — is not:
// it never is here, which is why the TTY branch is exercised through the
// mdShouldUseANSI seam in internal/tui rather than by manipulating the
// environment.
//
// t.Setenv restores the previous value even when the test fails part-way. A
// leaked NO_COLOR would silently disable colour for every test running after
// this one in the same process, and the failure would surface somewhere else.
func TestShouldUseANSI_NO_COLOR(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if ShouldUseANSI() {
		t.Error("NO_COLOR is set, so no escape sequences should be emitted")
	}

	// Empty is not absent for most variables, but NO_COLOR is tested for
	// non-emptiness, so an empty value must not disable colour by itself. With
	// NO_COLOR out of the way the answer is exactly the TTY check, and asserting
	// that equality keeps the test meaningful wherever it runs: under `go test`
	// both sides are false, on a real terminal both are true.
	t.Setenv("NO_COLOR", "")
	if got, want := ShouldUseANSI(), term.IsTerminal(int(os.Stdout.Fd())); got != want {
		t.Errorf("with empty NO_COLOR the answer should be the TTY check: got %v, want %v", got, want)
	}
}
