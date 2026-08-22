package tui

import (
	"strings"
	"testing"

	"qeuro/internal/client"
	"qeuro/internal/state"
	"qeuro/internal/tools"
)

func readFileCall(path string) client.ToolCall {
	return client.ToolCall{ID: "read-1", Type: "function",
		Function: client.FunctionCall{Name: tools.ToolReadFile, Arguments: `{"path":"` + path + `"}`}}
}

func TestRunToolCallsWarnsNearLimitOnce(t *testing.T) {
	m := model{app: state.New(), width: 80, toolStep: maxToolSteps - toolLimitWarnSteps - 1}
	m.pendingCalls = []client.ToolCall{readFileCall("missing.txt")}

	res, cmd := m.runToolCalls()
	m = res.(model)
	if cmd == nil {
		t.Fatal("expected read-only tool command")
	}
	if !m.toolWarned {
		t.Fatal("expected near-limit warning flag")
	}
	if got := m.history[len(m.history)-1].Content; !strings.Contains(got, "TOOL LOOP WARNING") {
		t.Fatalf("missing warning in history: %q", got)
	}

	m.pendingCalls = []client.ToolCall{readFileCall("missing.txt")}
	res, _ = m.runToolCalls()
	m = res.(model)
	warnings := 0
	for _, msg := range m.history {
		if strings.Contains(msg.Content, "TOOL LOOP WARNING") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("warning count = %d, want 1", warnings)
	}
}

func TestRunToolCallsFinalizesWithoutToolsAtLimit(t *testing.T) {
	m := model{app: state.New(), width: 80, projectID: "project-abc", toolStep: maxToolSteps}
	m.pendingCalls = []client.ToolCall{readFileCall("missing.txt")}
	m.streamText = "I need one more tool"

	res, cmd := m.runToolCalls()
	m = res.(model)
	if cmd == nil {
		t.Fatal("expected final no-tools stream command")
	}
	if !m.toolFinal {
		t.Fatal("expected final no-tools mode")
	}
	if len(m.pendingCalls) != 0 || len(m.toolQueue) != 0 {
		t.Fatalf("pending tools were not cleared: pending=%d queue=%d", len(m.pendingCalls), len(m.toolQueue))
	}
	if got := m.history[len(m.history)-1].Content; !strings.Contains(got, "TOOL LOOP LIMIT") || !strings.Contains(got, "WITHOUT tool calls") {
		t.Fatalf("missing limit instruction: %q", got)
	}

	req := m.buildRequest()
	if req.Tools != nil {
		t.Fatal("final request must not advertise tools")
	}
}
