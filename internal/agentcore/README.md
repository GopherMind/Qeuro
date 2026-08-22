# agentcore — каркас (Фаза 0)

Цель: агент Qeuro работает без TUI, как библиотека + headless CLI.

Что уже готово в каркасе:
- `events.go` — типы событий Agent Protocol v1 (+ поля v1.1 `before`/`after`) и потокобезопасный JSONL-эмиттер;
- `host.go` — чтение команд хоста из stdin (`approval_response`, `cancel`) в каналы;
- `engine.go` — синхронный Engine с готовым блокирующим `RequestApproval` (и AutoApprove для облака);
- `run.go` — разбор `qeuro run --headless --jsonl [--model m] "<prompt>"`, готов к подключению в `main.go`;
- `engine_test.go` — тест-инвариант «никакого Bubble Tea» + план golden-тестов.

Что осталось (ручная работа, нужен Go-toolchain и итерации):
1. Перенести цикл из `internal/tui/model.go` + `update.go` в `Engine.Run` по схеме в комментарии.
2. Подключить зависимости в `Deps` (internal/tools, internal/client, internal/memory, catalog — все уже без Bubble Tea).
3. `file_write`: заполнять `Before`/`After` из `snapshotFile` — это включит diff и «Откатить» в десктопе.
4. Записать golden-эталоны и добить `go test ./...`.
