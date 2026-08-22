package tools

import (
	"encoding/json"
	"testing"
)

// Характеризация: этот файл написан ДО перевода политик инструментов на реестр
// (roadmap §4.8) и фиксирует наблюдаемое поведение восьми встроенных тулов
// побайтово. Смысл именно в этом — существующие TestRequiresApproval и
// TestDefinitionsStayCompact проверяют *свойства* («patch_file требует
// одобрения», «схема не разрослась»), поэтому они остались бы зелёными, если
// реестр поменял бы порядок тулов, текст описания или форму схемы. Такие
// изменения не ломают тест, но ломают промпт: определения едут модели в каждом
// запросе, и правка описания меняет её поведение.
//
// Поэтому здесь не «разумные утверждения», а слепок. Если он падает после
// рефакторинга — либо изменение непреднамеренное, либо ожидаемое значение надо
// обновить осознанно, одной правкой, видимой в дифе.

// wantDefinitions — точный JSON, который Definitions() отдаёт сегодня.
// Порядок ключей задан json.Marshal (map сортируется по ключу), порядок тулов —
// порядком литерала в Definitions().
const wantDefinitions = `[{"function":{"description":"Read a relevant project file before editing.","name":"read_file","parameters":{"properties":{"path":{"type":"string"}},"required":["path"],"type":"object"}},"type":"function"},` +
	`{"function":{"description":"Inspect project tree; empty path is root.","name":"list_dir","parameters":{"properties":{"path":{"type":"string"}},"type":"object"}},"type":"function"},` +
	`{"function":{"description":"Apply a minimal targeted replacement.","name":"patch_file","parameters":{"properties":{"new_content":{"type":"string"},"old_content":{"type":"string"},"path":{"type":"string"}},"required":["path","old_content","new_content"],"type":"object"}},"type":"function"},` +
	`{"function":{"description":"Create a new file only; use patch_file for existing files.","name":"write_file","parameters":{"properties":{"content":{"type":"string"},"path":{"type":"string"}},"required":["path","content"],"type":"object"}},"type":"function"},` +
	`{"function":{"description":"Find symbols/errors before guessing; returns file:line hits.","name":"search_code","parameters":{"properties":{"path":{"type":"string"},"query":{"type":"string"}},"required":["query"],"type":"object"}},"type":"function"},` +
	`{"function":{"description":"Shell for focused facts, search, build/test/lint, formatting.","name":"run_command","parameters":{"properties":{"command":{"type":"string"}},"required":["command"],"type":"object"}},"type":"function"},` +
	`{"function":{"description":"Save one durable project fact.","name":"remember","parameters":{"properties":{"category":{"type":"string"},"note":{"type":"string"}},"required":["category","note"],"type":"object"}},"type":"function"},` +
	`{"function":{"description":"Read project memory; category optional.","name":"recall","parameters":{"properties":{"category":{"type":"string"}},"type":"object"}},"type":"function"}]`

func TestCharacterizationDefinitionsAreByteStable(t *testing.T) {
	if got := string(Definitions()); got != wantDefinitions {
		t.Errorf("Definitions() изменился.\n got: %s\nwant: %s", got, wantDefinitions)
	}
}

// TestCharacterizationDefinitionsCoverEveryBuiltin связывает список имён с
// тем, что реально предлагается модели. Без этой проверки тул можно было бы
// удалить из Definitions(), оставив константу и ветку в Execute: он перестал бы
// существовать для модели, а все остальные тесты остались бы зелёными.
func TestCharacterizationDefinitionsCoverEveryBuiltin(t *testing.T) {
	var defs []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(Definitions(), &defs); err != nil {
		t.Fatalf("definitions JSON: %v", err)
	}
	advertised := make(map[string]bool, len(defs))
	for _, d := range defs {
		advertised[d.Function.Name] = true
	}
	for _, name := range builtinNamesForTest {
		if !advertised[name] {
			t.Errorf("%s не предлагается модели в Definitions()", name)
		}
	}
	if len(advertised) != len(builtinNamesForTest) {
		t.Errorf("Definitions() отдаёт %d тулов, встроенных имён %d", len(advertised), len(builtinNamesForTest))
	}
}

// builtinNamesForTest — все восемь имён. Отдельный список, а не срез из
// продакшн-кода: иначе тест сравнивал бы реестр сам с собой и обе величины
// уменьшались бы вместе (эта ошибка уже случалась в §8, см.
// TestEveryResolvedSettingIsReportable в clientcfg).
var builtinNamesForTest = []string{
	ToolReadFile, ToolListDir, ToolSearchCode, ToolPatchFile,
	ToolWriteFile, ToolRunCommand, ToolRemember, ToolRecall,
}

// TestCharacterizationPolicyMatrix фиксирует политику для каждого имени целиком,
// а не по трём примерам. Пробел, который это закрывает, конкретен: сегодня
// Mutating(remember) == false и RequiresApproval(remember) == false, то есть
// remember исполняется без одобрения — и после перехода на реестр это должно
// остаться ровно таким же, иначе изменится поведение, которого §4.8 не касается.
func TestCharacterizationPolicyMatrix(t *testing.T) {
	cases := []struct {
		name     string
		mutating bool
		approval bool
	}{
		{ToolReadFile, false, false},
		{ToolListDir, false, false},
		{ToolSearchCode, false, false},
		{ToolPatchFile, true, true},
		{ToolWriteFile, true, true},
		{ToolRunCommand, false, true}, // не Mutating: команда не правит файлы через undo-стек
		{ToolRemember, false, false},
		{ToolRecall, false, false},
	}
	if len(cases) != len(builtinNamesForTest) {
		t.Fatalf("матрица покрывает %d имён из %d", len(cases), len(builtinNamesForTest))
	}
	for _, c := range cases {
		if got := Mutating(c.name); got != c.mutating {
			t.Errorf("Mutating(%s) = %v, want %v", c.name, got, c.mutating)
		}
		if got := RequiresApproval(c.name); got != c.approval {
			t.Errorf("RequiresApproval(%s) = %v, want %v", c.name, got, c.approval)
		}
	}
}

// TestUnknownNameFailsClosed фиксирует единственное намеренное изменение
// поведения при переходе на реестр.
//
// До реестра RequiresApproval("что угодно") == false, потому что функция была
// замкнута на восьми константах. То есть незнакомое имя — MCP-тул или имя,
// которое модель просто придумала, — исполнилось бы без одобрения. Первая
// редакция этого теста утверждала старое (неверное) значение и упала при
// переводе на реестр; это и было её задачей — сделать смену поведения видимой
// правкой, а не молчаливым сдвигом.
//
// Mutating остаётся false: незнакомому имени нельзя приписать ✎, потому что
// глиф обещает откат через undo-стек, которого для чужого эффекта нет.
func TestUnknownNameFailsClosed(t *testing.T) {
	for _, name := range []string{
		"mcp__github__create_issue", // MCP-имя, сервер не зарегистрирован
		"delete_everything",         // имя, которого не существует вовсе
		"",                          // пустое имя
	} {
		if !RequiresApproval(name) {
			t.Errorf("RequiresApproval(%q) = false, незнакомое имя обязано требовать одобрения", name)
		}
		if Mutating(name) {
			t.Errorf("Mutating(%q) = true, want false", name)
		}
		if Known(name) {
			t.Errorf("Known(%q) = true, want false", name)
		}
	}
}

// TestCharacterizationSummaryPerName фиксирует строку, которую видит человек в
// TUI, включая подстановки по умолчанию (list_dir без пути → ".", recall без
// категории → "memory: digest") и поведение при неизвестном имени.
func TestCharacterizationSummaryPerName(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{ToolReadFile, `{"path":"main.go"}`, "reading main.go"},
		{ToolListDir, `{"path":"internal"}`, "listing internal"},
		{ToolListDir, `{}`, "listing ."},
		{ToolSearchCode, `{"query":"Foo"}`, "searching «Foo»"},
		{ToolPatchFile, `{"path":"a.go"}`, "editing a.go"},
		{ToolWriteFile, `{"path":"b.go"}`, "writing b.go"},
		{ToolRunCommand, `{"command":"go test ./..."}`, "$ go test ./..."},
		{ToolRemember, `{"category":"build"}`, "remember → build"},
		{ToolRemember, `{}`, "remember → notes"},
		{ToolRecall, `{"category":"build"}`, "memory: build"},
		{ToolRecall, `{}`, "memory: digest"},
		{"mcp__github__search", `{}`, "mcp__github__search"},
		// Битый JSON не должен ронять UI: аргументы игнорируются, имя остаётся.
		{ToolReadFile, `{`, "reading "},
	}
	for _, c := range cases {
		if got := Summary(c.name, c.args); got != c.want {
			t.Errorf("Summary(%s, %s) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}
