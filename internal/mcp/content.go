package mcp

import (
	"strconv"
	"strings"
)

// maxResultBytes caps how much of a tool result reaches the model, mirroring
// maxCmdOutput in the tools package. A remote server is even less trustworthy
// than a local command about output size: it can stream megabytes that cost the
// user money on every subsequent request in the conversation.
const maxResultBytes = 16 << 10

// Text renders a tool result as the text the model will see, and reports whether
// anything was dropped.
//
// Only text content is forwarded. The other types are deliberately not passed
// through, each for its own reason:
//
//   - image/audio: the CLI's chat request has no multimodal path, so forwarding
//     base64 blobs would spend the user's tokens on data the model cannot use.
//   - resource_link: rendered as its URI without fetching. Auto-fetching would
//     let the server choose what the client downloads — an SSRF primitive
//     handed to a third party (.ai/RULES.md:22).
//   - resource: embedded text is forwarded; embedded blobs are named, not
//     decoded.
//
// The caller is responsible for fencing the returned text as untrusted data.
func (r CallResult) Text() (text string, truncated bool) {
	var b strings.Builder
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			b.WriteString(c.Text)
			if !strings.HasSuffix(c.Text, "\n") {
				b.WriteString("\n")
			}
		case "resource_link":
			b.WriteString("[resource link: " + sanitizeOneLine(c.URI))
			if c.Name != "" {
				b.WriteString(" (" + sanitizeOneLine(c.Name) + ")")
			}
			b.WriteString("]\n")
		case "resource":
			if c.Text != "" {
				b.WriteString(c.Text)
				if !strings.HasSuffix(c.Text, "\n") {
					b.WriteString("\n")
				}
				continue
			}
			b.WriteString("[embedded resource: " + sanitizeOneLine(c.URI) + " " + sanitizeOneLine(c.MIMEType) + " — not decoded]\n")
		case "image", "audio":
			b.WriteString("[" + c.Type + " content omitted: this client cannot forward it to the model]\n")
		default:
			// Forward-compatibility: a content type added in a later revision is
			// named, not dropped silently, so the user can see why a result looks
			// short.
			b.WriteString("[unsupported content type " + sanitizeOneLine(c.Type) + " omitted]\n")
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if len(out) > maxResultBytes {
		out = truncateUTF8(out, maxResultBytes) +
			"\n…[result truncated at " + strconv.Itoa(maxResultBytes) + " bytes]"
		truncated = true
	}
	if out == "" {
		// An empty result is a legitimate answer ("no issues found"), but an empty
		// string in the transcript reads as a failure. Say so explicitly.
		out = "(the tool returned no content)"
	}
	return out, truncated
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune, so the
// transcript cannot end in an invalid byte sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

// utf8Start reports whether b can begin a UTF-8 encoded rune.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// sanitizeOneLine strips control characters from a short server-supplied string
// that gets interpolated into a single line of transcript. This is not about
// injection — the whole result is fenced as untrusted by the caller — but about
// a server being unable to forge extra lines, ANSI escapes or a fence marker
// inside what looks like our own bracketed annotation.
func sanitizeOneLine(s string) string {
	const max = 256
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// dropped
		case r == ']':
			// A closing bracket would let the server end our annotation early and
			// continue in what reads as unquoted text.
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
		if b.Len() >= max {
			return b.String() + "…"
		}
	}
	return b.String()
}
