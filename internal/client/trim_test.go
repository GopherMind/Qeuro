package client

import (
	"strings"
	"testing"
)

func TestTrimMessagesDropsStaleToolExchangesPairwise(t *testing.T) {
	big := strings.Repeat("X", 20000)
	h := []Message{{Role: "user", Content: "task"}}
	for i := 0; i < 6; i++ {
		id := "c" + string(rune('0'+i))
		h = append(h,
			Message{Role: "assistant", ToolCalls: []ToolCall{{
				ID: id, Type: "function",
				Function: FunctionCall{Name: "write_file", Arguments: `{"path":"game.go","content":"` + big + `"}`},
			}}},
			Message{Role: "tool", ToolCallID: id, Name: "write_file", Content: big},
		)
	}

	out := TrimMessages(h)

	full := totalContent(h)
	trimmed := totalContent(out)
	if trimmed >= full {
		t.Fatalf("trimmed (%d) should be much smaller than full (%d)", trimmed, full)
	}
	if full/max(trimmed, 1) < 2 {
		t.Fatalf("expected >=2x reduction, got full=%d trimmed=%d", full, trimmed)
	}

	// No message may contain a stub in place of real call arguments.
	for _, m := range out {
		for _, c := range m.ToolCalls {
			if strings.Contains(c.Function.Arguments, "_elided") {
				t.Fatal("historical tool-call arguments must never be stubbed")
			}
		}
	}

	// Pairing invariant: every remaining tool result has a parent assistant
	// call with the same id, and vice versa.
	assertPairingValid(t, out)

	// The task (index 0) is preserved.
	if out[0].Content != "task" {
		t.Error("first message must be preserved")
	}
}

func TestTrimMessagesKeepsRecentInFull(t *testing.T) {
	big := strings.Repeat("Y", 3000)
	h := []Message{{Role: "user", Content: "start"}}
	for i := 0; i < 5; i++ {
		id := "t" + string(rune('0'+i))
		h = append(h,
			Message{Role: "assistant", ToolCalls: []ToolCall{{
				ID: id, Type: "function",
				Function: FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`},
			}}},
			Message{Role: "tool", ToolCallID: id, Name: "read_file", Content: big},
		)
	}
	out := TrimMessages(h)

	// The newest tool result survives in full; the oldest exchanges are gone.
	last := out[len(out)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, "YYYY") {
		t.Error("most recent tool result should be kept in full")
	}
	stale := 0
	for _, m := range out {
		if m.Role == "tool" {
			stale++
		}
	}
	if stale != keepFullToolResults {
		t.Fatalf("want %d kept tool results, got %d", keepFullToolResults, stale)
	}
	assertPairingValid(t, out)
}

func TestTrimMessagesNeverRewritesRecentCallArguments(t *testing.T) {
	big := strings.Repeat("Z", 20000)
	h := []Message{
		{Role: "user", Content: "write a file"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "c", Type: "function",
			Function: FunctionCall{Name: "write_file", Arguments: `{"path":"app.go","content":"` + big + `"}`},
		}}},
		{Role: "tool", ToolCallID: "c", Name: "write_file", Content: "ok"},
	}

	out := TrimMessages(h)
	args := out[1].ToolCalls[0].Function.Arguments
	if args != h[1].ToolCalls[0].Function.Arguments {
		t.Fatal("recent tool-call arguments must be resent verbatim — providers validate them against the tool schema")
	}
	assertPairingValid(t, out)
}

func TestTrimMessagesDropsEmptiedAssistantTurn(t *testing.T) {
	h := []Message{
		{Role: "user", Content: "task"},
		// A stale exchange: the assistant turn carries ONLY the tool call and
		// no text, so once the call is cut the whole message must go.
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "old", Type: "function",
			Function: FunctionCall{Name: "read_file", Arguments: `{"path":"a"}`},
		}}},
		{Role: "tool", ToolCallID: "old", Name: "read_file", Content: "aaa"},
	}
	// Add enough fresh exchanges to push "old" out of the keep-full window.
	for i := 0; i < keepFullToolResults; i++ {
		id := "n" + string(rune('0'+i))
		h = append(h,
			Message{Role: "assistant", Content: "working", ToolCalls: []ToolCall{{
				ID: id, Type: "function",
				Function: FunctionCall{Name: "read_file", Arguments: `{"path":"b"}`},
			}}},
			Message{Role: "tool", ToolCallID: id, Name: "read_file", Content: "bbb"},
		)
	}

	out := TrimMessages(h)
	for _, m := range out {
		if m.ToolCallID == "old" {
			t.Fatal("stale tool result must be dropped")
		}
		for _, c := range m.ToolCalls {
			if c.ID == "old" {
				t.Fatal("parent assistant call must be cut together with its result")
			}
		}
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == "" {
			t.Fatal("assistant shell with no calls and no text must be dropped")
		}
	}
	assertPairingValid(t, out)
}

func TestTrimMessagesWindowKeepsTaskAndPairing(t *testing.T) {
	h := []Message{{Role: "user", Content: "ЗАДАЧА"}}
	for i := 0; i < 80; i++ {
		h = append(h,
			Message{Role: "assistant", Content: "step", ToolCalls: []ToolCall{{
				ID: "c", Type: "function",
				Function: FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`},
			}}},
			Message{Role: "tool", ToolCallID: "c", Name: "read_file", Content: "data"},
		)
	}
	out := TrimMessages(h)

	if len(out) > maxWindowMsgs+1 {
		t.Fatalf("window not applied: %d messages", len(out))
	}
	if out[0].Content != "ЗАДАЧА" {
		t.Error("first message (the task) must be preserved")
	}
	if out[1].Role == "tool" {
		t.Error("window starts on an orphaned tool message")
	}
}

// assertPairingValid checks the provider invariant: every tool result pairs
// with a preceding assistant call of the same id and every assistant call has
// a following tool result (unless it is part of the newest, not-yet-answered
// turn — which TrimMessages never produces from a well-formed history).
func assertPairingValid(t *testing.T, msgs []Message) {
	t.Helper()
	calls := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			calls[c.ID] = true
		}
		if m.Role == "tool" && !calls[m.ToolCallID] {
			t.Fatalf("tool result %q has no preceding assistant call", m.ToolCallID)
		}
	}
}

func totalContent(h []Message) int {
	n := 0
	for _, m := range h {
		n += len(m.Content)
		for _, c := range m.ToolCalls {
			n += len(c.Function.Arguments)
		}
	}
	return n
}
