// Package agentcore — ядро агента Qeuro без зависимостей от Bubble Tea.
// Фаза 0 роадмапа: сюда переносится цикл из internal/tui/model.go + update.go.
package agentcore

import (
	"encoding/json"
	"io"
	"sync"
)

// ProtocolVersion — версия схемы событий (Agent Protocol v1, поля v1.1 опциональны).
const ProtocolVersion = 1

// Виды событий протокола.
//
// Значения намеренно продублированы из общего контракта агентного протокола
// (`domain/agentproto` в репозитории сервисов), а не импортированы: этот
// репозиторий — самостоятельный CLI-модуль, и образ headless-агента собирается
// из одного каталога, поэтому импорт соседнего модуля сломал бы сборку
// контейнера, который и запускает headless. Дубль без защиты разошёлся бы —
// ровно то, о чём предупреждает пакетный комментарий agentproto, — поэтому
// расхождение ловит TestEventKindsMatchSharedContract: он читает файл
// контракта, когда тот доступен (чекаут монорепозитория рядом), и пропускается
// там, где его нет (этот репозиторий и контейнер).
const (
	KindToken           = "token"
	KindAssistant       = "assistant"
	KindStatus          = "status"
	KindToolCall        = "tool_call"
	KindFileWrite       = "file_write"
	KindCommand         = "command"
	KindApprovalRequest = "approval_request"
	KindUsage           = "usage"
	KindError           = "error"
	KindDone            = "done"
	KindRoute           = "route"
)

// Статусы события done.
const (
	DoneOK        = "ok"
	DoneCancelled = "cancelled"
	DoneError     = "error"
)

// Event — одно событие протокола: одна JSON-строка в stdout.
// Хост игнорирует неизвестные поля и kind (forward-совместимость).
type Event struct {
	V     int    `json:"v"`
	RunID string `json:"run_id"`
	Kind  string `json:"kind"`

	Text      string  `json:"text,omitempty"`
	Name      string  `json:"name,omitempty"`
	ID        string  `json:"id,omitempty"`
	Path      string  `json:"path,omitempty"`
	Diff      string  `json:"diff,omitempty"`
	Before    *string `json:"before,omitempty"` // v1.1: для Monaco diff и «Откатить»
	After     *string `json:"after,omitempty"`  // v1.1
	Cmd       string  `json:"cmd,omitempty"`
	ExitCode  *int    `json:"exit_code,omitempty"`
	Tokens    int     `json:"tokens,omitempty"`
	TokensIn  int     `json:"tokens_in,omitempty"`  // v1.1: для точного TUI-отображения
	TokensOut int     `json:"tokens_out,omitempty"` // v1.1
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Model     string  `json:"model,omitempty"`
	Action    string  `json:"action,omitempty"`
	Preview   string  `json:"preview,omitempty"`
	Status    string  `json:"status,omitempty"` // done: ok | cancelled | error
	Code      string  `json:"code,omitempty"`   // error
}

// Emitter потокобезопасно пишет события JSONL.
type Emitter struct {
	mu    sync.Mutex
	w     io.Writer
	runID string
}

func NewEmitter(w io.Writer, runID string) *Emitter {
	return &Emitter{w: w, runID: runID}
}

func (e *Emitter) Emit(ev Event) error {
	ev.V = ProtocolVersion
	ev.RunID = e.runID
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = e.w.Write(b)
	return err
}

// Compile-time check that Emitter implements EventEmitter from engine.go
var _ interface{ Emit(Event) error } = (*Emitter)(nil)
