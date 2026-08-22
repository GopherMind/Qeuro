package tui

import (
	"strings"
	"testing"

	"qeuro/internal/client"
	"qeuro/internal/state"
	"qeuro/internal/tools"
)

func searchCodeCall(query, path string) client.ToolCall {
	return client.ToolCall{ID: "search-1", Type: "function",
		Function: client.FunctionCall{Name: tools.ToolSearchCode, Arguments: `{"query":"` + query + `","path":"` + path + `"}`}}
}

func TestWorkingStateAddedToContinuationRequest(t *testing.T) {
	m := model{app: state.New(), width: 80, projectID: "project-abc", toolStep: 1}
	m.history = append(m.history, client.Message{Role: "user", Content: "fix it"})
	m.recordTool(searchCodeCall("panic", "internal"), "internal/app.go:12: panic here", false, true)
	m.recordTool(patchCall("internal/app.go", "panic", "return err"), "ok: file changed", true, true)

	req := m.buildRequest()
	var found bool
	for _, msg := range req.Messages {
		if msg.Role == "system" && strings.Contains(msg.Content, "WORKING STATE") {
			found = true
			if !strings.Contains(msg.Content, "searched") || !strings.Contains(msg.Content, "patched internal/app.go") {
				t.Fatalf("working state lost tool evidence:\n%s", msg.Content)
			}
			if !strings.Contains(msg.Content, "verification pending") {
				t.Fatalf("working state should carry verification status:\n%s", msg.Content)
			}
		}
	}
	if !found {
		t.Fatal("expected compact working state system message")
	}
}

func TestWorkingStateDoesNotPrecedeHistory(t *testing.T) {
	m := model{app: state.New(), width: 80, projectID: "project-abc", toolStep: 1}
	m.history = append(m.history, client.Message{Role: "user", Content: "fix it"})
	m.recordTool(searchCodeCall("panic", "internal"), "internal/app.go:12: panic here", false, true)

	req := m.buildRequest()
	userIdx, stateIdx := -1, -1
	for i, msg := range req.Messages {
		if msg.Role == "user" && msg.Content == "fix it" {
			userIdx = i
		}
		if msg.Role == "system" && strings.Contains(msg.Content, "WORKING STATE") {
			stateIdx = i
		}
	}
	if userIdx < 0 || stateIdx < 0 {
		t.Fatalf("expected both user history and working state, got %+v", req.Messages)
	}
	if stateIdx < userIdx {
		t.Fatalf("working state should not break the cacheable prefix before history: state=%d user=%d", stateIdx, userIdx)
	}
}

func TestBuildRequestCarriesStableCacheSessionID(t *testing.T) {
	m := model{app: state.New(), width: 80, projectID: "project-abc"}
	m.history = append(m.history, client.Message{Role: "user", Content: "hello"})

	req := m.buildRequest()
	if req.ProjectID != "project-abc" {
		t.Fatalf("ProjectID = %q", req.ProjectID)
	}
	if req.SessionID != "qeuro-cli-project-abc" {
		t.Fatalf("SessionID = %q", req.SessionID)
	}
}

func TestBuildRequestCarriesShellDisciplinePrompt(t *testing.T) {
	m := model{app: state.New(), width: 80, projectID: "project-abc"}
	m.history = append(m.history, client.Message{Role: "user", Content: "fix tests"})

	req := m.buildRequest()
	var found bool
	for _, msg := range req.Messages {
		if msg.Role == "system" && strings.Contains(msg.Content, "SHELL DISCIPLINE") {
			found = true
			for _, want := range []string{"run_command", "Do not invent", "patch_file only", "write_file is only for brand-new files"} {
				if !strings.Contains(msg.Content, want) {
					t.Fatalf("shell prompt missing %q:\n%s", want, msg.Content)
				}
			}
		}
	}
	if !found {
		t.Fatal("expected shell discipline system prompt")
	}
}

func TestContinuationRequestCarriesShellDisciplinePrompt(t *testing.T) {
	m := model{app: state.New(), width: 80, projectID: "project-abc", toolStep: 2}
	m.history = append(m.history, client.Message{Role: "user", Content: "fix tests"})

	req := m.buildRequest()
	var found bool
	for _, msg := range req.Messages {
		if msg.Role == "system" && strings.Contains(msg.Content, "SHELL DISCIPLINE") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected shell discipline prompt on continuation requests")
	}
}

func TestContinuationRequestKeepsStablePromptPrefix(t *testing.T) {
	history := []client.Message{{Role: "user", Content: "fix tests"}}
	first := model{app: state.New(), width: 80, projectID: "project-abc", history: history, turnMemoryDigest: "stack: go"}
	next := model{app: state.New(), width: 80, projectID: "project-abc", history: history, toolStep: 2, turnMemoryDigest: "stack: go"}
	next.recordTool(searchCodeCall("panic", "internal"), "internal/app.go:12: panic here", false, true)

	a := first.buildRequest().Messages
	b := next.buildRequest().Messages
	if len(a) < 3 || len(b) < 3 {
		t.Fatalf("expected stable prompt prefix, got %d and %d messages", len(a), len(b))
	}
	for i := 0; i < 3; i++ {
		if a[i].Role != b[i].Role || a[i].Content != b[i].Content {
			t.Fatalf("prompt prefix differs at %d:\nfirst=%+v\nnext=%+v", i, a[i], b[i])
		}
	}
}

func TestCurrentTurnUserPromptStaysCacheStableAcrossToolSteps(t *testing.T) {
	task := strings.Repeat("fix this long pasted trace\n", 1400)
	history := []client.Message{{Role: "user", Content: task}}
	first := model{
		app:               state.New(),
		width:             80,
		projectID:         "project-abc",
		history:           append([]client.Message(nil), history...),
		turnStartIndex:    0,
		turnHistoryStable: true,
		turnMemoryDigest:  "stack: go",
	}
	next := first
	for i := 0; i < 4; i++ {
		next.history = append(next.history,
			client.Message{Role: "assistant", ToolCalls: []client.ToolCall{{
				ID: "search-" + string(rune('a'+i)), Type: "function",
				Function: client.FunctionCall{Name: tools.ToolSearchCode, Arguments: `{"query":"q","path":"."}`},
			}}},
			client.Message{Role: "tool", ToolCallID: "search-1", Name: tools.ToolSearchCode, Content: strings.Repeat("hit\n", 2000)},
		)
	}

	a := first.buildRequest().Messages
	b := next.buildRequest().Messages
	idx := 3 // system prompt, shell prompt, memory, then current user task
	if a[idx].Role != "user" || b[idx].Role != "user" {
		t.Fatalf("expected current user prompt at %d: first=%+v next=%+v", idx, a[idx], b[idx])
	}
	if a[idx].Content != task || b[idx].Content != task {
		t.Fatalf("current turn user prompt was trimmed or changed: first=%d next=%d want=%d", len(a[idx].Content), len(b[idx].Content), len(task))
	}
}

func TestWorkingStateIsBounded(t *testing.T) {
	m := model{app: state.New(), width: 80}
	for i := 0; i < maxWorkingStateItems+3; i++ {
		m.recordTool(searchCodeCall("query", "."), strings.Repeat("x", maxStateLineChars+80), false, true)
	}
	if m.workingState.Len() != maxWorkingStateItems {
		t.Fatalf("working state length = %d, want %d", m.workingState.Len(), maxWorkingStateItems)
	}
	for _, line := range m.workingState.Lines() {
		if len(line) > maxStateLineChars+3 {
			t.Fatalf("state line was not clipped: len=%d line=%q", len(line), line)
		}
	}
}
