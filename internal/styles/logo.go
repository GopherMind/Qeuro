package styles

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderSpaceBunny renders Clawd the space bunny mascot.
func renderSpaceBunny() []string {
	bunnyStyle := lipgloss.NewStyle().Foreground(Violet).Bold(true)
	starStyle := lipgloss.NewStyle().Foreground(Cyan).Bold(true)
	return []string{
		"    " + starStyle.Render("✦") + "    ",
		" " + bunnyStyle.Render("█     █") + " ",
		" " + bunnyStyle.Render("█     █") + " ",
		bunnyStyle.Render("█████████"),
		bunnyStyle.Render("███") + " " + bunnyStyle.Render("█") + " " + bunnyStyle.Render("███"),
		bunnyStyle.Render("█████████"),
		"  " + bunnyStyle.Render("█   █") + "  ",
	}
}

// renderVerticalMascot renders the space bunny mascot.
func renderVerticalMascot() []string {
	return renderSpaceBunny()
}

// renderMascotAndLogo renders the space bunny mascot next to the wordmark.
func renderMascotAndLogo() []string {
	bunny := renderSpaceBunny()
	logoStyle1 := lipgloss.NewStyle().Foreground(Accent2)
	logoStyle2 := lipgloss.NewStyle().Foreground(Sky)

	l1 := logoStyle1.Render(" ____  ____  ") + logoStyle2.Render("_  _  ____  ____")
	l2 := logoStyle1.Render(" |  |  |___  ") + logoStyle2.Render("|  |  |__/  |  |")
	l3 := logoStyle1.Render(" |_\\|  |___  ") + logoStyle2.Render("|__|  |  \\  |__|")

	return []string{
		bunny[0],
		bunny[1],
		bunny[2] + "   " + l1,
		bunny[3] + "   " + l2,
		bunny[4] + "   " + l3,
		bunny[5],
		bunny[6],
	}
}

// Logo returns the full wordmark plus a tagline (used by version/help commands).
func Logo(version string) string {
	var b strings.Builder
	for _, line := range renderMascotAndLogo() {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString("  ")
	b.WriteString(Pill("✨ QEURO", Accent2))
	b.WriteString(Subtle.Render("  ·  "))
	b.WriteString(Muted.Render("cosmic space bunny edition"))
	b.WriteString(Subtle.Render("  ·  "))
	b.WriteString(Muted.Render("v" + version))
	return b.String()
}

// LogoCard renders the startup welcome block.
// LogoCompact renders a minimal one-line welcome for narrow terminals.
func LogoCompact(version, activeModel string) string {
	var b strings.Builder
	b.WriteString("\n  ")
	b.WriteString(Accent.Render("✳ qeuro"))
	b.WriteString(Subtle.Render("  v" + version))
	if activeModel != "" {
		b.WriteString(Subtle.Render("  ·  " + activeModel))
	}
	b.WriteString("\n")
	return b.String()
}

func LogoCard(version, activeModel string, total int) string {
	// Narrow mode: skip the two-column card and render a compact one-liner.
	if IsNarrow(total) {
		return LogoCompact(version, activeModel)
	}

	boxW := int(float64(total) * 0.90)
	if boxW < 60 {
		boxW = 60
	}

	innerW := boxW - 2
	leftW := int(float64(innerW) * 0.48)
	rightW := innerW - leftW - 1

	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}
	if username == "" {
		username = "Developer"
	}

	var left []string
	left = append(left,
		"",
		Strong.Render("Welcome back "+username+"!"),
		"",
	)
	left = append(left, renderSpaceBunny()...)
	left = append(left,
		"",
		Subtle.Render("model  ")+Muted.Render(activeModel),
		Subtle.Render("cwd    ")+Muted.Render(compactWD()),
		"",
	)

	right := []string{
		Muted.Render("Ask Qeuro to explain code, write tests, or find bugs."),
		"",
		Accent.Render("Quick start"),
		Muted.Render("/model — pick a model (8 free on every plan)"),
		Muted.Render("/team — plan → build → verify with an AI team"),
		Muted.Render("/resume — restore your last session"),
		"",
		Accent.Render("Account"),
		Muted.Render("qeuro login <token> · qeuro whoami · qeuro logout"),
	}

	for len(right) < len(left) {
		right = append(right, "")
	}
	for len(left) < len(right) {
		left = append(left, "")
	}

	if rightW < 30 {
		content := strings.Join(append(left, "", Chip("READY", Green)+" "+Muted.Render("Use /help or start typing.")), "\n")
		return "  " + Frame("Qeuro v"+version, content, boxW)
	}

	var b strings.Builder
	writeLine := func(s string) {
		b.WriteString("  " + s + "\n")
	}

	writeLine(borderTop(boxW, " Qeuro v"+version+" ", " Tips for getting started ", leftW, rightW))
	for i := range left {
		writeLine(borderRow(left[i], right[i], leftW, rightW))
	}
	writeLine(borderBottom(boxW, leftW, rightW))
	return strings.TrimRight(b.String(), "\n")
}

// CompactWD returns the working directory with the home prefix shortened to
// "~" — used by the welcome card.
func CompactWD() string { return compactWD() }

func compactWD() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if strings.HasPrefix(wd, home) {
			wd = "~" + strings.TrimPrefix(wd, home)
		}
	}
	return wd
}

func borderTop(width int, leftTitle, rightTitle string, leftW, rightW int) string {
	borderStyle := lipgloss.NewStyle().Foreground(Accent2)
	leftSegment := formatBorderSegment(leftW, leftTitle)
	rightSegment := formatBorderSegment(rightW, rightTitle)
	return borderStyle.Render("╭") +
		leftSegment +
		borderStyle.Render("┬") +
		rightSegment +
		borderStyle.Render("╮")
}

func borderBottom(width int, leftW, rightW int) string {
	borderStyle := lipgloss.NewStyle().Foreground(Accent2)
	return borderStyle.Render("╰") +
		borderStyle.Render(strings.Repeat("─", leftW)) +
		borderStyle.Render("┴") +
		borderStyle.Render(strings.Repeat("─", rightW)) +
		borderStyle.Render("╯")
}

func borderRow(left, right string, leftW, rightW int) string {
	borderStyle := lipgloss.NewStyle().Foreground(Accent2)

	if right == "---" {
		return borderStyle.Render("│") +
			centerCell(left, leftW) +
			borderStyle.Render("├") +
			borderStyle.Render(strings.Repeat("─", rightW)) +
			borderStyle.Render("┤")
	}

	rightContent := right
	if right != "" {
		rightContent = " " + right
	}

	return borderStyle.Render("│") +
		centerCell(left, leftW) +
		borderStyle.Render("│") +
		padCell(rightContent, rightW) +
		borderStyle.Render("│")
}

func centerCell(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return lipgloss.NewStyle().MaxWidth(width).Render(s)
	}
	left := (width - w) / 2
	right := width - w - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

func padCell(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return lipgloss.NewStyle().MaxWidth(width).Render(s)
	}
	return s + strings.Repeat(" ", width-w)
}

func formatBorderSegment(width int, title string) string {
	borderStyle := lipgloss.NewStyle().Foreground(Accent2)
	if title == "" {
		return borderStyle.Render(strings.Repeat("─", width))
	}
	styledTitle := Accent.Render(title)
	titleLen := lipgloss.Width(title)
	if titleLen+2 > width {
		return borderStyle.Render(strings.Repeat("─", width))
	}
	fill := width - titleLen - 1
	return borderStyle.Render("─") + styledTitle + borderStyle.Render(strings.Repeat("─", fill))
}
