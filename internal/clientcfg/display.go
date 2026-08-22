package clientcfg

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// ShouldUseANSI reports whether ANSI escape sequences should be emitted.
// Returns false if NO_COLOR is set or stdout is not a terminal (piped/redirected).
func ShouldUseANSI() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// DisplaySafe removes control characters from s, making it safe to print to a
// terminal. Control characters are shown as \xNN escape sequences; newlines are
// also escaped. Tabs are preserved. This prevents untrusted text from
// repositioning the cursor, changing colours, or injecting commands.
//
// Use this on any text that comes from outside the CLI before printing it:
// model output, file paths from the server, error messages from the network.
func DisplaySafe(s string) string {
	if indexControlForDisplaySafe(s) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\t':
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// DisplaySafeBlock is like DisplaySafe but preserves newlines, making it
// suitable for multi-line blocks (e.g. resumed conversation turns, journal
// previews). Single-line fields (file paths, summaries) should use DisplaySafe.
func DisplaySafeBlock(s string) string {
	if indexControlForDisplay(s) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\t', c == '\n':
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// indexControlForDisplaySafe returns the index of the first control character
// in s that DisplaySafe would escape (including newlines), or -1 if none found.
func indexControlForDisplaySafe(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return i
		}
	}
	return -1
}

// indexControlForDisplay returns the index of the first control character in s,
// or -1 if s contains no control characters. Tabs and newlines are not treated
// as control characters for this purpose (callers decide whether to keep them).
func indexControlForDisplay(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t' && c != '\n') || c == 0x7f {
			return i
		}
	}
	return -1
}
