package client_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"qeuro/internal/client"
)

// Runs only when QEURO_TEST_BACKEND_URL and QEURO_TEST_TOKEN are set. It drives
// the same client code the TUI uses against a live backend, verifying the SSE
// parsing path end to end:
//
//	QEURO_TEST_BACKEND_URL=http://localhost:8080 QEURO_TEST_TOKEN=qeuro_live_... \
//	  go test ./internal/client -run Integration -v
func TestIntegrationChatStream(t *testing.T) {
	base := os.Getenv("QEURO_TEST_BACKEND_URL")
	token := os.Getenv("QEURO_TEST_TOKEN")
	if base == "" || token == "" {
		t.Skip("QEURO_TEST_BACKEND_URL/QEURO_TEST_TOKEN not set; skipping client integration test")
	}
	model := os.Getenv("QEURO_TEST_MODEL")
	if model == "" {
		model = "deepseek-ai/deepseek-v4-flash"
	}

	c := client.New(base, token)

	// /v1/me should round-trip.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := c.Me(ctx); err != nil {
		t.Fatalf("Me: %v", err)
	}

	// Stream a chat.
	cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer ccancel()
	ch, err := c.Chat(cctx, client.ChatRequest{
		Mode:      "chat",
		Model:     model,
		Effort:    "low",
		ProjectID: "cli-it",
		Messages:  []client.Message{{Role: "user", Content: "Reply with exactly: pong"}},
	})
	if err != nil {
		t.Fatalf("Chat connect: %v", err)
	}

	var sawRoute, sawDone bool
	var text strings.Builder
	var usage *client.Usage
	for ev := range ch {
		switch ev.Kind {
		case client.EventRoute:
			sawRoute = true
		case client.EventToken:
			text.WriteString(ev.Text)
		case client.EventUsage:
			usage = ev.Usage
		case client.EventError:
			t.Fatalf("stream error: %s (%s)", ev.ErrMsg, ev.ErrCode)
		case client.EventDone:
			sawDone = true
		}
	}

	if !sawRoute {
		t.Error("never received a route event")
	}
	if !sawDone {
		t.Error("never received a done event")
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Error("no text streamed")
	}
	t.Logf("output=%q usage=%+v", text.String(), usage)
}
