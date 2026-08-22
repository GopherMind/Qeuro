package tui

import (
	"strings"
	"testing"

	"qeuro/internal/client"
)

// Simulates the token cost of a multi-step agent turn: a file is read/written
// repeatedly, and the whole history is re-sent each step. It asserts that
// trimming the history before each send dramatically cuts the cumulative input
// size versus sending the raw history every time (the bug behind the 120k-token
// blowup). Pure local arithmetic — no backend needed.
func TestTokenEconomyAcrossSteps(t *testing.T) {
	file := strings.Repeat("Z", 16000) // ~16 KB file, re-read/written each step

	var history []client.Message
	history = append(history, client.Message{Role: "user", Content: "сделай простую игру"})

	rawSent, trimmedSent := 0, 0
	const steps = 6
	for i := 0; i < steps; i++ {
		// Each step re-sends the conversation so far (this is what the loop does).
		rawSent += totalContent(history)
		trimmedSent += totalContent(trimmedHistory(history))

		// The model writes the file and gets a tool result back.
		history = append(history,
			client.Message{Role: "assistant", ToolCalls: []client.ToolCall{{
				ID: "c", Type: "function",
				Function: client.FunctionCall{Name: "write_file", Arguments: `{"path":"game.go","content":"` + file + `"}`},
			}}},
			client.Message{Role: "tool", ToolCallID: "c", Name: "write_file", Content: "ок: файл game.go записан"},
		)
	}

	t.Logf("cumulative input chars — raw: %d, trimmed: %d (%.1fx less)",
		rawSent, trimmedSent, float64(rawSent)/float64(max(trimmedSent, 1)))

	if trimmedSent*2 > rawSent {
		t.Fatalf("trimming did not meaningfully reduce tokens: raw=%d trimmed=%d", rawSent, trimmedSent)
	}
}

func totalContent(h []client.Message) int {
	n := 0
	for _, m := range h {
		n += len(m.Content)
		for _, c := range m.ToolCalls {
			n += len(c.Function.Arguments)
		}
	}
	return n
}
