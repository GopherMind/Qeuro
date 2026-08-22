package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/catalog"
	"qeuro/internal/client"
	"qeuro/internal/state"
)

// ansiResetTerminal wipes both the visible screen and the terminal's saved
// scrollback lines (ESC[3J): bubbletea's tea.ClearScreen only repaints the
// visible area, so scrolling up would still show the old transcript. Emitted
// through tea.Println so the renderer writes it in order with other output.
const ansiResetTerminal = "\x1b[2J\x1b[3J\x1b[H"

// runCommand executes a slash command by name.
func (m model) runCommand(name string, args ...string) (tea.Model, tea.Cmd) {
	m.pal.close()
	m.infoView = "" // a new command replaces any open info panel
	switch name {
	case "exit":
		// Same exit as a second Ctrl+C, including the in-flight cancel: /exit typed
		// mid-stream used to leave the turn's goroutine to notice the process was
		// gone on its own (H2).
		return m.quitNow()
	case "clear":
		m.notice = "session cleared"
		m.app.CtxUsed = 0
		m.app.MsgCount = 0
		m.app.Usage.Reset()
		m.lastUsage = nil
		m.history = nil
		m.pastes = nil
		// The guard directive lives in the history, so clearing the history removes
		// it. Without this the next conversation's first fenced MCP payload arrives
		// with no directive telling the model the block is data.
		m.untrustedBlocks = nil
		m.mcpGuardSent = false
		return m, tea.Sequence(
			tea.Println(ansiResetTerminal),
			tea.ClearScreen,
			tea.Println(welcomeScreen(m.version, m.app.Model.Label, m.width)),
		)
	case "help":
		m.infoView = helpScreen(m.width)
		return m, nil
	case "team":
		return m.toggleTeam()
	case "chat":
		m.app.Mode = state.ModeChat
		if m.local {
			m.notice = "mode: chat · " + m.statusModelName()
		} else {
			m.notice = "mode: chat · " + m.app.Model.Label
		}
		return m, nil
	case "model", "models":
		if m.local {
			m.notice = "local model is chosen by --local-model or QEURO_LOCAL_MODEL; restart to change it"
			return m, nil
		}
		m.sel.openWith(selBrand, "Model Router · provider", brandItems(), m.app.Model.ID, false)
		return m, nil
	case "effort":
		m.sel.openWith(selEffort, "Reasoning Effort · "+m.app.Model.Label,
			effortItems(m.app.Model), string(m.app.Effort), false)
		return m, nil
	case "settings":
		m.sel.openWith(selSettings, "Router Settings", settingsItems(), m.app.Mode.String(), false)
		return m, nil
	case "mode":
		m.sel.openWith(selOutput, "Output Mode", outputItems(), m.app.Output.String(), false)
		return m, nil
	case "approvals":
		m.sel.openWith(selApprovals, "Agent Approvals", approvalItems(), m.app.Approvals.String(), false)
		return m, nil
	case "undo":
		if m.runner == nil {
			m.notice = "undo is unavailable"
			return m, nil
		}
		msg, ok := m.runner.Undo()
		m.notice = msg
		_ = ok
		return m, nil
	case "context":
		if m.local && m.app.Usage.Requests == 0 {
			m.notice = "local server does not report context/token usage"
			return m, nil
		}
		m.infoView = contextScreen(m.app, m.width)
		return m, nil
	case "usage":
		if m.local && m.app.Usage.Requests == 0 {
			m.notice = "local server does not report token or credit usage"
			return m, nil
		}
		m.infoView = usageScreen(m.app, m.width)
		return m, nil
	case "doctor":
		m.infoView = doctorScreen(m.version, m.loggedIn, m.local, m.width)
		return m, nil
	case "memory":
		m.infoView = memoryScreen(m.runner, m.width)
		return m, nil
	case "resume":
		id := ""
		if len(args) > 0 {
			id = strings.TrimSpace(args[0])
		}
		return m.resumeSession(id)
	case "sessions":
		m.infoView = sessionsScreen(m.sessionID, m.journal, m.width)
		return m, nil
	case "login":
		// A session started with --local promised that nothing leaves this machine.
		// Verifying a token is a backend call, so honouring /login here would break
		// that promise from inside the session the user opened to avoid it — and it
		// would send a bearer token to a network the user declared closed.
		if m.local {
			m.notice = "local session — /login is disabled; restart without --local to sign in"
			return m, nil
		}
		token := ""
		if len(args) > 0 {
			token = strings.TrimSpace(args[0])
		}
		if token == "" {
			m.infoView = loginScreen(m.consoleURL, m.loggedIn, m.width)
			return m, nil
		}
		m.notice = "verifying token…"
		return m, loginCmd(m.baseURL, token)
	case "logout":
		if !m.loggedIn {
			m.notice = "not signed in — run /login <token>"
			return m, nil
		}
		prev := m.cli
		m.cli = client.New(m.baseURL, "")
		m.loggedIn = false
		m.providers = nil
		m.app.Conn = state.Offline
		m.notice = "signing out…"
		return m, logoutCmd(prev)
	case "providers":
		if !m.loggedIn {
			m.notice = "sign in first: /login <token>"
			return m, nil
		}
		m.notice = "syncing providers with the web console…"
		return m, providersCmd(m.cli, m.consoleURL, false)
	case "update":
		m.infoView = stubScreen(name)
		return m, nil
	default:
		m.notice = "unknown command: /" + name
		return m, nil
	}
}

// onSelectorChoose applies the chosen item based on the selector kind.
// Brand selection drills into that brand's models rather than closing.
func (m model) onSelectorChoose(it selectorItem) (tea.Model, tea.Cmd) {
	switch m.sel.kind {
	case selBrand:
		for _, b := range catalog.Current() {
			if b.Key == it.value {
				m.sel.openWith(selModel, "Model Router · "+b.Name,
					modelItems(b), m.app.Model.ID, true)
				break
			}
		}
		return m, nil

	case selModel:
		if mdl, _, ok := catalog.FindModel(it.value); ok {
			m.app.SetModel(mdl)
			m.app.Mode = state.ModeChat
			m.notice = "model: " + mdl.Label + " · effort " + string(m.app.Effort)
		}
		m.sel.close()
		return m, nil

	case selEffort:
		m.app.Effort = catalog.Effort(it.value)
		m.notice = "reasoning effort: " + it.value
		m.sel.close()
		return m, nil

	case selSettings:
		if it.value == "auto" {
			m.app.Mode = state.ModeAuto
		} else {
			m.app.Mode = state.ModeChat
		}
		m.notice = "router mode: " + it.value
		m.sel.close()
		return m, nil

	case selOutput:
		switch it.value {
		case "full":
			m.app.Output = state.OutputFull
		case "caveman":
			m.app.Output = state.OutputCaveman
		default:
			m.app.Output = state.OutputConcise
		}
		m.notice = "output mode: " + it.value
		m.sel.close()
		return m, nil

	case selApprovals:
		switch it.value {
		case "edits":
			m.app.Approvals = state.ApprovalEdits
		case "all":
			m.app.Approvals = state.ApprovalAll
		default:
			m.app.Approvals = state.ApprovalAsk
		}
		m.notice = "approvals: " + it.value
		m.sel.close()
		return m, nil
	}
	m.sel.close()
	return m, nil
}

// brandItems lists provider brands for the first selector level.
//
// catalog.Current rather than catalog.Brands: the selector shows what the backend
// last told us it serves, falling back to the compiled-in list when nothing is
// cached (§8 "Startup").
func brandItems() []selectorItem {
	current := catalog.Current()
	items := make([]selectorItem, 0, len(current))
	for _, b := range current {
		items = append(items, selectorItem{
			label: b.Name,
			note:  itoa(len(b.Models)) + " models",
			value: b.Key,
		})
	}
	return items
}

// modelItems lists the models within one brand.
func modelItems(b catalog.Brand) []selectorItem {
	items := make([]selectorItem, 0, len(b.Models))
	for _, mdl := range b.Models {
		items = append(items, selectorItem{
			label: mdl.Label,
			note:  mdl.Note,
			value: mdl.ID,
		})
	}
	return items
}

// effortItems lists the effort levels a given model supports.
func effortItems(mdl catalog.Model) []selectorItem {
	items := make([]selectorItem, 0, len(mdl.Efforts))
	for _, e := range mdl.Efforts {
		items = append(items, selectorItem{
			label: string(e),
			note:  catalog.EffortNote[e],
			value: string(e),
		})
	}
	return items
}

// settingsItems offers the routing modes.
func settingsItems() []selectorItem {
	return []selectorItem{
		{label: "auto", note: "auto-router picks the model", value: "auto"},
		{label: "chat", note: "one pinned model", value: "chat"},
	}
}

// approvalItems offers the auto-approval levels.
func approvalItems() []selectorItem {
	return []selectorItem{
		{label: "ask", note: "confirm every edit and command", value: "ask"},
		{label: "edits", note: "file edits auto, commands ask", value: "edits"},
		{label: "all", note: "edits and write_file auto; shell commands ask", value: "all"},
	}
}

// outputItems offers the output-verbosity modes (token economy).
func outputItems() []selectorItem {
	return []selectorItem{
		{label: "concise", note: "no fluff — saves tokens (default)", value: "concise"},
		{label: "full", note: "normal detailed answers", value: "full"},
		{label: "caveman", note: "code/diff only, minimal words", value: "caveman"},
	}
}
