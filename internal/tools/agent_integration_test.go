package tools_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"qeuro/internal/client"
	"qeuro/internal/tools"
)

// Drives the full agentic loop (model → tool_calls → local execution →
// continue) against a live backend, verifying the model can read and edit a
// real file via tools. Runs only when the backend env vars are set:
//
//	QEURO_TEST_BACKEND_URL=http://localhost:8080 QEURO_TEST_TOKEN=qeuro_live_... \
//	QEURO_TEST_SCRATCH=/tmp/qeuro-scratch \
//	  go test ./internal/tools -run IntegrationAgent -v
func TestIntegrationAgentToolLoop(t *testing.T) {
	base := os.Getenv("QEURO_TEST_BACKEND_URL")
	token := os.Getenv("QEURO_TEST_TOKEN")
	scratch := os.Getenv("QEURO_TEST_SCRATCH")
	if base == "" || token == "" || scratch == "" {
		t.Skip("backend/scratch env not set; skipping agent integration test")
	}
	model := os.Getenv("QEURO_TEST_MODEL")
	if model == "" {
		model = "deepseek-ai/deepseek-v4-flash"
	}

	runner, err := tools.NewRunner(scratch)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	c := client.New(base, token)

	history := []client.Message{
		{Role: "system", Content: "You are a coding agent with file tools. Use list_dir and read_file to inspect the project, then patch_file to make edits. The project root contains main.go."},
		{Role: "user", Content: "В файле main.go поменяй порт с :8080 на :9090, используя инструмент patch_file. Сначала прочитай файл."},
	}

	const maxSteps = 12
	var lastText string

	for step := 0; step < maxSteps; step++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		ch, err := c.Chat(ctx, client.ChatRequest{
			Mode:      "chat",
			Model:     model,
			Effort:    "low",
			ProjectID: "agent-it",
			Messages:  history,
			Tools:     tools.Definitions(),
		})
		if err != nil {
			cancel()
			t.Fatalf("chat connect (step %d): %v", step, err)
		}

		var text strings.Builder
		var calls []client.ToolCall
		for ev := range ch {
			switch ev.Kind {
			case client.EventToken:
				text.WriteString(ev.Text)
			case client.EventToolCalls:
				calls = ev.ToolCalls
			case client.EventError:
				cancel()
				t.Fatalf("stream error (step %d): %s", step, ev.ErrMsg)
			}
		}
		cancel()
		lastText = text.String()

		if len(calls) == 0 {
			t.Logf("final answer (step %d): %q", step, lastText)
			break
		}

		// Record assistant tool-call turn, execute locally, append results.
		history = append(history, client.Message{Role: "assistant", Content: text.String(), ToolCalls: calls})
		for _, tc := range calls {
			res, mutated := runner.Execute(tc.Function.Name, tc.Function.Arguments)
			t.Logf("step %d tool=%s args=%s mutated=%v -> %s", step, tc.Function.Name, tc.Function.Arguments, mutated, truncate(res, 80))
			history = append(history, client.Message{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: res,
			})
		}
	}

	// Verify the file was actually modified on disk.
	data, err := os.ReadFile(scratch + "/main.go")
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, ":9090") {
		t.Fatalf("expected port :9090 in file, got:\n%s", got)
	}
	if strings.Contains(got, ":8080") {
		t.Fatalf("old port :8080 still present:\n%s", got)
	}
	t.Logf("SUCCESS — file now contains :9090")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
