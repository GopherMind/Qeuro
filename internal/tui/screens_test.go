package tui

import (
	"strings"
	"testing"

	"qeuro/internal/client"
	"qeuro/internal/state"
)

func TestHistoryScreenShowsSessionMessagesAndCompactsTools(t *testing.T) {
	longToolOutput := strings.Repeat("x", historyToolMaxRunes+80)
	out := historyScreen([]client.Message{
		{Role: "user", Content: "fix the auth bug"},
		{Role: "assistant", ToolCalls: []client.ToolCall{{
			Function: client.FunctionCall{Name: "read_file"},
		}}},
		{Role: "tool", Name: "read_file", Content: longToolOutput},
		{Role: "assistant", Content: "fixed and verified"},
	}, 80)

	for _, want := range []string{
		"Session History",
		"USER",
		"fix the auth bug",
		"QEURO",
		"requested tools: read_file",
		"TOOL",
		"read_file result:",
		"fixed and verified",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("history screen missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, longToolOutput) {
		t.Fatal("history screen should compact long tool output")
	}
}

func TestHistoryScreenHidesInternalQualityGate(t *testing.T) {
	out := historyScreen([]client.Message{
		{Role: "user", Content: "QUALITY GATE: run tests before finishing"},
		{Role: "system", Content: "internal"},
	}, 80)

	if strings.Contains(out, "QUALITY GATE") || strings.Contains(out, "internal") {
		t.Fatalf("history screen leaked internal messages:\n%s", out)
	}
	if !strings.Contains(out, "No visible conversation history") {
		t.Fatalf("history screen should show empty state:\n%s", out)
	}
}

func TestContextAndUsageScreensShowSplitTelemetry(t *testing.T) {
	app := state.New()
	app.CtxUsed = 12000
	app.CtxLimit = 200000
	app.MsgCount = 2
	app.Usage.RecordUsage(state.UsageRecord{
		InputTokens:       12000,
		OutputTokens:      900,
		CachedInputTokens: 3000,
		CostUSD:           0.0123,
		Credits:           0.615,
		SavedUSD:          0.0400,
		Balance:           42.5,
	})

	ctx := contextScreen(app, 80)
	for _, want := range []string{"Context Window", "turn input", "turn cache", "3,000", "9,000 billable"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context screen missing %q:\n%s", want, ctx)
		}
	}

	usage := usageScreen(app, 80)
	for _, want := range []string{"Usage", "LAST TURN", "$0.0123", "0.6 spent"} {
		if !strings.Contains(usage, want) {
			t.Fatalf("usage screen missing %q:\n%s", want, usage)
		}
	}
	for _, blocked := range []string{"saved", "balance"} {
		if strings.Contains(usage, blocked) {
			t.Fatalf("usage screen should not show %q:\n%s", blocked, usage)
		}
	}
}
