package agentcore

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Контракт протокола объявлен в двух местах по необходимости: образ агента
// собирается из одного каталога CLI, поэтому импортировать `domain/agentproto`
// из репозитория сервисов нельзя, не сломав сборку контейнера, который
// запускает headless. Дубль без проверки разошёлся бы, а расхождение здесь —
// это событие, которое хост не понимает: правка файла, не попавшая в панель
// Changes, или запуск без done.
//
// Поэтому тест читает файл контракта и сверяет значения. Он пропускается, если
// файла нет: в этом отдельном репозитории и внутри контейнера соседнего модуля
// не существует, и падение там означало бы «тест ищет не то», а не расхождение.
func TestEventKindsMatchSharedContract(t *testing.T) {
	path := filepath.Join("..", "..", "..", "domain", "agentproto", "agentproto.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("общий контракт недоступен (%v) — сборка вне репозитория", err)
	}
	src := string(data)

	shared := map[string]string{}
	re := regexp.MustCompile(`(?m)^\s*(Kind[A-Za-z]+|Done[A-Za-z]+)\s*=\s*"([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		shared[m[1]] = m[2]
	}
	if len(shared) == 0 {
		t.Fatalf("в %s не найдено ни одной константы — тест перестал что-либо проверять", path)
	}

	local := map[string]string{
		"KindToken":           KindToken,
		"KindAssistant":       KindAssistant,
		"KindStatus":          KindStatus,
		"KindToolCall":        KindToolCall,
		"KindFileWrite":       KindFileWrite,
		"KindCommand":         KindCommand,
		"KindApprovalRequest": KindApprovalRequest,
		"KindUsage":           KindUsage,
		"KindError":           KindError,
		"KindDone":            KindDone,
		"DoneOK":              DoneOK,
		"DoneCancelled":       DoneCancelled,
		"DoneError":           DoneError,
	}
	for name, want := range local {
		got, ok := shared[name]
		if !ok {
			t.Errorf("%s есть здесь, но нет в общем контракте", name)
			continue
		}
		if got != want {
			t.Errorf("%s = %q здесь и %q в общем контракте", name, want, got)
		}
	}

	// Обратное направление: kind, добавленный в контракт, здесь тоже нужен —
	// иначе агент не умеет то, что хост уже разбирает. KindPlan исключён
	// намеренно: цикл не эмитит отдельного события плана, план приходит текстом.
	for name := range shared {
		if name == "KindPlan" {
			continue
		}
		if _, ok := local[name]; !ok {
			t.Errorf("%s появился в общем контракте, но не объявлен здесь", name)
		}
	}
}

// ProtocolVersion тоже не должен разъезжаться: хост читает поле v.
func TestProtocolVersionMatchesSharedContract(t *testing.T) {
	path := filepath.Join("..", "..", "..", "domain", "agentproto", "agentproto.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("общий контракт недоступен (%v)", err)
	}
	m := regexp.MustCompile(`ProtocolVersion\s*=\s*(\d+)`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("ProtocolVersion не найден в общем контракте")
	}
	if m[1] != "1" || ProtocolVersion != 1 {
		t.Fatalf("версии разошлись: здесь %d, в контракте %s", ProtocolVersion, m[1])
	}
}

// Поля Event должны быть теми же: хост разбирает JSON по именам, поэтому
// переименованный тег — это молча потерянное значение (path у file_write,
// exit_code у command).
func TestEventJSONFieldsMatchSharedContract(t *testing.T) {
	path := filepath.Join("..", "..", "..", "domain", "agentproto", "agentproto.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("общий контракт недоступен (%v)", err)
	}
	shared := jsonTagsOfEvent(t, string(data))

	local, err := os.ReadFile("events.go")
	if err != nil {
		t.Fatalf("events.go: %v", err)
	}
	mine := jsonTagsOfEvent(t, string(local))

	for tag := range shared {
		if !mine[tag] {
			t.Errorf("поле %q есть в общем контракте, но не здесь", tag)
		}
	}
	for tag := range mine {
		if !shared[tag] {
			t.Errorf("поле %q есть здесь, но не в общем контракте", tag)
		}
	}
}

func jsonTagsOfEvent(t *testing.T, src string) map[string]bool {
	t.Helper()
	i := strings.Index(src, "type Event struct {")
	if i < 0 {
		t.Fatal("объявление type Event не найдено")
	}
	rest := src[i:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatal("объявление type Event не закрыто")
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`json:"([^",]+)`).FindAllStringSubmatch(rest[:end], -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("в type Event не найдено json-тегов")
	}
	return out
}
