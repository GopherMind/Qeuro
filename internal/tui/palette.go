package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"qeuro/internal/commands"
	"qeuro/internal/styles"
)

// palette is the slash-command suggestion panel that opens when the input
// starts with "/". It filters in real time and supports keyboard navigation.
type palette struct {
	open     bool
	query    string // text after the leading slash
	items    []commands.Command
	selected int
}

// sync recomputes the filtered list for the current input line. It is called
// whenever the input changes. The panel opens only when the line starts with
// "/" and contains no spaces (a complete command has been typed otherwise).
func (p *palette) sync(line string) {
	if !strings.HasPrefix(line, "/") || strings.Contains(line, " ") {
		p.open = false
		p.query = ""
		return
	}
	p.open = true
	p.query = line[1:]
	p.items = commands.Filter(p.query)
	if p.selected >= len(p.items) {
		p.selected = 0
	}
}

func (p *palette) close() {
	p.open = false
	p.selected = 0
	p.query = ""
}

func (p *palette) up() {
	if len(p.items) == 0 {
		return
	}
	p.selected = (p.selected - 1 + len(p.items)) % len(p.items)
}

func (p *palette) down() {
	if len(p.items) == 0 {
		return
	}
	p.selected = (p.selected + 1) % len(p.items)
}

// current returns the highlighted command and whether one exists.
func (p *palette) current() (commands.Command, bool) {
	if !p.open || len(p.items) == 0 {
		return commands.Command{}, false
	}
	return p.items[p.selected], true
}

// view renders the panel. width bounds the panel; it shows at most maxRows
// items with the matched query characters highlighted.
func (p *palette) view(width int) string {
	if !p.open || len(p.items) == 0 {
		if p.open {
			body := styles.Chip("NO MATCH", styles.Faint) + " " +
				styles.Muted.Render("no commands match ") + styles.UserTag.Render("/"+p.query)
			return styles.Frame("Command Palette", body, width)
		}
		return ""
	}

	const maxRows = 7
	items := p.items
	start := 0
	if p.selected >= maxRows {
		start = p.selected - maxRows + 1
	}
	end := start + maxRows
	if end > len(items) {
		end = len(items)
	}

	// Column width for the command name so descriptions align.
	nameW := 0
	for _, c := range items {
		if w := len(c.Name) + 1; w > nameW {
			nameW = w
		}
	}

	var b strings.Builder
	query := "/"
	if p.query != "" {
		query += p.query
	}
	b.WriteString(styles.Chip("SEARCH", styles.Sky) + " " + styles.UserTag.Render(query) + styles.Subtle.Render("  type to filter") + "\n\n")
	for i := start; i < end; i++ {
		c := items[i]
		name := highlightMatch("/"+c.Name, p.query)
		pad := nameW + 1 - (len(c.Name) + 1)
		if pad < 1 {
			pad = 1
		}
		row := name + strings.Repeat(" ", pad) + styles.Muted.Render(c.Desc)
		if c.Hotkey != "" {
			row += "  " + styles.Chip(c.Hotkey, styles.Faint)
		}
		if i == p.selected {
			rowW := width - 6
			if rowW < 24 {
				rowW = 24
			}
			selected := " " + row + " "
			b.WriteString(styles.Selected.Width(rowW).Render(selected) + "\n")
		} else {
			b.WriteString(styles.Subtle.Render("   ") + row + "\n")
		}
	}
	footer := styles.HintBar(
		[2]string{"↑↓", "select"},
		[2]string{"tab", "autocomplete"},
		[2]string{"↵", "run"},
		[2]string{"esc", "close"},
	)
	return styles.Frame("Command Palette", strings.TrimRight(b.String(), "\n")+"\n"+footer, width)
}

// highlightMatch underlines the characters of name that match the query.
// name includes the leading slash; query does not.
func highlightMatch(name, query string) string {
	base := lipgloss.NewStyle().Foreground(styles.Cyan).Bold(true)
	if query == "" {
		return base.Render(name)
	}
	lowName := strings.ToLower(name)
	lowQ := strings.ToLower(query)
	idx := strings.Index(lowName, lowQ)
	if idx < 0 {
		return base.Render(name)
	}
	pre := name[:idx]
	mid := name[idx : idx+len(query)]
	post := name[idx+len(query):]
	return base.Render(pre) + styles.Match.Render(mid) + base.Render(post)
}
