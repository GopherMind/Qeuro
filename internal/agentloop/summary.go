package agentloop

import (
	"encoding/json"
	"fmt"
	"strings"

	"qeuro/internal/tools"
)

// SummarizeStep описывает один исполненный вызов одной строкой для WORKING
// STATE. Формат общий для TUI и headless: сводка попадает в промпт, поэтому
// разный формат — это разный вход модели при одинаковых действиях.
func SummarizeStep(name, argsJSON, result string, mutated, ran bool) string {
	args := toolArgs(argsJSON)
	outcome := "ok"
	first := strings.ToLower(FirstResultLine(result))
	switch {
	case !ran:
		outcome = "rejected"
	case strings.Contains(first, "error"):
		outcome = "error"
	case strings.Contains(first, "failed"):
		outcome = "failed"
	}

	var line string
	switch name {
	case tools.ToolReadFile:
		line = fmt.Sprintf("read %s: %s", valueOr(args["path"], "(unknown)"), FirstResultLine(result))
	case tools.ToolListDir:
		line = fmt.Sprintf("listed %s: %s", valueOr(args["path"], "."), FirstResultLine(result))
	case tools.ToolSearchCode:
		line = fmt.Sprintf("searched %q in %s: %s", valueOr(args["query"], ""), valueOr(args["path"], "."), FirstResultLine(result))
	case tools.ToolPatchFile:
		line = fmt.Sprintf("patched %s: %s", valueOr(args["path"], "(unknown)"), FirstResultLine(result))
	case tools.ToolWriteFile:
		line = fmt.Sprintf("wrote %s: %s", valueOr(args["path"], "(unknown)"), FirstResultLine(result))
	case tools.ToolRunCommand:
		line = fmt.Sprintf("ran %q: %s", valueOr(args["command"], ""), FirstResultLine(result))
	case tools.ToolRecall:
		line = fmt.Sprintf("recalled memory %s: %s", valueOr(args["category"], "summary"), FirstResultLine(result))
	case tools.ToolRemember:
		line = fmt.Sprintf("remembered %s: %s", valueOr(args["category"], "notes"), FirstResultLine(result))
	default:
		line = fmt.Sprintf("%s: %s", name, FirstResultLine(result))
	}
	if mutated && ran {
		line += "; code changed"
	}
	if outcome != "ok" {
		line += "; " + outcome
	}
	return ClipStateLine(line)
}

// commandArg достаёт поле command из аргументов вызова. Ошибка разбора даёт
// пустую строку, а не панику: аргументы приходят от модели и валидными не
// гарантированы.
func commandArg(argsJSON string) string {
	var args struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)
	return strings.TrimSpace(args.Command)
}

func toolArgs(raw string) map[string]string {
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(generic))
	for k, v := range generic {
		if s, ok := v.(string); ok {
			out[k] = strings.TrimSpace(s)
		}
	}
	return out
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}
