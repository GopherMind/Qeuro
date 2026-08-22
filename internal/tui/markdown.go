// Final-answer Markdown rendering (M5.2): completed agent replies are
// pretty-printed with glamour (headings, lists, tables, syntax-highlighted
// code blocks) using the dark or light standard style depending on the
// terminal background. Rendering happens only when a stream finishes —
// partial chunks stay plain so the live view never flickers — and it fails
// open: any renderer error returns the raw text unchanged.
//
// ANSI codes are automatically disabled when stdout is redirected (pipe or
// file) or when NO_COLOR is set, ensuring compatibility with CI logs and
// scripts (roadmap §8.render).
package tui

import (
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"qeuro/internal/clientcfg"
)

var (
	mdMu       sync.Mutex
	mdRenderer *glamour.TermRenderer
	mdWidth    int
	mdDark     bool
	mdANSI     bool

	// mdShouldUseANSI is the seam the tests drive. Under `go test` stdout is
	// never a terminal, so the real ShouldUseANSI answers false whatever
	// NO_COLOR says — which makes the two branches below indistinguishable and
	// the memo key untestable by environment alone. Naming the dependency here
	// keeps the production path a plain call while letting a test render the
	// same body in both modes within one process, the only arrangement in which
	// a stale cached renderer is observable.
	mdShouldUseANSI = clientcfg.ShouldUseANSI
)

// renderMarkdown renders body as terminal Markdown wrapped at width columns.
// Plain prose without Markdown markers is returned untouched so short chat
// replies keep the compact block styling.
//
// ANSI formatting is auto-disabled when stdout is not a TTY (pipe/redirect)
// or when NO_COLOR environment variable is set (roadmap §8.render).
func renderMarkdown(body string, width int) string {
	if strings.TrimSpace(body) == "" || !looksLikeMarkdown(body) {
		return body
	}
	if width < 24 {
		width = 24
	}
	dark := lipgloss.HasDarkBackground()
	ansi := mdShouldUseANSI()

	mdMu.Lock()
	defer mdMu.Unlock()
	if mdRenderer == nil || mdWidth != width || mdDark != dark || mdANSI != ansi {
		// The three branches differ only in which standard style is asked for;
		// wrapping and emoji are properties of the row regardless of colour.
		// "notty" is glamour's escape-free style, so a piped or NO_COLOR run
		// still gets the structure — headings, lists, tables — without codes.
		style := "notty"
		if ansi {
			style = "light"
			if dark {
				style = "dark"
			}
		}
		r, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle(style),
			glamour.WithWordWrap(width),
			glamour.WithEmoji(),
		)
		if err != nil {
			return body
		}
		mdRenderer, mdWidth, mdDark, mdANSI = r, width, dark, ansi
	}

	out, err := mdRenderer.Render(body)
	if err != nil || strings.TrimSpace(out) == "" {
		return body
	}
	return strings.Trim(out, "\n")
}

// looksLikeMarkdown reports whether body contains block or inline Markdown
// markers worth pretty-printing.
func looksLikeMarkdown(body string) bool {
	if strings.Contains(body, "```") || strings.Contains(body, "**") || strings.Contains(body, "`") {
		return true
	}
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "# "), strings.HasPrefix(t, "## "), strings.HasPrefix(t, "### "):
			return true
		case strings.HasPrefix(t, "- "), strings.HasPrefix(t, "* "), strings.HasPrefix(t, "> "), strings.HasPrefix(t, "| "):
			return true
		case len(t) > 2 && t[0] >= '1' && t[0] <= '9' && t[1] == '.' && t[2] == ' ':
			return true
		}
	}
	return false
}
