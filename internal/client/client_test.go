package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRevokeToken(t *testing.T) {
	var saw bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = true
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/token/revoke" {
			t.Errorf("path = %s, want /v1/token/revoke", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer qeuro_live_test" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := New(srv.URL, "qeuro_live_test").RevokeToken(context.Background()); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if !saw {
		t.Fatal("server was not called")
	}
}

func TestParseSSEAccumulatesMultilineData(t *testing.T) {
	stream := "event: token\n" +
		"data: {\"text\":\n" +
		"data: \"hello\"}\n\n" +
		"event: done\n" +
		"data: {}\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}
	out := make(chan Event, 4)

	parseSSE(context.Background(), resp, out)

	var events []Event
	for ev := range out {
		events = append(events, ev)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(events), events)
	}
	if events[0].Kind != EventToken || events[0].Text != "hello" {
		t.Fatalf("first event = %+v, want token hello", events[0])
	}
	if events[1].Kind != EventDone {
		t.Fatalf("second event = %+v, want done", events[1])
	}
}
