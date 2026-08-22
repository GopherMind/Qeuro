package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"

	"qeuro/internal/agentloop"
	"qeuro/internal/catalog"
	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/session"
	"qeuro/internal/skills"
	"qeuro/internal/state"
	"qeuro/internal/styles"
	"qeuro/internal/tools"
)

// model is the root Bubble Tea model. It runs inline (no alt-screen): finished
// messages are flushed to the terminal scrollback via tea.Println, while the
// input box, hint line and status bar are redrawn live at the bottom.
type model struct {
	version string
	app     *state.App

	input    textarea.Model
	spin     spinner.Model
	pal      palette
	sel      selector
	notice   string   // transient in-terminal notification (one line)
	infoView string   // transient info panel (/help, /context, /usage) — any key closes
	pastes   []string // large pasted blocks, shown as [paste N lines] labels

	// Input buffering. On Windows a paste arrives as a burst of single-char key
	// events (no bracketed-paste marker), with embedded newlines delivered as
	// separate Enter keys. Rather than insert each char live (O(n²) redraws that
	// stutter and split large pastes), we accumulate keystrokes in pendingInput
	// — O(1) per char — and commit once the burst settles: a multi-line run
	// becomes a compact "[paste N lines]" label, anything else is inserted as-is.
	// lastRuneAt distinguishes a real "submit" Enter from a paste newline.
	lastRuneAt   time.Time
	pendingInput string // buffered keystrokes not yet committed to the textarea
	pasteGen     int    // bumped per buffered key; a flush only acts if still current

	// backend wiring. cli is the backend client, used for the account endpoints
	// (/v1/me, /v1/models, providers, revoke) that only the backend has. provider
	// is where inference goes, and in offline mode it is a local model server
	// instead (roadmap §8 row "Offline") — which is why the two are separate: the
	// account calls have no local equivalent and are simply not made.
	cli        *client.Client
	provider   client.Provider
	local      bool   // offline mode: nothing is sent to the backend
	localAt    string // local model endpoint, for the status line and errors
	localModel string // configured model label; empty means server-selected
	// unsafeParallelWrites carries the roadmap-v3 §4.1 rollout flag through to the
	// team engine. Off by default, meaning a parallel team step is read-only.
	unsafeParallelWrites bool
	loggedIn             bool
	baseURL              string                  // backend API origin (from clientcfg)
	consoleURL           string                  // web console origin (provider sync, M7)
	providers            []client.ProviderConfig // providers linked on the web console (M7)
	projectID            string
	history              []client.Message // conversation, sent each request for context

	// tools (function calling) — local file operations the model can request
	runner *tools.Runner

	// Durable session journal (roadmap §8, row "Сессии"). sessionID is this run's
	// id — printed on the welcome screen, so `qeuro resume <id>` can name it — and
	// journal is the append-only, fsynced transcript. A nil journal means "no
	// config directory": journalling is absent, never silently redirected.
	// journalWarned keeps a failing disk from reporting itself on every turn.
	sessionID     string
	journal       *session.Journal
	journalWarned bool

	// initCmd is an extra command Init returns, used by `qeuro resume` to print
	// the restored transcript. Bubble Tea runs no command until Init, so work
	// prepared before the program starts has to be handed over rather than run.
	initCmd tea.Cmd

	// mcp holds the connections to the user's configured MCP servers. It is nil
	// until the async start finishes (or when no server is configured), and every
	// registered MCP tool requires explicit approval, so a nil manager degrades to
	// "no third-party tools", never to "unchecked ones".
	mcp mcpCaller

	// untrustedBlocks are the fenced MCP payloads waiting to be appended to the
	// history as user messages after this step's tool results. They are collected
	// rather than appended inline because a provider requires every tool-role
	// result for one assistant turn to be contiguous: a user message in the middle
	// of that run makes the whole request invalid.
	untrustedBlocks []string

	// mcpGuardSent records that the untrusted-data directive is already in this
	// conversation. It is sent once, immediately before the first fenced block,
	// rather than on every step: it is a fixed ~1 KB of text and the whole history
	// is re-sent on each tool step.
	mcpGuardSent bool

	toolStep int // current tool-loop iteration, to bound runaway loops
	// workingState — компактная сводка хода, живёт в agentloop: тот же тип
	// использует headless-движок, поэтому формат сводки один на оба входа.
	workingState      agentloop.WorkingState
	turnMemoryDigest  string
	turnStartIndex    int
	turnHistoryStable bool
	toolWarned        bool // whether the model already got a near-limit warning
	toolFinal         bool // final no-tools pass after the tool-loop limit

	// Verification gate: after a successful file edit, the solo agent cannot
	// finish until a focused build/test/lint/typecheck command succeeds.
	verificationRequired bool
	verificationPassed   bool
	verificationNote     string

	// live streaming state for the in-flight reply
	streaming    bool
	streamText   string // accumulated partial output
	streamMeta   string // "model · effort" from the route event
	streamErr    string // non-empty if the stream reported an error
	streamCh     <-chan client.Event
	pendingCalls []client.ToolCall // tool calls awaiting local execution
	lastUsage    *client.Usage
	agentHost    *agentHost // solo agent host adapter (nil when idle or in team mode)

	// Turn cancellation (H2). turnCtx bounds the whole in-flight turn (a solo
	// stream across its tool steps, or a team run); turnCancel cancels it on Esc
	// (in-session abort) or on quit (so the worker goroutine is not orphaned).
	// interrupted records that the user, not the backend, ended the turn.
	turnCtx     context.Context
	turnCancel  context.CancelFunc
	interrupted bool

	// quitArmed records that Ctrl+C was pressed with nothing to cancel, so the
	// next one exits (roadmap §8, row "Прерывание": first Ctrl+C is a soft
	// cancel, second quits). It is cleared by any other key: a Ctrl+C the user
	// has already moved on from must not still be holding a loaded exit.
	quitArmed bool

	// budget is the session's hard spend ceiling (roadmap §8, row "Стоимость").
	// It is checked before a turn and between tool steps, because a turn can bill
	// up to maxToolSteps times without the user acting.
	budget budget

	// account/usage surfaced in the status bar
	credits      float64 // remaining credits balance
	creditsKnown bool    // whether we have a real balance yet
	tier         string  // subscription tier (free/pro/mid/ultra) — picks team profile
	turnStarted  time.Time

	// team mode: a multi-agent orchestration run for the next message
	teamMode          bool
	skills            *skills.Library // lazily loaded skill library for team workers
	teamCh            chan teamEvent  // active team run's progress channel (nil when idle)
	teamReplyCh       chan string     // delivers the user's answer back to a waiting team run
	awaitingTeamInput bool            // the team paused to ask the user a question

	// tool-approval state machine: mutating edits wait for the user's choice
	awaitingApproval bool
	approvalChoice   int               // highlighted option: 0 yes · 1 yes+always · 2 no
	pendingTool      *client.ToolCall  // the mutating call awaiting a decision
	pendingPreview   string            // diff/preview shown to the user
	announcedToolID  string            // call already logged by the approval status line
	toolQueue        []client.ToolCall // remaining calls to process this turn
	toolResults      []client.Message  // tool-role results accumulated this turn
	toolLines        []string          // display lines for the resolved actions
	turnPreface      string            // assistant text that preceded the tool calls

	width  int
	height int
	quit   bool
}

// newModel builds the initial model.
// newModel builds the starting model with no flag layer. Entry points that
// accept flags go through newModelWithFlags so the flag lands in the same
// resolution that records provenance for `config doctor`.
func newModel(version string) model { return newModelWithFlags(version, nil) }

func newModelWithFlags(version string, flags map[string]string) model {
	in := textarea.New()
	in.Placeholder = "ask Qeuro to inspect, edit, run, explain...  (/ commands)"
	in.Prompt = ""
	in.ShowLineNumbers = false
	in.CharLimit = 16000
	in.SetHeight(1)
	in.SetWidth(72)
	// Enter submits (handled in onKey); newline goes on Ctrl+J / Alt+Enter.
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))
	in.Focus()
	in.Cursor.Style = lipgloss.NewStyle().Foreground(styles.Cyan)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(styles.Indigo)

	cfg, cfgErr := clientcfg.LoadWithFlags(flags)
	wd, _ := os.Getwd()
	runner, _ := tools.NewRunner(wd)
	w := 80
	if tw, _, err := term.GetSize(os.Stdout.Fd()); err == nil && tw > 0 {
		w = tw
	}

	m := model{
		version: version,
		app:     state.New(),
		input:   in,
		spin:    sp,
		width:   w,
		// NewLazy, not New: this runs before the first frame, and resolving the
		// token means reading the OS keychain — a D-Bus round trip on Linux. The
		// client needs the value when it sends a request, which is after the prompt
		// is up (roadmap §8 "Startup"). LoggedIn answers presence without it.
		cli:        client.NewLazy(cfg.BaseURL, cfg.Secret),
		provider:   cfg.LazyProvider(),
		local:      cfg.Local,
		localAt:    cfg.LocalEndpoint(),
		localModel: cfg.LocalModel,

		unsafeParallelWrites: cfg.UnsafeParallelWrites,
		loggedIn:             cfg.LoggedIn(),
		baseURL:              cfg.BaseURL,
		consoleURL:           cfg.ConsoleURL,
		projectID:            projectID(),
		runner:               runner,
		budget:               budget{limit: cfg.Budget},
		turnStartIndex:       -1,
	}

	// A configured model pins the session to it (roadmap §8: the config layers
	// govern every entry point, not just headless runs). An id the catalogue
	// does not know is reported rather than silently ignored — the user asked
	// for a specific model, and starting on a different one without saying so is
	// the failure this row exists to prevent.
	if mdl, notice, ok := resolveConfiguredModel(cfg.Model, m.app.Model); ok {
		m.app.Model = mdl
		m.app.Effort = mdl.DefaultEffort()
		m.app.Mode = state.ModeChat
	} else if notice != "" {
		m.notice = notice
	}

	if cfgErr != nil {
		// Corrupt config file: warn instead of silently appearing logged out (L10).
		m.notice = "config is unreadable (" + clientcfg.FilePath() + ") — run qeuro login <token>"
		m.app.Conn = state.Offline
	} else if m.local {
		// "Offline" here is a transport guarantee, not an error state. Name both
		// endpoint and mode so the user can see before typing sensitive code that
		// this session will not use the backend.
		m.notice = "local session · " + clientcfg.DisplaySafe(m.localAt) + " · backend disabled"
		m.app.Conn = state.Offline
	} else if !m.loggedIn {
		m.notice = "offline session — qeuro login opens registration"
		m.app.Conn = state.Offline
	} else if len(cfg.Warnings) > 0 {
		// One line, not all of them: the status area is one line tall, and
		// `config doctor` is where the full list belongs.
		m.notice = cfg.Warnings[0] + "  (qeuro config doctor)"
	}

	// The session journal is created here rather than in Init because the id
	// appears on the welcome screen Init prints, and it is created *after* the
	// model is resolved so the meta record names the model the session will
	// actually use, not the default it started from. New() touches no disk — the
	// file appears with the first record — so this adds nothing to startup.
	m.sessionID = session.NewID(time.Now())
	if j, err := session.New(m.sessionID, time.Now(), session.Record{
		Version: version,
		Dir:     wd,
		Model:   m.app.Model.ID,
	}); err == nil {
		m.journal = j
	}
	return m
}

// resolveConfiguredModel decides what a configured model id means for a session.
// It is separate from newModel because newModel reads the real config and the
// terminal size, and this is the part worth testing: an id the catalogue does not
// know must be reported, not silently swapped for the default.
//
// The id is echoed into the terminal, so it goes through clientcfg.DisplaySafe.
// The TOML reader refuses control characters, but env vars and flags never pass
// through it, and QEURO_MODEL is one of those.
func resolveConfiguredModel(configured string, current catalog.Model) (catalog.Model, string, bool) {
	id := strings.TrimSpace(configured)
	if id == "" {
		return current, "", false
	}
	if mdl, _, ok := catalog.FindModel(id); ok {
		return mdl, "", true
	}
	return current, "configured model " + clientcfg.DisplaySafe(id) +
		" is not in the catalogue — using " + current.Label, false
}

// projectID derives a stable per-repository id from the working directory, so
// project memory is scoped to the current folder (backend.txt §8).
func projectID() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return "cli-default"
	}
	sum := sha256.Sum256([]byte(wd))
	return hex.EncodeToString(sum[:])[:16]
}

func (m model) Init() tea.Cmd {
	// Print the welcome screen once into scrollback, then start the spinner tick.
	// The welcome card and a resumed transcript are two prints whose order
	// matters, and tea.Batch gives no ordering guarantee — hence Sequence for
	// this pair specifically. m.initCmd is nil outside `qeuro resume`, and
	// Sequence drops nil commands, so the normal start is unchanged.
	cmds := []tea.Cmd{
		tea.Sequence(
			tea.Println(welcomeScreen(m.version, m.app.Model.Label, m.width)),
			m.initCmd,
		),
		m.spin.Tick,
		textarea.Blink,
	}
	if !m.local {
		// MCP servers may themselves be remote. A session explicitly started with
		// --local promises no internet access, so it must not auto-start arbitrary
		// configured processes whose transport the CLI cannot police.
		cmds = append(cmds, startMCPCmd())
	}
	if m.loggedIn && !m.local {
		cmds = append(cmds, fetchMe(m.cli)) // load credits balance for the status bar
		// M7: sync provider credentials linked on the web console so chat
		// requests can use them right away (same records as the Providers page).
		cmds = append(cmds, fetchProviders(m.cli, m.consoleURL))
		// §8 "Startup": revalidate the cached model catalogue. This is a tea.Cmd, so
		// it runs after Init returns and the first frame is drawn — the startup path
		// itself stays free of network calls, and a conditional request usually costs
		// one 304 with no body.
		cmds = append(cmds, refreshCatalog(m.cli))
	}
	return tea.Batch(cmds...)
}
