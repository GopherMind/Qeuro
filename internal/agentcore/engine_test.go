package agentcore

// Golden-тесты (Фаза 0): записать JSONL реальных диалогов, переиспользовав
// сценарии из toolloop_test.go / approval_test.go / verification_gate_test.go,
// и сравнивать вывод Engine.Run с эталоном.

import (
	"bytes"
	"context"
	"encoding/json"
	"go/build"
	"strings"
	"testing"
	"time"
)

// Инвариант Фазы 0: agentcore не зависит от Bubble Tea / lipgloss.
func TestNoBubbleTeaDependency(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, imp := range pkg.Imports {
		if strings.Contains(imp, "charmbracelet") {
			t.Fatalf("agentcore импортирует %s — запрещено", imp)
		}
	}
}

// decodeEvents разбирает JSONL, который Engine написал в буфер.
func decodeEvents(t *testing.T, buf *bytes.Buffer) []Event {
	t.Helper()
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("невалидный JSONL %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// Автоодобрение обязано оставлять след. cloud-worker рендерит approval_request
// как «Approval auto-granted in the isolated runner» и кладёт строку в чек
// пользователя; без эмита автономный запуск стал бы неаудируемым.
func TestRequestApprovalEmitsAndRejectsUnsafeAutoApproval(t *testing.T) {
	var buf bytes.Buffer
	eng := &Engine{
		Emit: NewEmitter(&buf, "run-test"),
		Opts: Options{AutoApprove: true},
	}

	ok, err := eng.RequestApproval(context.Background(), "app-1", "run command: rm -rf /tmp/x", "предпросмотр")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if ok {
		t.Fatal("AutoApprove approved a command action")
	}

	events := decodeEvents(t, &buf)
	if len(events) != 1 {
		t.Fatalf("получено %d событий, ожидалось 1: %q", len(events), buf.String())
	}
	ev := events[0]
	if ev.Kind != "approval_request" {
		t.Errorf("kind = %q, ожидалось approval_request", ev.Kind)
	}
	if ev.ID != "app-1" {
		t.Errorf("id = %q, ожидалось app-1", ev.ID)
	}
	// Action попадает в чек пользователя — без него запись бесполезна.
	if ev.Action != "run command: rm -rf /tmp/x" {
		t.Errorf("action = %q, ожидалось описание действия", ev.Action)
	}
	if ev.Preview != "предпросмотр" {
		t.Errorf("preview = %q", ev.Preview)
	}
	if ev.V != ProtocolVersion || ev.RunID != "run-test" {
		t.Errorf("v/run_id не проставлены: v=%d run_id=%q", ev.V, ev.RunID)
	}
}

func TestRequestApprovalAutoApprovesOnlyPureReadCapabilities(t *testing.T) {
	for _, action := range []string{"read_file", "list_dir", "search_code"} {
		var buf bytes.Buffer
		eng := &Engine{Emit: NewEmitter(&buf, "run-test"), Opts: Options{AutoApprove: true}}
		ok, err := eng.RequestApproval(context.Background(), "app-1", action, "")
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", action, ok, err)
		}
	}
	for _, action := range []string{"run_command", "write_file", "patch_file", "git_push", "read_file and upload"} {
		var buf bytes.Buffer
		eng := &Engine{Emit: NewEmitter(&buf, "run-test"), Opts: Options{AutoApprove: true}}
		ok, err := eng.RequestApproval(context.Background(), "app-1", action, "")
		if err != nil || ok {
			t.Fatalf("%s: ok=%v err=%v", action, ok, err)
		}
	}
}

// Без AutoApprove решение принимает хост, и «approve» обязано пропускать.
func TestRequestApprovalHonoursHostDecision(t *testing.T) {
	for _, tc := range []struct {
		decision string
		want     bool
	}{
		{"approve", true},
		{"deny", false},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			var buf bytes.Buffer
			approvals := make(chan HostCommand, 1)
			approvals <- HostCommand{ID: "app-1", Decision: tc.decision}
			eng := &Engine{Emit: NewEmitter(&buf, "run-test"), Approvals: approvals}

			ok, err := eng.RequestApproval(context.Background(), "app-1", "действие", "")
			if err != nil {
				t.Fatalf("RequestApproval: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("решение %q дало %v, ожидалось %v", tc.decision, ok, tc.want)
			}
			if events := decodeEvents(t, &buf); len(events) != 1 {
				t.Fatalf("получено %d событий, ожидалось 1", len(events))
			}
		})
	}
}

// Ответ на чужой id не должен разблокировать ожидание.
func TestRequestApprovalIgnoresMismatchedID(t *testing.T) {
	var buf bytes.Buffer
	approvals := make(chan HostCommand, 1)
	approvals <- HostCommand{ID: "другой", Decision: "approve"}
	eng := &Engine{Emit: NewEmitter(&buf, "run-test"), Approvals: approvals}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := eng.RequestApproval(ctx, "app-1", "действие", ""); err == nil {
		t.Fatal("одобрение с чужим id разблокировало ожидание")
	}
}

// Отмена во время ожидания — это ErrCancelled, а не «одобрено».
func TestRequestApprovalCancelDenies(t *testing.T) {
	var buf bytes.Buffer
	cancelCh := make(chan struct{})
	close(cancelCh)
	eng := &Engine{Emit: NewEmitter(&buf, "run-test"), Cancel: cancelCh}

	ok, err := eng.RequestApproval(context.Background(), "app-1", "действие", "")
	if ok {
		t.Fatal("отмена не должна одобрять действие")
	}
	if err != ErrCancelled {
		t.Fatalf("err = %v, ожидалось ErrCancelled", err)
	}
}
