// Package state holds the live application state shared across the TUI:
// current mode, selected model, connection status and context usage.
package state

import "qeuro/internal/catalog"

// Mode is the routing behaviour for incoming prompts.
type Mode int

const (
	ModeAuto Mode = iota // auto-router picks the model
	ModeChat             // pinned to a single model
)

func (m Mode) String() string {
	if m == ModeChat {
		return "chat"
	}
	return "auto"
}

// Connection is the link status to the backend proxy.
type Connection int

const (
	Online Connection = iota
	Offline
	Connecting
)

func (c Connection) String() string {
	switch c {
	case Offline:
		return "offline"
	case Connecting:
		return "connecting"
	default:
		return "online"
	}
}

// Phase is the current activity of the agent, used to drive status/spinner.
type Phase int

const (
	PhaseIdle Phase = iota
	PhaseGenerating
	PhaseError
	PhaseDone
)

// OutputMode controls how verbose model output is — a token-economy lever
// (plan §14). full = normal prose; concise = no fluff; caveman = code/diffs only.
type OutputMode int

const (
	OutputConcise OutputMode = iota // default: direct, no preamble
	OutputFull                      // normal prose
	OutputCaveman                   // only code/diffs, minimal words
)

func (o OutputMode) String() string {
	switch o {
	case OutputFull:
		return "full"
	case OutputCaveman:
		return "caveman"
	default:
		return "concise"
	}
}

// Approval controls which agent actions need the user's confirmation.
type Approval int

const (
	ApprovalAsk   Approval = iota // confirm every edit and command (default)
	ApprovalEdits                 // auto-approve file edits; still ask for commands
	ApprovalAll                   // auto-approve file writes; still ask for commands
)

func (a Approval) String() string {
	switch a {
	case ApprovalEdits:
		return "edits"
	case ApprovalAll:
		return "all"
	default:
		return "ask"
	}
}

// UsageRecord is token and billing telemetry for one completed model request.
type UsageRecord struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	CostUSD           float64
	Credits           float64
	SavedUSD          float64
	Balance           float64
}

// TotalTokens returns all tokens reported for the request.
func (u UsageRecord) TotalTokens() int {
	return u.InputTokens + u.OutputTokens
}

// BillableInputTokens returns input tokens not reported as cache hits.
func (u UsageRecord) BillableInputTokens() int {
	billable := u.InputTokens - u.CachedInputTokens
	if billable < 0 {
		return 0
	}
	return billable
}

// UsageStats keeps the current turn split separately from session totals.
type UsageStats struct {
	Last     UsageRecord
	Total    UsageRecord
	Requests int
}

// RecordUsage updates last-turn and cumulative usage telemetry.
func (s *UsageStats) RecordUsage(u UsageRecord) {
	s.Last = u
	s.Total.InputTokens += u.InputTokens
	s.Total.OutputTokens += u.OutputTokens
	s.Total.CachedInputTokens += u.CachedInputTokens
	s.Total.CostUSD += u.CostUSD
	s.Total.Credits += u.Credits
	s.Total.SavedUSD += u.SavedUSD
	s.Total.Balance = u.Balance
	s.Requests++
}

// Reset clears all usage telemetry.
func (s *UsageStats) Reset() {
	*s = UsageStats{}
}

// App is the mutable application state.
type App struct {
	Mode      Mode
	Model     catalog.Model  // active model (used when Mode == ModeChat)
	Effort    catalog.Effort // active reasoning effort
	Output    OutputMode     // output verbosity (token economy)
	Approvals Approval       // which agent actions require confirmation
	Conn      Connection
	Phase     Phase
	CtxUsed   int // tokens used in the current context window
	CtxLimit  int // total context window size
	MsgCount  int // number of messages exchanged this session
	Usage     UsageStats
}

// New returns sensible defaults for a fresh session.
func New() *App {
	m := catalog.Default()
	return &App{
		Mode:      ModeAuto,
		Model:     m,
		Effort:    m.DefaultEffort(),
		Output:    OutputConcise,
		Approvals: ApprovalAsk,
		Conn:      Online,
		Phase:     PhaseIdle,
		CtxUsed:   0,
		CtxLimit:  200_000,
	}
}

// ModelLabel returns the short label of the active model.
func (a *App) ModelLabel() string { return a.Model.Label }

// SetModel switches the active model and clamps effort to a supported level.
func (a *App) SetModel(m catalog.Model) {
	a.Model = m
	if !m.Supports(a.Effort) {
		a.Effort = m.DefaultEffort()
	}
}

// CtxPercent returns context usage as an integer percentage 0..100.
func (a *App) CtxPercent() int {
	if a.CtxLimit <= 0 {
		return 0
	}
	p := a.CtxUsed * 100 / a.CtxLimit
	if p > 100 {
		p = 100
	}
	return p
}
