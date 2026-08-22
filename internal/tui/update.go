package tui

import (
	"context"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/agentloop"
	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/session"
	"qeuro/internal/state"
	"qeuro/internal/styles"
	"qeuro/internal/tools"
)

// pasteLabelRe matches the inline placeholder shown for a multi-line paste,
// e.g. "[paste 42 lines]". On submit each is expanded back to its content.
var pasteLabelRe = regexp.MustCompile(`\[paste \d+ lines\]`)

// maxToolSteps bounds the agentic loop so a misbehaving model cannot call tools
// forever within a single user turn.
const (
	maxToolSteps       = 64
	toolLimitWarnSteps = 8
)

// pasteEnterWindow is the maximum gap between the previous buffered character
// and an Enter for that Enter to be treated as a paste-embedded newline rather
// than a submit. Comfortably above console paste bursts (~sub-millisecond per
// char) yet far below human typing cadence.
const pasteEnterWindow = 40 * time.Millisecond

// maxInputRows caps how tall the input box grows before it scrolls internally.
const maxInputRows = 10

// pasteFlushDelay is how long after the last buffered key to commit the pending
// input. During a paste, keys arrive far faster than this and keep resetting
// the timer, so the whole block commits at once when the burst ends; for normal
// typing each key commits ~immediately after it. Small enough to feel instant.
const pasteFlushDelay = 20 * time.Millisecond

// streamStartMsg is delivered once the backend connection is established (or
// failed) for a chat request.
type streamStartMsg struct {
	ch  <-chan client.Event
	err error
}

// meMsg carries the account info fetched at startup (credits balance).
type meMsg struct {
	me  *client.MeResponse
	err error
}

// streamEventMsg carries one SSE event; ok=false means the stream closed.
type streamEventMsg struct {
	ev client.Event
	ok bool
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Input spans the framed prompt row: outer gutter, left rail, prompt,
		// and right edge are kept outside the textarea width.
		m.input.SetWidth(m.inputWrapWidth())
		return m, nil

	case streamStartMsg:
		return m.onStreamStart(msg)

	case agentEventMsg:
		return m.onAgentEvent(msg)

	case agentDoneMsg:
		return m.onAgentDone(msg)

	case meMsg:
		if msg.err == nil && msg.me != nil {
			m.credits = msg.me.CreditsBalance
			m.creditsKnown = true
			m.tier = msg.me.Tier
		}
		return m, nil

	case teamEventMsg:
		return m.onTeamEvent(msg)

	case loginDoneMsg:
		if msg.err != nil {
			m.notice = "login failed: " + msg.err.Error()
			return m, nil
		}
		m.cli = client.New(m.baseURL, msg.token)
		m.loggedIn = true
		m.app.Conn = state.Online
		m.infoView = ""
		m.notice = "signed in"
		if msg.me != nil && msg.me.Tier != "" {
			m.notice = "signed in · plan " + msg.me.Tier
		}
		return m, tea.Batch(fetchMe(m.cli), fetchProviders(m.cli, m.consoleURL))

	case logoutDoneMsg:
		m.notice = "signed out — token removed"
		if msg.saveErr != nil {
			m.notice = "signed out, but saving config failed: " + msg.saveErr.Error()
		} else if msg.revokeErr != nil {
			m.notice = "signed out locally; server revoke failed: " + msg.revokeErr.Error()
		}
		return m, nil

	case catalogMsg:
		// A quiet refresh reports nothing at all: it succeeded silently, or the
		// compiled-in catalogue is still in use, and neither is news while the user
		// is typing. Only an explicit refresh speaks.
		if msg.quiet {
			return m, nil
		}
		if msg.err != nil {
			// The message can be the backend's own error text, so it is remote input on
			// its way into a terminal — same treatment as the mcp and resume notices.
			m.notice = "model catalogue refresh failed: " + clientcfg.DisplaySafe(msg.err.Error())
		} else if msg.changed {
			m.notice = "model catalogue updated"
		} else {
			m.notice = "model catalogue is up to date"
		}
		return m, nil

	case providersMsg:
		if msg.err != nil {
			if !msg.quiet {
				m.notice = "provider sync failed: " + msg.err.Error()
			}
			return m, nil
		}
		m.providers = msg.providers
		if !msg.quiet {
			m.notice = ""
			m.infoView = providersScreen(m.providers, m.consoleURL, m.width)
		}
		return m, nil

	case streamEventMsg:
		return m.onStreamEvent(msg)

	case toolDoneMsg:
		return m.onToolDone(msg)

	case mcpReadyMsg:
		return m.onMCPReady(msg)

	case pasteFlushMsg:
		return m.onPasteFlush(msg.gen)

	case tea.MouseMsg:
		// Captured only to stop the terminal's own right-click paste; ignored.
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)
	}

	return m.updateSubmodels(msg)
}

// onKey routes key presses, giving the slash palette priority when open.
func (m model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Checked before every overlay and prompt handler below, so "stop" and "leave"
	// are reachable from any state the UI can get into — including one where an
	// overlay is misbehaving and swallowing keys.
	switch msg.String() {
	case "ctrl+c":
		return m.onInterruptKey()
	case "ctrl+l":
		return m.runCommand("clear")
	}

	// Any key other than Ctrl+C disarms the pending exit. Otherwise a Ctrl+C
	// pressed a minute ago, on a turn since finished, would still turn the next
	// one into an immediate quit — the second press has to be a second press,
	// not an eventual one.
	m.quitArmed = false

	// A transient info panel (/help, /context, /usage…) is dismissed by any
	// key: esc/enter simply close it, any other key closes it and falls
	// through, so typing the next message continues uninterrupted.
	if m.infoView != "" {
		m.infoView = ""
		switch msg.String() {
		case "esc", "enter":
			return m, nil
		}
	}

	// Esc interrupts an in-flight turn (solo stream or team run) without quitting
	// — the in-session abort the review called for (H2). It is checked before the
	// overlay/approval handlers below so it can't be swallowed mid-stream; when a
	// team run is paused awaiting input there is no approval/overlay open anyway.
	if msg.String() == "esc" && (m.streaming || m.awaitingTeamInput) && !m.awaitingApproval && !m.sel.open && !m.pal.open {
		return m.interruptTurn()
	}

	// A pending file edit/command captures keys until decided. Three choices,
	// chosen with arrows + Enter, a number key, or the y/n shortcuts.
	if m.awaitingApproval {
		switch msg.String() {
		case "up", "ctrl+p":
			if m.approvalChoice > 0 {
				m.approvalChoice--
			}
			return m, nil
		case "down", "ctrl+n":
			if m.approvalChoice < 2 {
				m.approvalChoice++
			}
			return m, nil
		case "1":
			return m.applyApprovalChoice(0)
		case "2":
			return m.applyApprovalChoice(1)
		case "3":
			return m.applyApprovalChoice(2)
		case "y", "Y":
			return m.applyApprovalChoice(0)
		case "n", "N", "esc":
			return m.applyApprovalChoice(2)
		case "enter":
			return m.applyApprovalChoice(m.approvalChoice)
		}
		return m, nil // ignore other keys while deciding
	}

	// Paste detection. Bracketed paste sets msg.Paste; some terminals instead
	// deliver a paste as one key event carrying many runes (possibly with
	// embedded newlines). Both cases: treat the whole payload as a paste so a
	// large block becomes a compact label instead of cramming the input.
	if msg.Paste || len(msg.Runes) > 1 {
		m.commitPending()
		return m.onPaste(string(msg.Runes))
	}

	// A selector overlay captures navigation keys before anything else.
	if m.sel.open {
		switch msg.String() {
		case "esc":
			if m.sel.canBack && m.sel.kind == selModel {
				// Drill back up from the model list to the brand list.
				m.sel.openWith(selBrand, "Pick a model · brand", brandItems(), m.app.Model.ID, false)
				return m, nil
			}
			m.sel.close()
			// Wipe the selector frame so it does not linger in scrollback.
			return m, eraseOverlayCmd("")
		case "up", "ctrl+p":
			m.sel.up()
			return m, nil
		case "down", "ctrl+n":
			m.sel.down()
			return m, nil
		case "enter":
			if it, ok := m.sel.current(); ok {
				next, cmd := m.onSelectorChoose(it)
				if nm, isModel := next.(model); isModel && !nm.sel.open {
					// The selector is gone: erase its frame and leave a single
					// ✓ log line instead (Claude Code style).
					status := ""
					if nm.notice != "" {
						status = "  " + styles.OK.Render("✓ ") + styles.Muted.Render(nm.notice)
					}
					return next, seqCmds(eraseOverlayCmd(status), cmd)
				}
				return next, cmd
			}
			return m, nil
		}
		// Ignore other keys while the overlay is open.
		return m, nil
	}

	if m.pal.open {
		switch msg.String() {
		case "esc":
			m.pal.close()
			// Wipe the palette frame so it does not linger in scrollback.
			return m, eraseOverlayCmd("")
		case "up", "ctrl+p":
			m.pal.up()
			return m, nil
		case "down", "ctrl+n":
			m.pal.down()
			return m, nil
		case "tab":
			if c, ok := m.pal.current(); ok {
				m.input.SetValue("/" + c.Name)
				m.input.CursorEnd()
				m.pal.sync(m.input.Value())
			}
			return m, nil
		case "enter":
			if c, ok := m.pal.current(); ok {
				m.input.SetValue("")
				m.pal.close()
				next, cmd := m.runCommand(c.Name)
				// Erase the palette frame before the command's own output.
				return next, seqCmds(eraseOverlayCmd(""), cmd)
			}
		}
	}

	// A lone newline rune (some console paths deliver '\n' as a single-rune key)
	// is always a newline insert, never a submit — buffer it.
	if len(msg.Runes) == 1 && msg.Runes[0] == '\n' {
		return m.bufferInput("\n")
	}

	if msg.String() == "enter" {
		// Distinguish a human pressing Enter to submit from a newline that is
		// part of a fast paste. During a paste the buffer is still filling
		// (pendingInput non-empty) or the last char landed microseconds ago;
		// a deliberate submit comes after the buffer has settled and committed.
		if m.pendingInput != "" || (!m.lastRuneAt.IsZero() && time.Since(m.lastRuneAt) < pasteEnterWindow) {
			return m.bufferInput("\n")
		}
		m.commitPending()
		return m.onSubmit()
	}

	// Printable input: accumulate it in the paste buffer (committed on settle).
	if len(msg.Runes) > 0 {
		return m.bufferInput(string(msg.Runes))
	}

	// Any other key (backspace, arrows, ctrl-…): commit pending input first so
	// edits apply to the real text, then let the textarea handle the key.
	m.commitPending()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.syncInputHeight()
	m.pal.sync(m.input.Value())
	return m, cmd
}

// inputWrapWidth is the column at which the textarea soft-wraps text. It must
// match the width handed to input.SetWidth, or the row count below drifts from
// what is actually drawn.
func (m model) inputWrapWidth() int {
	w := m.width - 8
	if w < 20 {
		w = 20
	}
	return w
}

// pasteFlushMsg fires a short time after the last buffered key; if no newer key
// arrived (gen still current), the burst has settled and is committed.
type pasteFlushMsg struct{ gen int }

func schedulePasteFlush(gen int) tea.Cmd {
	return tea.Tick(pasteFlushDelay, func(time.Time) tea.Msg {
		return pasteFlushMsg{gen: gen}
	})
}

// expandPastes replaces each "[paste N lines]" label with its stored content,
// in order of appearance (labels are inserted in paste order, so the i-th label
// maps to the i-th stored paste).
func (m model) expandPastes(text string) string {
	if len(m.pastes) == 0 {
		return text
	}
	i := 0
	return pasteLabelRe.ReplaceAllStringFunc(text, func(match string) string {
		if i < len(m.pastes) {
			v := m.pastes[i]
			i++
			return v
		}
		return match
	})
}

// onSubmit handles Enter when the palette is closed.
func (m model) onSubmit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}

	if strings.HasPrefix(text, "/") {
		m.input.SetValue("")
		m.input.SetHeight(1)
		m.notice = ""
		fields := strings.Fields(text)
		name := strings.TrimPrefix(fields[0], "/")
		return m.runCommand(name, fields[1:]...)
	}

	// A team run paused to ask the user something: route this message back to
	// the waiting engine instead of starting a new turn.
	if m.awaitingTeamInput {
		m.turnStarted = time.Now()
		return m.answerTeam(text)
	}

	// Block new input while a reply is still streaming.
	if m.streaming {
		m.notice = "wait for the reply to finish…"
		return m, nil
	}
	// Offline mode is exempt: a local model needs no account, and there is no
	// balance to protect (roadmap §8 row "Offline").
	if !m.loggedIn && !m.local {
		m.notice = "not logged in — qeuro login <token>"
		return m, nil
	}
	// The ceiling also refuses to start a new turn, not just to continue one. The
	// input is left in the box: the user's text is not lost to a limit they can
	// raise and retry with.
	if m.budget.exhausted() {
		m.notice = m.budget.notice()
		return m, nil
	}

	m.input.SetValue("")
	m.input.SetHeight(1)
	m.notice = ""

	// The transcript shows the compact text (with [paste N lines] labels); the
	// model receives the labels expanded to their full pasted content.
	full := m.expandPastes(text)
	m.pastes = nil

	// Echo the user message into scrollback and add it to the conversation.
	m.turnStarted = time.Now()
	userBlock := styles.Message(styles.RoleUser, clock(m.turnStarted), "", text, m.width)
	m.turnStartIndex = len(m.history)
	m.turnHistoryStable = true
	m.history = append(m.history, client.Message{Role: "user", Content: full})
	m.logSession(session.KindUser, full)
	m.app.Phase = state.PhaseGenerating
	m.app.MsgCount++
	m.streaming = true
	m.streamText = ""
	m.streamMeta = ""
	m.streamErr = ""
	m.toolStep = 0
	m.toolWarned = false
	m.toolFinal = false
	m.workingState = agentloop.WorkingState{}
	m.turnMemoryDigest = m.projectMemoryDigest()
	m.verificationRequired = false
	m.verificationPassed = false
	m.verificationNote = ""
	m.app.Conn = state.Connecting

	// Team mode: run the multi-agent orchestration instead of a solo stream.
	if m.teamMode {
		ctx := m.beginTurn()
		m2, teamCmd := m.startTeamRun(ctx, full)
		header := strings.TrimRight(styles.Message(styles.RoleSystem, clock(m.turnStarted), "team · "+m2.profileName(),
			"assembling the agent team…", m.width), "\n")
		return m2, tea.Batch(
			tea.Println(strings.TrimRight(userBlock, "\n")),
			tea.Println(header),
			teamCmd,
		)
	}

	ctx := m.beginTurn()

	// Solo turn: use the shared agentcore.Engine via the host adapter.
	host, cmd := startAgentHost(ctx, m.provider, m.runner, full, m.app.Model.ID, m.budget.limit)
	m.agentHost = host
	return m, tea.Batch(
		tea.Println(strings.TrimRight(userBlock, "\n")),
		cmd,
	)
}

// agentSystemPrompt is a deliberately tight system prompt (token economy,
// plan §14 / §7): it steers tool use toward minimal diffs and concise output
// without a verbose preamble.
// Оба промпта живут в internal/agentloop и общие с headless-циклом: агент,
// который в облаке получает другую инструкцию, чем в терминале, — это другой
// агент под тем же именем.
const agentSystemPrompt = agentloop.SystemPrompt

const agentShellPrompt = agentloop.ShellPrompt

const soloMaxTokens = 4096

// buildRequest assembles the chat request from the current UI state, including
// the local tool definitions so the model can read and edit files. The
// conversation is trimmed before sending so stale file dumps are not
// retransmitted every tool step (token economy, plan §14).
func (m model) buildRequest() client.ChatRequest {
	msgs := make([]client.Message, 0, len(m.history)+2)
	// Keep the leading prompt prefix stable within a turn so provider prompt
	// caches can match continuation requests.
	// conversation, so we send a short continuation prompt and skip the digest —
	// re-injecting ~3.5 KB of memory every step was a dominant, needless cost.
	msgs = append(msgs, client.Message{Role: "system", Content: agentSystemPrompt})
	msgs = append(msgs, client.Message{Role: "system", Content: agentShellPrompt})
	if digest := m.cacheableMemoryDigest(); digest != "" {
		msgs = append(msgs, client.Message{
			Role:    "system",
			Content: "Project memory (.infinity/, accumulated over previous sessions):\n" + digest,
		})
	}
	msgs = append(msgs, m.cacheStableHistory()...)
	if state := m.workingStateMessage(); state != "" {
		msgs = append(msgs, client.Message{Role: "system", Content: state})
	}

	// The MCP tools currently registered are offered alongside the built-ins,
	// within a byte budget: definitions are re-sent on every step of a tool loop,
	// so a server with verbose descriptions is paid for many times in one turn.
	// With no server configured this is byte-identical to tools.Definitions().
	defs, _ := tools.WithMCP(tools.DefaultMCPDescriptionBudget)

	req := client.ChatRequest{
		ProjectID:  m.projectID,
		SessionID:  "qeuro-cli-" + m.projectID,
		Mode:       m.app.Mode.String(),
		OutputMode: m.app.Output.String(),
		Messages:   msgs,
		MaxTokens:  soloMaxTokens,
		Providers:  m.providers,
		Tools:      defs,
	}
	if m.toolFinal {
		req.Tools = nil
	}
	if m.app.Mode == state.ModeChat {
		req.Model = m.app.Model.ID
		req.Effort = string(m.app.Effort)
	}
	return req
}

func (m model) cacheStableHistory() []client.Message {
	if !m.turnHistoryStable || m.turnStartIndex < 0 || m.turnStartIndex >= len(m.history) {
		return trimmedHistory(m.history)
	}
	out := make([]client.Message, 0, len(m.history))
	out = append(out, trimmedHistory(m.history[:m.turnStartIndex])...)
	out = append(out, trimmedHistory(m.history[m.turnStartIndex:])...)
	return out
}

func (m model) projectMemoryDigest() string {
	if m.runner == nil {
		return ""
	}
	mem := m.runner.Memory()
	if mem == nil {
		return ""
	}
	return mem.Digest()
}

func (m model) cacheableMemoryDigest() string {
	if m.turnMemoryDigest != "" || m.toolStep > 0 {
		return m.turnMemoryDigest
	}
	return m.projectMemoryDigest()
}

// onInterruptKey implements the two meanings of Ctrl+C the roadmap row asks for
// (§8, "Прерывание"): the first press cancels the work in flight, the second
// leaves. Esc keeps cancelling too — it always did, and a user who learned it
// should not have to relearn.
//
// Quitting on the first press is what the row exists to stop. A turn is the
// expensive thing in this program: tokens were spent, a partial answer is on
// screen, and Ctrl+C is the reflex for "stop that" in every other terminal
// program. Making it mean "throw away the session" instead loses the answer the
// row is specifically about keeping.
//
// With nothing running, the first press has nothing to cancel, so it arms the
// exit and says so rather than doing nothing: a Ctrl+C that produces no visible
// response reads as a hung program and invites the SIGKILL reflex.
func (m model) onInterruptKey() (tea.Model, tea.Cmd) {
	if m.streaming || m.awaitingTeamInput || m.awaitingApproval {
		// Cancelling is itself the response, so the exit is not armed here: the
		// user asked to stop the work, not to leave, and arming would make the
		// natural follow-up (a second Ctrl+C because the first seemed slow) quit
		// the session that was just recovered.
		out, cmd := m.interruptTurn()
		im, ok := out.(model)
		if !ok {
			return out, cmd
		}
		im.notice = "turn cancelled · ctrl+c again to quit"
		im.quitArmed = true
		return im, cmd
	}

	if !m.quitArmed {
		m.quitArmed = true
		m.notice = "nothing to cancel · ctrl+c again to quit"
		return m, nil
	}

	return m.quitNow()
}

// quitNow ends the session, cancelling any in-flight work first so the worker
// goroutine (solo stream or team run) is not orphaned on exit (H2).
func (m model) quitNow() (tea.Model, tea.Cmd) {
	if m.turnCancel != nil {
		m.turnCancel()
	}
	// Same reason as in interruptTurn: an in-flight reply is journalled before
	// the process goes away, so quitting mid-stream loses the terminal output
	// but not the answer.
	m.journalPartial()
	m.quit = true
	return m, tea.Quit
}

// interruptTurn cancels the in-flight turn (solo stream, team run or pending
// approval) in response to Esc or the first Ctrl+C, returning the model to idle.
// The cancelled context unblocks the HTTP request / SSE parse so the background
// goroutine exits instead of being orphaned (H2).
func (m model) interruptTurn() (tea.Model, tea.Cmd) {
	m.interrupted = true
	// An open approval panel belongs to the turn being cancelled, so it goes with
	// it. Leaving it up would keep a dialog on screen that still captures keys and
	// whose "yes" runs a tool for a turn whose context is already cancelled — the
	// tool would execute and its result would have nowhere to go. Esc could not
	// reach this before (its guard excludes awaitingApproval); Ctrl+C can.
	approvalCmd := tea.Cmd(nil)
	if m.awaitingApproval {
		summary := ""
		if m.pendingTool != nil {
			summary = tools.Summary(m.pendingTool.Function.Name, m.pendingTool.Function.Arguments)
		}
		// The same erase the y/n path uses: the panel is wiped from the visible
		// frame and replaced by one line, so a multi-line dialog does not stay in
		// the scrollback looking like it is still waiting for an answer.
		approvalCmd = eraseOverlayCmd(approvalStatus(false, summary))
		m.awaitingApproval = false
		m.pendingTool = nil
		m.pendingPreview = ""
		m.approvalChoice = 0
	}
	// Solo agentcore host: signal cancel through the protocol channel so the
	// engine emits its own done{cancelled} and unwinds cleanly, rather than
	// being killed mid-tool by a bare context cancel.
	if m.agentHost != nil {
		m.agentHost.stop()
		m.agentHost = nil
	}
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	// Journal what was streamed before the cancel. The roadmap row pairs
	// interruption with "keep the partial answer": the text is on screen but
	// scrollback is not a record, and without this the next resume shows a user
	// turn with nothing after it.
	m.journalPartial()
	// And keep it in the conversation, marked for what it is. The tokens were
	// generated and billed, so dropping the text means the user paid for an
	// answer the next turn cannot refer to — "continue" would have nothing to
	// continue from. The label matters as much as the text: an unmarked fragment
	// reads to the model as a complete answer it gave, which teaches it that
	// stopping mid-sentence is an acceptable way to finish.
	m.keepPartialInHistory()
	m.streaming = false
	m.awaitingTeamInput = false
	m.turnStartIndex = -1
	m.turnHistoryStable = false
	m.streamCh = nil
	m.teamCh = nil
	m.pendingCalls = nil
	m.toolQueue = nil
	// Same reason as in finalizeToolLimit: an interrupted step's fenced payloads
	// have no tool result left to accompany them, and carrying them into the next
	// turn would present server output the model never asked for.
	m.untrustedBlocks = nil
	m.app.Phase = state.PhaseIdle
	m.app.Conn = state.Online
	m.streamText, m.streamMeta, m.streamErr = "", "", ""
	block := styles.Message(styles.RoleSystem, clock(m.turnStarted), "", "interrupted by user", m.width)
	// Sequenced, not batched: the erase writes relative to the current frame, so it
	// has to land before the interrupt notice that follows it.
	return m, seqCmds(approvalCmd, tea.Println(strings.TrimRight(block, "\n")))
}

// partialSuffix marks a cancelled reply inside the conversation. The model reads
// its own previous messages back, so the note has to be in the message itself —
// there is nowhere else to put it that the next request would carry.
const partialSuffix = "\n\n[the user cancelled this reply before it finished]"

// keepPartialInHistory appends the cancelled reply to the conversation so the
// next turn can continue it. It is a no-op when nothing was streamed — including
// after a tool step, which clears streamText once its preface is committed to
// history, so there is no path here that appends the same text twice.
func (m *model) keepPartialInHistory() {
	text := strings.TrimSpace(m.streamText)
	if text == "" {
		return
	}
	m.history = append(m.history, client.Message{
		Role:    "assistant",
		Content: text + partialSuffix,
	})
	m.app.MsgCount++
}

// fetchMe loads the account info (credits balance) off the UI goroutine.
func fetchMe(cli *client.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		me, err := cli.Me(ctx)
		return meMsg{me: me, err: err}
	}
}

// waitStream blocks for the next event on the stream channel.
func waitStream(ch <-chan client.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		return streamEventMsg{ev: ev, ok: ok}
	}
}

// applyApprovalChoice resolves the pending approval by the chosen option:
// 0 = approve once, 1 = approve and stop asking (auto-approve the rest of the
// session), 2 = reject.
func (m model) applyApprovalChoice(choice int) (tea.Model, tea.Cmd) {
	switch choice {
	case 1:
		name := ""
		if m.pendingTool != nil {
			name = m.pendingTool.Function.Name
		}
		switch {
		case name == tools.ToolRunCommand:
			m.app.Approvals = state.ApprovalEdits
			m.notice = "command approved once; shell commands always require approval"
		case tools.IsMCPName(name):
			// "Allow for this session" must not turn into "and also auto-approve
			// file edits from now on". The user answered a question about a
			// third-party tool; nothing else may be inferred from that answer, and
			// MCP tools themselves have no session grant at all (roadmap.txt:333).
			m.notice = "MCP tool approved once; every external tool call asks"
		default:
			m.app.Approvals = state.ApprovalAll
			m.notice = "auto-approval for edits enabled; commands still ask"
		}
		return m.resolveApproval(true)
	case 2:
		return m.resolveApproval(false)
	default:
		return m.resolveApproval(true)
	}
}

// toolDoneMsg carries the result of a tool executed off the UI goroutine (M9).
type toolDoneMsg struct {
	call    client.ToolCall
	result  string
	mutated bool

	// untrusted is a fully fenced block of third-party output, already wrapped by
	// tools.FenceUntrusted. It is delivered separately from result because the two
	// go to different places: result is written by this CLI and lands in the
	// tool-role message and in WORKING STATE (a system message), while untrusted
	// may only ever appear in the user role behind delimiters
	// (.ai/SECURITY.md:33, roadmap.txt:333). Empty for every built-in tool.
	untrusted string
}

// recordTool appends a tool-role result to the history-in-progress and a
// display line. ok marks whether the action ran (vs rejected).
func (m *model) recordTool(c client.ToolCall, result string, mutated, ok bool) {
	summary := tools.Summary(c.Function.Name, c.Function.Arguments)
	glyph := styles.Subtle.Render("⚙")
	switch {
	case !ok:
		glyph = styles.Err.Render("✗")
	case mutated:
		glyph = styles.OK.Render("✎")
	}
	// Approval-gated calls are already logged with a one-line ✓/✗ status the
	// moment the user decides (resolveApproval) — skip the duplicate line.
	if c.ID != "" && c.ID == m.announcedToolID {
		m.announcedToolID = ""
	} else {
		m.toolLines = append(m.toolLines, "  "+glyph+" "+styles.Muted.Render(summary))
	}
	m.toolResults = append(m.toolResults, client.Message{
		Role:       "tool",
		ToolCallID: c.ID,
		Name:       c.Function.Name,
		Content:    result,
	})
	m.noteWorkingState(c, result, mutated, ok)
	m.noteToolForVerification(c, result, mutated, ok)
}

// untrustedMessages turns this step's collected MCP payloads into user messages,
// preceded once per conversation by the guard directive.
//
// They go after every tool-role result, not interleaved with them: a provider
// requires the tool results answering one assistant turn to be contiguous, and a
// user message in the middle of that run makes the request invalid.
//
// The guard is a system message and the payloads are user messages — the split
// .ai/SECURITY.md:33 requires. It is sent immediately before the first block
// rather than at conversation start so a session that never calls an MCP tool
// never pays for it, and it is sent once because the whole history is re-sent on
// every tool step.
func (m *model) untrustedMessages() []client.Message {
	if len(m.untrustedBlocks) == 0 {
		return nil
	}
	out := make([]client.Message, 0, len(m.untrustedBlocks)+1)
	if !m.mcpGuardSent {
		m.mcpGuardSent = true
		out = append(out, client.Message{Role: "system", Content: tools.GuardDirective})
	}
	for _, b := range m.untrustedBlocks {
		out = append(out, client.Message{Role: "user", Content: b})
	}
	return out
}

// continueAfterTools flushes the resolved tool actions to the history and
// scrollback, then re-issues the request so the model continues the turn.
func (m model) continueAfterTools() (tea.Model, tea.Cmd) {
	m.history = append(m.history, m.toolResults...)
	m.history = append(m.history, m.untrustedMessages()...)
	m.untrustedBlocks = nil

	var printed string
	if strings.TrimSpace(m.turnPreface) != "" {
		printed = strings.TrimRight(styles.Message(styles.RoleAgent, clock(m.turnStarted), m.streamMeta, m.turnPreface, m.width), "\n") + "\n"
	}
	printed += strings.Join(m.toolLines, "\n")

	m.toolResults = nil
	m.toolLines = nil
	m.turnPreface = ""
	m.app.Phase = state.PhaseGenerating

	// Continue within the same turn context so Esc/quit still cancels mid-turn.
	ctx := m.turnCtx
	if ctx == nil {
		ctx = m.beginTurn()
	}
	req := m.buildRequest()
	return m, tea.Batch(
		tea.Println(printed),
		startStream(ctx, m.provider, req),
	)
}

// updateSubmodels forwards non-key messages to the spinner.
func (m model) updateSubmodels(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.spin, cmd = m.spin.Update(msg)
	return m, cmd
}
