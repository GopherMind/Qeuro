// Package styles holds the visual language of Qeuro CLI: the brand palette,
// reusable lipgloss styles and the building blocks (logo, message blocks,
// status bar) that every screen is composed from.
//
// Visual language: sharp corners only (no rounded borders), panels rendered as
// a bold title header over a thin left gutter — a clean, code-first look in the
// style of modern agent terminals, not a boxed-in form UI. Colours are truecolor
// hex; lipgloss degrades them gracefully on 256/16-colour terminals.
package styles

import "github.com/charmbracelet/lipgloss"

// Brand palette — Qeuro. A focused terminal palette: cyan for intent, cobalt
// for agent chrome, amber for caution and restrained neutrals for long sessions.
var (
	// Accent2 is the primary brand accent (prompts, brand, active items):
	// a sleek, cosmic indigo representing neural connections.
	Accent2 = lipgloss.Color("#818CF8") // Glowing Indigo-400
	Indigo  = Accent2                   // alias for existing "accent" call sites
	Coral   = lipgloss.Color("#F43F5E") // warm highlight
	Cyan    = lipgloss.Color("#22D3EE") // bright cyan-400 (star / interactive accent)
	Sky     = lipgloss.Color("#C7D2FE") // info — light space indigo-200
	Blue    = Accent2
	Violet  = lipgloss.Color("#A78BFA") // space violet-400 (reasoning / status)
	Purple  = Violet

	Green = lipgloss.Color("#34D399") // emerald-400
	Amber = lipgloss.Color("#FBBF24") // amber-400
	Red   = lipgloss.Color("#F87171") // red-400
	Gray  = lipgloss.Color("#94A3B8") // slate-400 (space gray)
	Faint = lipgloss.Color("#334155") // slate-700
	Fg    = lipgloss.Color("#F1F5F9") // slate-100

	Surface  = lipgloss.Color("#080C14") // very deep void black-blue
	Surface2 = lipgloss.Color("#121824") // void panel slate-900
	Surface3 = lipgloss.Color("#1C2536") // void panel slate-800
	Border   = Accent2                   // Indigo border
)

// Core text styles.
var (
	Base    = lipgloss.NewStyle().Foreground(Fg)
	Muted   = lipgloss.NewStyle().Foreground(Gray)
	Subtle  = lipgloss.NewStyle().Foreground(Faint)
	Accent  = lipgloss.NewStyle().Foreground(Indigo).Bold(true)
	UserTag = lipgloss.NewStyle().Foreground(Cyan).Bold(true)
	OK      = lipgloss.NewStyle().Foreground(Green)
	Warn    = lipgloss.NewStyle().Foreground(Amber)
	Err     = lipgloss.NewStyle().Foreground(Red)
	Strong  = lipgloss.NewStyle().Foreground(Fg).Bold(true)

	// Highlight for the matched characters of a slash command while filtering.
	Match = lipgloss.NewStyle().Foreground(Cyan).Bold(true).Underline(true)

	// SelBg is the highlighted background of the focused row in a selector.
	SelBg = lipgloss.Color("#1E1B4B") // deep purple-950 selection bar

	// Selected fills the focused row: bright text on a neutral bar.
	Selected = lipgloss.NewStyle().Foreground(Fg).Background(SelBg).Bold(true)
	// SelectedNote is the dimmer description text inside a focused row.
	SelectedNote = lipgloss.NewStyle().Foreground(lipgloss.Color("#C7D2FE")).Background(SelBg)
)
