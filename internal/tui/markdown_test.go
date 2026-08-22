package tui

import (
	"strings"
	"testing"
)

func TestLooksLikeMarkdown(t *testing.T) {
	positives := []string{
		"# Heading",
		"- item one\n- item two",
		"1. first\n2. second",
		"run `go test` locally",
		"```go\nfunc main() {}\n```",
		"**bold** claim",
		"> quoted",
	}
	for _, s := range positives {
		if !looksLikeMarkdown(s) {
			t.Errorf("looksLikeMarkdown(%q) = false, want true", s)
		}
	}
	negatives := []string{
		"plain sentence with no markers.",
		"another line\nand one more line",
	}
	for _, s := range negatives {
		if looksLikeMarkdown(s) {
			t.Errorf("looksLikeMarkdown(%q) = true, want false", s)
		}
	}
}

func TestRenderMarkdownFailsOpenOnPlainText(t *testing.T) {
	in := "plain sentence with no markers."
	if got := renderMarkdown(in, 80); got != in {
		t.Fatalf("plain text must pass through unchanged, got %q", got)
	}
	if got := renderMarkdown("", 80); got != "" {
		t.Fatalf("empty body must stay empty, got %q", got)
	}
}

func TestRenderMarkdownRendersBlocks(t *testing.T) {
	in := "# Title\n\n- item\n\n```go\nfunc main() {}\n```"
	out := renderMarkdown(in, 60)
	if strings.TrimSpace(out) == "" {
		t.Fatal("rendered markdown is empty")
	}
	if strings.Contains(out, "```") {
		t.Fatal("code fences should be rendered away, not echoed")
	}
}

// TestRenderMarkdownHonoursNoColor covers the roadmap's "автоотключение ANSI при
// пайпе и NO_COLOR" for the markdown path, and specifically the reason that
// clause needed code rather than just a check: the renderer is memoised.
//
// A renderer cached while colour was on keeps emitting colour after the mode
// changes, so it is not enough for renderMarkdown to consult ShouldUseANSI - the
// answer has to be part of the memo key. The test therefore renders the same body
// in both modes inside one process, which is the only arrangement where the stale
// cache is observable; a single colourless run would pass against a renderer that
// never re-checks.
//
// It drives mdShouldUseANSI rather than NO_COLOR because stdout is not a terminal
// under `go test`: the real predicate answers false either way, so setting the
// env var would compare false against false and prove nothing. NO_COLOR itself is
// covered where it is read, in clientcfg.TestShouldUseANSI_NO_COLOR.
func TestRenderMarkdownHonoursNoColor(t *testing.T) {
	const in = "# Title\n\n- item"

	reset := func() {
		mdMu.Lock()
		mdRenderer = nil
		mdMu.Unlock()
	}
	orig := mdShouldUseANSI
	t.Cleanup(func() {
		mdShouldUseANSI = orig
		// Drop the renderer this test built, so the next caller in the process
		// rebuilds against the real predicate.
		reset()
	})
	reset()

	mdShouldUseANSI = func() bool { return true }
	withColour := renderMarkdown(in, 60)
	if !strings.Contains(withColour, "\x1b[") {
		t.Fatalf("colour mode produced no ANSI at all: %q", withColour)
	}

	// No reset here on purpose: the cached renderer from the call above is
	// exactly what must not be reused.
	mdShouldUseANSI = func() bool { return false }
	plain := renderMarkdown(in, 60)

	if strings.Contains(plain, "\x1b[") {
		t.Errorf("ANSI survived with colour disabled - the memo key ignores the mode, so the cached renderer keeps colouring: %q", plain)
	}
	if strings.TrimSpace(plain) == "" {
		t.Error("colourless output is empty; the text still has to be readable")
	}
	if !strings.Contains(plain, "Title") || !strings.Contains(plain, "item") {
		t.Errorf("colourless output lost the content: %q", plain)
	}
}
