package tui

import (
	"fmt"
	"strings"
	"time"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/commands"
	"qeuro/internal/session"
	"qeuro/internal/state"
	"qeuro/internal/styles"
	"qeuro/internal/tools"
)

const (
	historyScreenLimit      = 18
	historyContentMaxRunes  = 420
	historyToolMaxRunes     = 180
	historyToolSummaryLimit = 4
	// sessionListLimit bounds /sessions: each entry is parsed to count turns, so
	// an unbounded list would read every journal the directory keeps.
	sessionListLimit = 12
)

// welcomeScreen is printed once at startup and on /clear: the space-bunny
// mascot card (two columns — mascot + session info on the left, quick-start
// tips on the right) followed by a one-line command hint.
func welcomeScreen(version, activeModel string, width int) string {
	hints := styles.Muted.Render("/help — commands · /resume — restore last session · /model — pick a model")
	return "\n" + styles.LogoCard(version, activeModel, width) + "\n\n  " + hints + "\n"
}

// helpScreen lists every command with its hotkey.
func helpScreen(width int) string {
	var rows strings.Builder
	rows.WriteString(styles.Chip("COMMANDS", styles.Sky) + " " + styles.Muted.Render("slash commands available inside the session") + "\n\n")
	for _, c := range commands.All() {
		name := styles.UserTag.Render("/" + c.Name)
		pad := 12 - len(c.Name)
		if pad < 1 {
			pad = 1
		}
		rows.WriteString(styles.Subtle.Render("  ") + name + strings.Repeat(" ", pad) + styles.Muted.Render(c.Desc))
		if c.Hotkey != "" {
			rows.WriteString("  " + styles.Chip(c.Hotkey, styles.Faint))
		}
		rows.WriteString("\n")
	}
	keys := styles.HintBar(
		[2]string{"↑↓", "navigate"},
		[2]string{"tab", "autocomplete"},
		[2]string{"esc", "close panel"},
		[2]string{"ctrl+l", "clear"},
		[2]string{"ctrl+c", "cancel · again quits"},
	)
	body := strings.TrimRight(rows.String(), "\n") + "\n\n" + keys
	return "\n" + styles.Frame("Command Palette", body, styles.ContentWidth(width, 52, 96))
}

// contextScreen visualises context-window usage with a bar.
func contextScreen(app *state.App, width int) string {
	p := app.CtxPercent()
	barW := 38
	if styles.IsWide(width) {
		barW = 52
	} else if styles.IsNarrow(width) {
		barW = 24
	}
	col := styles.Green
	if p >= 85 {
		col = styles.Red
	} else if p >= 60 {
		col = styles.Amber
	}
	bar := styles.ProgressBar(p, barW, col)
	last := app.Usage.Last
	total := app.Usage.Total

	body := styles.Chip("WINDOW", col) + " " + bar + "  " + styles.Strong.Render(itoa(p)+"%") + "\n\n" +
		styles.FieldRow("context", styles.Muted.Render(formatInt(app.CtxUsed)+" / "+formatInt(app.CtxLimit)+" input tokens"), 58) + "\n" +
		styles.FieldRow("turn input", styles.Muted.Render(formatInt(last.InputTokens)+" total, "+formatInt(last.BillableInputTokens())+" billable"), 58) + "\n" +
		styles.FieldRow("turn cache", styles.Muted.Render(formatInt(last.CachedInputTokens)+" hit ("+itoa(cachePercent(last))+"%)"), 58) + "\n" +
		styles.FieldRow("turn output", styles.Muted.Render(formatInt(last.OutputTokens)+" tokens"), 58) + "\n" +
		styles.FieldRow("turn total", styles.Muted.Render(formatInt(last.TotalTokens())+" tokens"), 58) + "\n\n" +
		styles.Chip("SESSION", styles.Sky) + "\n" +
		styles.FieldRow("requests", styles.Muted.Render(formatInt(app.Usage.Requests)), 58) + "\n" +
		styles.FieldRow("turns", styles.Muted.Render(formatInt(app.MsgCount)), 58) + "\n" +
		styles.FieldRow("input", styles.Muted.Render(formatInt(total.InputTokens)+" total, "+formatInt(total.BillableInputTokens())+" billable"), 58) + "\n" +
		styles.FieldRow("cache", styles.Muted.Render(formatInt(total.CachedInputTokens)+" hit ("+itoa(cachePercent(total))+"%)"), 58) + "\n" +
		styles.FieldRow("output", styles.Muted.Render(formatInt(total.OutputTokens)+" tokens"), 58)
	return "\n" + styles.Frame("Context Window", body, styles.ContentWidth(width, 60, 82))
}

func usageScreen(app *state.App, width int) string {
	if app.Usage.Requests == 0 {
		body := styles.Chip("NO DATA", styles.Faint) + " " +
			styles.Muted.Render("Usage appears after the next streamed response.")
		return "\n" + styles.Frame("Usage", body, styles.ContentWidth(width, 48, 80))
	}

	last := app.Usage.Last
	total := app.Usage.Total
	body := styles.Chip("LAST TURN", styles.Amber) + "\n" +
		styles.FieldRow("tokens", styles.Muted.Render(formatInt(last.InputTokens)+" in / "+formatInt(last.OutputTokens)+" out / "+formatInt(last.CachedInputTokens)+" cache"), 62) + "\n" +
		styles.FieldRow("cost", styles.Muted.Render(formatUSD(last.CostUSD)), 62) + "\n" +
		styles.FieldRow("credits", styles.Muted.Render(fmtCredits(last.Credits)+" spent"), 62) + "\n\n" +
		styles.Chip("SESSION", styles.Sky) + "\n" +
		styles.FieldRow("requests", styles.Muted.Render(formatInt(app.Usage.Requests)), 62) + "\n" +
		styles.FieldRow("tokens", styles.Muted.Render(formatInt(total.InputTokens)+" in / "+formatInt(total.OutputTokens)+" out / "+formatInt(total.CachedInputTokens)+" cache"), 62) + "\n" +
		styles.FieldRow("cost", styles.Muted.Render(formatUSD(total.CostUSD)), 62) + "\n" +
		styles.FieldRow("credits", styles.Muted.Render(fmtCredits(total.Credits)+" spent"), 62)
	return "\n" + styles.Frame("Usage", body, styles.ContentWidth(width, 52, 86))
}

func cachePercent(u state.UsageRecord) int {
	if u.InputTokens <= 0 || u.CachedInputTokens <= 0 {
		return 0
	}
	p := u.CachedInputTokens * 100 / u.InputTokens
	if p > 100 {
		return 100
	}
	return p
}

func formatInt(n int) string {
	if n < 0 {
		return "-" + formatInt(-n)
	}
	s := itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	b.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		b.WriteString(",")
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func formatCompactInt(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return itoa(n/1_000) + "k"
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return itoa(n)
	}
}

func formatUSD(v float64) string {
	return fmt.Sprintf("$%.4f", v)
}

// doctorScreen reports a quick environment self-check.
func doctorScreen(version string, loggedIn, local bool, width int) string {
	ok := styles.OK.Render("●")
	warn := styles.Warn.Render("●")
	backend := warn + " " + styles.Base.Render("cloud session   ") + styles.Muted.Render("offline, run qeuro login")
	if local {
		backend = ok + " " + styles.Base.Render("local session   ") + styles.Muted.Render("backend disabled")
	} else if loggedIn {
		backend = ok + " " + styles.Base.Render("cloud session   ") + styles.Muted.Render("token saved")
	}
	body := styles.Chip("SYSTEM", styles.Sky) + " " + styles.Muted.Render("local environment check") + "\n\n" +
		ok + " " + styles.Base.Render("qeuro cli       ") + styles.Muted.Render(version) + "\n" +
		ok + " " + styles.Base.Render("terminal ansi   ") + styles.Muted.Render("enabled") + "\n" +
		ok + " " + styles.Base.Render("tui engine      ") + styles.Muted.Render("bubbletea") + "\n" +
		ok + " " + styles.Base.Render("workspace tools ") + styles.Muted.Render("read/edit/run") + "\n" +
		backend
	return "\n" + styles.Frame("Doctor", body, styles.ContentWidth(width, 52, 76))
}

// stubScreen is a placeholder for commands not yet implemented.
func stubScreen(name string) string {
	body := styles.Muted.Render("Command ") + styles.UserTag.Render("/"+name) +
		styles.Muted.Render(" is still in development.")
	return "\n" + styles.Frame("Module · "+name, body, 52)
}

// historyScreen shows the visible conversation from the current CLI session.
// Internal guardrail prompts are hidden, and tool payloads are compacted so the
// command stays readable even after long agentic turns.
func historyScreen(history []client.Message, width int) string {
	frameW := styles.ContentWidth(width, 44, 92)
	if frameW < 62 {
		frameW = 62
	}
	if frameW > 96 {
		frameW = 96
	}

	entries := visibleHistory(history)
	if len(entries) == 0 {
		body := styles.Chip("EMPTY", styles.Faint) + " " +
			styles.Muted.Render("No visible conversation history in this session yet.")
		return "\n" + styles.Frame("Session History", body, frameW)
	}

	start := 0
	if len(entries) > historyScreenLimit {
		start = len(entries) - historyScreenLimit
	}

	visible := entries[start:]
	var rows strings.Builder
	rows.WriteString(styles.Chip("SESSION", styles.Sky) + " " +
		styles.Muted.Render("showing "+itoa(len(visible))+" of "+itoa(len(entries))+" visible entries") + "\n\n")
	for i, msg := range visible {
		rows.WriteString(historyRow(start+i+1, msg))
		if i < len(visible)-1 {
			rows.WriteString("\n")
		}
	}
	if start > 0 {
		rows.WriteString("\n\n" + styles.Subtle.Render(itoa(start)+" older entries hidden; use terminal scrollback for full output."))
	}

	return "\n" + styles.Frame("Session History", rows.String(), frameW)
}

func visibleHistory(history []client.Message) []client.Message {
	out := make([]client.Message, 0, len(history))
	for _, msg := range history {
		if historyMessageVisible(msg) {
			out = append(out, msg)
		}
	}
	return out
}

func historyMessageVisible(msg client.Message) bool {
	content := strings.TrimSpace(msg.Content)
	switch msg.Role {
	case "system":
		return false
	case "user":
		return content != "" && !strings.HasPrefix(content, "QUALITY GATE:")
	case "assistant":
		return content != "" || len(msg.ToolCalls) > 0
	case "tool":
		return msg.Name != "" || content != ""
	default:
		return content != "" || len(msg.ToolCalls) > 0
	}
}

func historyRow(index int, msg client.Message) string {
	label := historyRoleLabel(msg)
	content := historyMessageSummary(msg)
	return styles.Subtle.Render("  "+itoa(index)+". ") + label + " " + styles.Base.Render(content)
}

func historyRoleLabel(msg client.Message) string {
	switch msg.Role {
	case "user":
		return styles.UserTag.Render("USER")
	case "assistant":
		return styles.Accent.Render("QEURO")
	case "tool":
		return styles.Muted.Render("TOOL")
	default:
		return styles.Muted.Render(strings.ToUpper(msg.Role))
	}
}

func historyMessageSummary(msg client.Message) string {
	content := compactHistoryContent(msg.Content, historyContentMaxRunes)
	switch msg.Role {
	case "assistant":
		if content == "" && len(msg.ToolCalls) > 0 {
			return "requested tools: " + toolCallsSummary(msg.ToolCalls)
		}
		if len(msg.ToolCalls) > 0 {
			return content + " | requested tools: " + toolCallsSummary(msg.ToolCalls)
		}
		return content
	case "tool":
		name := strings.TrimSpace(msg.Name)
		if name == "" {
			name = "tool"
		}
		result := compactHistoryContent(msg.Content, historyToolMaxRunes)
		if result == "" {
			return name + " result"
		}
		return name + " result: " + result
	default:
		if content != "" {
			return content
		}
		if len(msg.ToolCalls) > 0 {
			return "requested tools: " + toolCallsSummary(msg.ToolCalls)
		}
		return "(empty)"
	}
}

func compactHistoryContent(content string, maxRunes int) string {
	content = strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n"))
	if content == "" {
		return ""
	}
	content = strings.Join(strings.Fields(content), " ")
	runes := []rune(content)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return content
}

func toolCallsSummary(calls []client.ToolCall) string {
	if len(calls) == 0 {
		return "none"
	}
	n := len(calls)
	if n > historyToolSummaryLimit {
		n = historyToolSummaryLimit
	}
	names := make([]string, 0, n+1)
	for i := 0; i < n; i++ {
		name := strings.TrimSpace(calls[i].Function.Name)
		if name == "" {
			name = "tool"
		}
		names = append(names, name)
	}
	if extra := len(calls) - n; extra > 0 {
		names = append(names, "+"+itoa(extra)+" more")
	}
	return strings.Join(names, ", ")
}

// memoryScreen shows what the agent has recorded in local project memory
// (.infinity/). It reads the curated digest so the user can see — and trust —
// what the agent "knows" about the project.
func memoryScreen(runner *tools.Runner, width int) string {
	if runner == nil || runner.Memory() == nil {
		return "\n" + styles.Frame("Project Memory", styles.Muted.Render("unavailable"), styles.ContentWidth(width, 44, 84))
	}
	mem := runner.Memory()
	if !mem.HasContent() {
		body := styles.Chip("EMPTY", styles.Faint) + " " +
			styles.Muted.Render("The agent records important project facts here ") +
			styles.Subtle.Render("(stack, architecture, frontend, backend, conventions, changes)") +
			styles.Muted.Render(" as it works — and remembers them in future sessions.")
		return "\n" + styles.Frame("Project Memory · .infinity/", body, styles.ContentWidth(width, 60, 84))
	}
	body := mem.Digest()
	cats := strings.Join(mem.List(), styles.Subtle.Render(" · "))
	footer := styles.Subtle.Render("sections  ") + styles.Muted.Render(cats)
	return "\n" + styles.Frame("Project Memory · .infinity/", body+"\n\n"+footer, styles.ContentWidth(width, 64, 92))
}

// sessionsScreen lists the durable session journals (roadmap §8, row "Сессии").
//
// It exists because `/resume` with no id has to guess, and an id is not something
// a user memorises. Turn count and "did it exit cleanly" are what make one entry
// distinguishable from another, and neither is in the filename — so each journal
// is parsed rather than merely listed.
func sessionsScreen(currentID string, j *session.Journal, width int) string {
	frameW := styles.ContentWidth(width, 60, 92)
	sessions := session.List(sessionListLimit)

	var b strings.Builder
	b.WriteString(styles.Chip("THIS SESSION", styles.Sky) + " " + styles.Strong.Render(currentID))
	if p := j.Path(); p != "" {
		b.WriteString("\n" + styles.Subtle.Render(p))
	} else {
		b.WriteString("\n" + styles.Muted.Render("not journalled: no config directory available"))
	}
	if warn := j.Err(); warn != "" {
		b.WriteString("\n" + styles.Warn.Render(clientcfg.DisplaySafe(warn)))
	}
	b.WriteString("\n\n")

	others := 0
	for _, s := range sessions {
		if s.ID == currentID {
			continue
		}
		others++
		turns := len(s.Turns())
		row := styles.UserTag.Render(s.ID) + styles.Subtle.Render("  "+itoa(turns)+" turns")
		if age := session.Age(s, time.Now()); age != "" {
			row += styles.Subtle.Render(" · " + age)
		}
		if s.Crashed {
			row += " " + styles.Chip("CRASHED", styles.Amber)
		}
		b.WriteString("  " + row + "\n")
	}
	if others == 0 {
		b.WriteString("  " + styles.Muted.Render("no earlier sessions recorded yet") + "\n")
	}
	b.WriteString("\n" + styles.Subtle.Render("restore  ") + styles.UserTag.Render("/resume [id]") +
		styles.Subtle.Render("  ·  from the shell  ") + styles.UserTag.Render("qeuro resume [id]"))

	return "\n" + styles.Frame("Sessions", b.String(), frameW)
}

// loginScreen explains how to link this CLI with a web console account. It is
// shown by /login when no token argument is given.
func loginScreen(consoleURL string, loggedIn bool, width int) string {
	status := styles.Warn.Render("●") + " " + styles.Base.Render("status ") + styles.Muted.Render("signed out")
	if loggedIn {
		status = styles.OK.Render("●") + " " + styles.Base.Render("status ") + styles.Muted.Render("signed in — /login <token> switches accounts")
	}
	body := styles.Chip("ACCOUNT", styles.Sky) + " " + styles.Muted.Render("link this CLI to your web console account") + "\n\n" +
		status + "\n\n" +
		styles.Base.Render("1. ") + styles.Muted.Render("open ") + styles.UserTag.Render(consoleURL+"/settings") + "\n" +
		styles.Base.Render("2. ") + styles.Muted.Render("generate or copy your CLI token (qeuro_live_…)") + "\n" +
		styles.Base.Render("3. ") + styles.Muted.Render("run ") + styles.UserTag.Render("/login <token>") + "\n\n" +
		styles.Subtle.Render("the token is stored securely · /logout removes it")
	return "\n" + styles.Frame("Sign in", body, styles.ContentWidth(width, 52, 72))
}

// providersScreen lists the AI provider credentials linked on the web
// console. The exact same records power the console's Providers page: both
// surfaces read one list, and enabled providers ride along with every chat
// request (M7).
func providersScreen(providers []client.ProviderConfig, consoleURL string, width int) string {
	if len(providers) == 0 {
		body := styles.Chip("PROVIDERS", styles.Sky) + " " + styles.Muted.Render("linked with the web console") + "\n\n" +
			styles.Muted.Render("no providers yet — add one at ") + styles.UserTag.Render(consoleURL+"/providers") + "\n" +
			styles.Subtle.Render("providers added there appear here automatically after /providers or /login")
		return "\n" + styles.Frame("AI Providers", body, styles.ContentWidth(width, 56, 80))
	}
	var b strings.Builder
	b.WriteString(styles.Chip("PROVIDERS", styles.Sky) + " " + styles.Muted.Render("linked with the web console — attached to chat requests") + "\n\n")
	for _, p := range providers {
		dot := styles.OK.Render("●")
		if !p.Enabled {
			dot = styles.Subtle.Render("●")
		}
		name := p.Name
		if name == "" {
			name = p.Provider
		}
		meta := p.Provider
		if p.Kind != "" {
			meta += " · " + p.Kind
		}
		meta += " · " + itoa(len(p.Models)) + " models"
		if !p.Enabled {
			meta += " · disabled"
		}
		b.WriteString(dot + " " + styles.Base.Render(name) + "  " + styles.Muted.Render(meta) + "\n")
		for _, mdl := range p.Models {
			label := mdl.ID
			if mdl.Label != "" {
				label = mdl.Label + "  " + mdl.ID
			}
			b.WriteString(styles.Subtle.Render("    · ") + styles.Muted.Render(label) + "\n")
		}
	}
	b.WriteString("\n" + styles.Subtle.Render("manage at ") + styles.UserTag.Render(consoleURL+"/providers"))
	return "\n" + styles.Frame("AI Providers", strings.TrimRight(b.String(), "\n"), styles.ContentWidth(width, 56, 84))
}
