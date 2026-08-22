package tui

import "qeuro/internal/styles"

// message is one rendered entry in the chat transcript.
type message struct {
	role styles.Role
	meta string // e.g. "sonnet-4.6 · medium"
	body string
}
