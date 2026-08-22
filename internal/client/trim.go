package client

import "strings"

// Token-economy policy for any agentic tool loop: re-sending the full
// conversation every tool step is the dominant cost, because tool results and
// tool-call arguments often carry whole files. Keep the recent working set
// intact and DROP stale tool exchanges in pairs — never rewrite historical
// tool-call arguments in place: providers validate old calls against the
// current tool schema, and stub bodies like {"_elided":true} fail that
// validation and poison the whole request.

const (
	// keepFullToolResults is how many of the most recent tool results are kept
	// (their matching assistant tool-call turns stay intact as well).
	keepFullToolResults = 3
	// maxToolResultChars caps the size of even a recent tool result. Clipping
	// free-form result text is safe — unlike call arguments, it carries no
	// schema the provider re-validates.
	maxToolResultChars = 4000
	// maxAssistantContentChars keeps intermediate assistant narration from
	// being replayed in full on every tool step.
	maxAssistantContentChars = 2000
	// maxOldUserContentChars protects long sessions with repeated pasted input
	// while preserving the newest user request in full.
	maxOldUserContentChars = 6000
	// maxWindowMsgs bounds how many messages are sent on a very long agentic
	// task, so context cannot overflow. The first message is always kept.
	maxWindowMsgs = 40
)

// TrimMessages returns a copy of the conversation safe to re-send. The recent
// tool working set is kept; STALE tool exchanges are removed in pairs:
//
//  1. a stale tool result is dropped entirely;
//  2. via its tool_call_id, the matching call is cut out of the parent
//     assistant message's ToolCalls array;
//  3. an assistant message left with no tool calls and no text is dropped.
//
// Message pairing (tool_call ids ↔ tool results) therefore stays valid at all
// times and no historical call arguments are ever modified. Index 0 is always
// kept (the system prompt for team workers, or the original task for the solo
// loop).
func TrimMessages(history []Message) []Message {
	lastUser := -1
	var toolResultIdx []int
	for i, m := range history {
		if m.Role == "user" {
			lastUser = i
		}
		if m.Role == "tool" {
			toolResultIdx = append(toolResultIdx, i)
		}
	}

	// The most recent tool results stay; older ones are dropped pairwise.
	keepResult := make(map[int]bool, keepFullToolResults)
	for i := len(toolResultIdx) - 1; i >= 0 && len(keepResult) < keepFullToolResults; i-- {
		keepResult[toolResultIdx[i]] = true
	}

	// Pass 1: mark stale tool results and collect their call ids so the parent
	// assistant turns can be un-paired symmetrically.
	dropMsg := make(map[int]bool)
	dropCall := make(map[string]bool)
	for i, m := range history {
		if m.Role == "tool" && !keepResult[i] && i != 0 {
			dropMsg[i] = true
			if m.ToolCallID != "" {
				dropCall[m.ToolCallID] = true
			}
		}
	}

	// Pass 2: rebuild the history, cutting dropped calls out of assistant
	// turns and clipping free-form content.
	out := make([]Message, 0, len(history))
	for i, m := range history {
		if dropMsg[i] {
			continue
		}
		if len(m.ToolCalls) > 0 {
			kept := make([]ToolCall, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				if !dropCall[c.ID] {
					kept = append(kept, c)
				}
			}
			m.ToolCalls = kept
			// An assistant turn that existed only to make now-dropped calls
			// carries no information — delete it entirely (i != 0 is implied:
			// index 0 is never an empty tool-call shell in practice, but guard
			// anyway to honor the "always keep the task" contract).
			if len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == "" && i != 0 {
				continue
			}
		}
		switch {
		case m.Role == "tool":
			m.Content = clipText(m.Content, maxToolResultChars)
		case m.Role == "assistant":
			m.Content = clipText(m.Content, maxAssistantContentChars)
		case m.Role == "user" && i != 0 && i != lastUser:
			m.Content = clipText(m.Content, maxOldUserContentChars)
		}
		out = append(out, m)
	}
	return windowMessages(out)
}

// windowMessages keeps the first message plus a recent window so very long
// sessions stay within the model's context. The window start is backed off any
// "tool" message so a tool result is never sent without its assistant turn.
func windowMessages(msgs []Message) []Message {
	if len(msgs) <= maxWindowMsgs {
		return msgs
	}
	start := len(msgs) - maxWindowMsgs
	for start > 1 && msgs[start].Role == "tool" {
		start--
	}
	if start <= 1 {
		return msgs
	}
	out := make([]Message, 0, len(msgs)-start+1)
	out = append(out, msgs[0])
	out = append(out, msgs[start:]...)
	return out
}

// clipText truncates s to at most n characters, appending a marker when cut.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.ToValidUTF8(s[:n], "")
	return cut + "\n...[truncated]"
}
