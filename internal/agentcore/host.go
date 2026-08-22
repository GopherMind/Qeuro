package agentcore

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// HostCommand — команда хоста из stdin (двусторонний протокол).
type HostCommand struct {
	Kind     string `json:"kind"`               // approval_response | cancel
	ID       string `json:"id,omitempty"`       // id из approval_request
	Decision string `json:"decision,omitempty"` // approve | deny
}

// ReadHostCommands читает stdin построчно и раздаёт команды по каналам.
// Неизвестные строки игнорируются (forward-совместимость).
func ReadHostCommands(r io.Reader) (approvals <-chan HostCommand, cancel <-chan struct{}) {
	ap := make(chan HostCommand, 4)
	cn := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			var c HostCommand
			if json.Unmarshal(sc.Bytes(), &c) != nil {
				continue
			}
			switch c.Kind {
			case "approval_response":
				ap <- c
			case "cancel":
				select {
				case cn <- struct{}{}:
				default:
				}
			}
		}
	}()
	return ap, cn
}

// ChannelEmitter отправляет события в канал вместо записи JSONL.
// TUI host использует его для получения typed Event вместо парсинга строк.
type ChannelEmitter struct {
	mu     sync.Mutex
	ch     chan<- Event
	runID  string
	closed bool
}

// NewChannelEmitter создаёт emitter, который отправляет события в канал.
// Канал должен быть буферизованным или иметь активного получателя, иначе
// отправка заблокирует engine. После cancellation новые события отбрасываются,
// чтобы отменённый engine не ждал вечно на send.
func NewChannelEmitter(ch chan<- Event, runID string) *ChannelEmitter {
	return &ChannelEmitter{ch: ch, runID: runID}
}

func (e *ChannelEmitter) Emit(ev Event) error {
	ev.V = ProtocolVersion
	ev.RunID = e.runID

	e.mu.Lock()
	defer e.mu.Unlock()

	// После terminal события или explicit close больше ничего не отправляем.
	// Engine.Run дедуплицирует done на своей границе; эта проверка сохраняет ту же
	// гарантию на границе TUI-хоста для любого другого producer.
	if e.closed {
		return nil
	}

	// Terminal событие закрывает поток. Это гарантирует, что хост получит ровно
	// одно done и сможет завершить UI-обработку, даже если engine попытается
	// отправить что-то после этого.
	if ev.Kind == KindDone {
		e.closed = true
		e.ch <- ev
		close(e.ch)
		return nil
	}

	// Неблокирующая отправка: если канал переполнен или никто не читает (race
	// между cancellation и emit), событие отбрасывается. Это лучше, чем зависший
	// engine, который ждёт send после того, как хост уже забрал Cancel.
	select {
	case e.ch <- ev:
	default:
	}
	return nil
}
