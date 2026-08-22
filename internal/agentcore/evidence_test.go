package agentcore

import (
	"strings"
	"testing"

	"qeuro/internal/agentloop"
	"qeuro/internal/client"
	"qeuro/internal/tools"
)

// Тесты на литеральный вывод команды в событии протокола (roadmap-v3 §5.1).
//
// Панель Evidence в консоли утверждает то, что читатель проверить не может: что
// команда выполнялась и что она напечатала. Каждый тест ниже закрывает один способ
// сделать это утверждение ложным — выдать причину отказа за вывод, обрезать вывод
// по первой строке (падение теста печатается ниже неё), или пропустить в текст
// управляющую последовательность, которая перерисует блок, где она отображается.

// runCommandResult собирает результат в формате runCommand.
func runCommandResult(status, output string) string {
	return status + "\n" + tools.CommandOutputSeparator + "\n" + output
}

// commandCall — вызов run_command с заданной командой.
func commandCall(cmd string) client.ToolCall {
	return client.ToolCall{
		ID: "call_1",
		Function: client.FunctionCall{
			Name:      tools.ToolRunCommand,
			Arguments: `{"command":"` + cmd + `"}`,
		},
	}
}

func TestCommandEventCarriesWholeOutputNotFirstLine(t *testing.T) {
	// Статус «failed» стоит первой строкой, а причина падения — ниже. Text из одной
	// строки означал бы панель Evidence, в которой не видно, что именно упало.
	result := runCommandResult("failed: exit status 1", strings.Join([]string{
		"--- FAIL: TestBilling (0.01s)",
		"    billing_test.go:42: balance = 0, want 100",
		"FAIL\tqeuro/webapi/internal/billing\t0.312s",
	}, "\n"))

	ev := stepEvent(commandCall("go test ./..."), result, false, true)

	if ev.Kind != KindCommand {
		t.Fatalf("kind = %q, want %q", ev.Kind, KindCommand)
	}
	if !strings.Contains(ev.Text, "balance = 0, want 100") {
		t.Fatalf("в Text нет причины падения: %q", ev.Text)
	}
	if !strings.Contains(ev.Text, "FAIL\tqeuro/webapi/internal/billing") {
		t.Fatalf("в Text нет итоговой строки: %q", ev.Text)
	}
	if strings.Contains(ev.Text, tools.CommandOutputSeparator) {
		t.Fatalf("разделитель попал в Text: %q", ev.Text)
	}
	if strings.Contains(ev.Text, "failed: exit status 1") {
		t.Fatalf("строка статуса попала в вывод: %q", ev.Text)
	}
	if ev.ExitCode == nil || *ev.ExitCode != 1 {
		t.Fatalf("exit code = %v, want 1", ev.ExitCode)
	}
}

func TestCommandEventKeepsSuccessOutput(t *testing.T) {
	result := runCommandResult(tools.CommandOKPrefix, "ok  \tqeuro/webapi\t1.204s\n")

	ev := stepEvent(commandCall("go test ./..."), result, false, true)

	if !strings.Contains(ev.Text, "ok  \tqeuro/webapi") {
		t.Fatalf("Text = %q", ev.Text)
	}
	if ev.ExitCode == nil || *ev.ExitCode != 0 {
		t.Fatalf("exit code = %v, want 0", ev.ExitCode)
	}
}

func TestRejectedCommandHasNoOutputAndNoExitCode(t *testing.T) {
	// Отказ политики формирует не Runner: разделителя в результате нет, и первая
	// строка — это причина отказа. Выдать её за вывод команды значит показать в
	// панели шаг, которого не было; проставить exit code — показать его успешным.
	const refusal = "command rejected by security policy: rm is not allowed"

	ev := stepEvent(commandCall("rm -rf /"), refusal, false, false)

	if ev.Kind != KindCommand {
		t.Fatalf("kind = %q", ev.Kind)
	}
	if ev.Text != refusal {
		t.Fatalf("Text = %q, want причину отказа как есть", ev.Text)
	}
	if ev.ExitCode != nil {
		t.Fatalf("exit code = %d у команды, которая не выполнялась", *ev.ExitCode)
	}
	if ev.Cmd != "" {
		// Пустой Cmd — это контракт: cloud-worker строит evidence только когда Cmd
		// заполнен, иначе отклонённая команда попала бы в панель как выполненный шаг.
		t.Fatalf("Cmd = %q у отклонённой команды", ev.Cmd)
	}
}

func TestNotRunResultNeverYieldsAnExitCodeOrOutput(t *testing.T) {
	// ran=false покрывает не только отказ политики: сюда попадают результаты, текст
	// которых похож на вывод exec. exitCodeOf ищет «exit status N» в первой строке, а
	// commandOutputOf — разделитель, и оба могут найтись в тексте, который сформировал
	// не Runner. Без проверки ran такой результат превратится в шаг, «выполненный» с
	// кодом возврата — то есть в панели Evidence появится проверка, которой не было.
	cases := map[string]string{
		"отказ политики":   "command rejected by security policy: rm is not allowed",
		"нет раннера":      "error: tool runner is not available",
		"хук заблокировал": "pre-commit hook blocked the command",
		"хук упал":         "pre-commit hook failed: exit status 1",
		"изолированное дерево": "error: commands are unavailable in an isolated worktree; " +
			"the integration step runs the build and tests once, in the project tree",
		"текст с разделителем": "pre-commit hook failed: exit status 2\n" +
			tools.CommandOutputSeparator + "\nсодержимое, которое команда не печатала",
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			ev := stepEvent(commandCall("git commit -m x"), result, false, false)

			if ev.ExitCode != nil {
				t.Fatalf("exit code = %d у команды, которая не выполнялась", *ev.ExitCode)
			}
			// Text для !ran — это первая строка результата, то есть причина. Вывода
			// после разделителя здесь быть не должно: его напечатала не команда.
			want := agentloop.FirstResultLine(result)
			if ev.Text != want {
				t.Fatalf("Text = %q, want причину (%q)", ev.Text, want)
			}
			if strings.Contains(ev.Text, tools.CommandOutputSeparator) ||
				strings.Contains(ev.Text, "команда не печатала") {
				t.Fatalf("в Text попал текст после разделителя: %q", ev.Text)
			}
		})
	}
}

func TestCommandOutputOfIgnoresASeparatorPrintedByTheCommand(t *testing.T) {
	// Команда может напечатать ту же строку сама (например `cat` файла этого теста).
	// Разделителем считается только первая строка, равная константе — иначе вывод
	// команды подменил бы границу и часть вывода потерялась бы.
	out := "line one\n" + tools.CommandOutputSeparator + "\nline two"
	result := runCommandResult(tools.CommandOKPrefix, out)

	got := commandOutputOf(result)

	if got != out {
		t.Fatalf("commandOutputOf = %q, want %q", got, out)
	}
}

func TestCommandOutputOfReturnsEmptyWithoutASeparator(t *testing.T) {
	for _, in := range []string{
		"",
		"command rejected by security policy: rm is not allowed",
		"error: tool runner is not available",
		tools.CommandOKPrefix,
		"prefix " + tools.CommandOutputSeparator + " suffix", // не отдельная строка
	} {
		if got := commandOutputOf(in); got != "" {
			t.Fatalf("commandOutputOf(%q) = %q, want \"\"", in, got)
		}
	}
}

func TestCommandOutputOfBoundsTheOutput(t *testing.T) {
	long := strings.Repeat("щ", maxCommandEvidence+500)
	got := commandOutputOf(runCommandResult(tools.CommandOKPrefix, long))

	if n := len([]rune(got)); n != maxCommandEvidence {
		t.Fatalf("оставлено %d рун, want %d", n, maxCommandEvidence)
	}
	// Резать по рунам, а не по байтам: срез по байтам даёт битый UTF-8, а JSONL с
	// битой строкой хост уже не разберёт.
	if !strings.HasPrefix(long, got) {
		t.Fatal("обрезка исказила вывод")
	}
}

func TestCommandOutputIsNeutralizedBeforeTheProtocol(t *testing.T) {
	// Десктоп пишет Text прямо в xterm (TerminalPane), консоль — в блок Evidence.
	// Последовательность вроде \x1b[1A\x1b[2K перемещает курсор и стирает строку:
	// вывод, «доказывающий» успех теста, может быть напечатан выводом, который его
	// лишь заявляет.
	result := runCommandResult("failed: exit status 1",
		"\x1b[31mFAIL\x1b[0m\tpkg\n\x1b[1A\x1b[2KPASS\tpkg\n\tкейс\tсохранён\n")

	ev := stepEvent(commandCall("go test ./..."), result, false, true)

	if strings.Contains(ev.Text, "\x1b") {
		t.Fatalf("escape дошёл до протокола: %q", ev.Text)
	}
	// \n и \t остаются: без них многострочный вывод перестаёт быть читаемым, а
	// именно читаемость — причина показывать литеральный вывод.
	if !strings.Contains(ev.Text, "\n") || !strings.Contains(ev.Text, "\t") {
		t.Fatalf("Text потерял структуру: %q", ev.Text)
	}
	if !strings.Contains(ev.Text, "кейс") {
		t.Fatalf("Text потерял не-ASCII текст: %q", ev.Text)
	}
	for _, r := range ev.Text {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			t.Fatalf("управляющая руна %U в %q", r, ev.Text)
		}
	}
}

func TestFileWriteEventStillCarriesFirstLineOnly(t *testing.T) {
	// Вывод целиком — только у команд. У правки файла Text остаётся сводкой: панель
	// Changes показывает path/diff, и вываливать туда содержимое файла незачем.
	call := client.ToolCall{
		ID: "call_2",
		Function: client.FunctionCall{
			Name:      "write_file",
			Arguments: `{"path":"x.go","content":"package x\n"}`,
		},
	}
	result := "wrote x.go\nsecond line"

	ev := stepEvent(call, result, true, true)

	if ev.Kind != KindFileWrite {
		t.Fatalf("kind = %q, want %q", ev.Kind, KindFileWrite)
	}
	if strings.Contains(ev.Text, "second line") {
		t.Fatalf("Text = %q, want только первую строку", ev.Text)
	}
}
