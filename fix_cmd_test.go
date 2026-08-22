package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGetLastFailedCommand_Windows проверяет чтение истории PowerShell на Windows.
func TestGetLastFailedCommand_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only test")
	}

	// getLastFailedCommandWindows использует os.UserHomeDir(), который читает
	// переменную окружения USERPROFILE на Windows. Подменяем её.
	tmpDir := t.TempDir()
	oldProfile := os.Getenv("USERPROFILE")
	os.Setenv("USERPROFILE", tmpDir)
	t.Cleanup(func() { os.Setenv("USERPROFILE", oldProfile) })

	// Проверяем, что функция возвращает ошибку при отсутствующем файле истории
	_, _, err := getLastFailedCommand()
	if err == nil {
		t.Error("expected error when history file does not exist")
	}

	// Создаём фиктивную историю с командами
	histPath := filepath.Join(tmpDir, "AppData", "Roaming", "Microsoft", "Windows", "PowerShell", "PSReadLine")
	if err := os.MkdirAll(histPath, 0700); err != nil {
		t.Fatal(err)
	}
	histFile := filepath.Join(histPath, "ConsoleHost_history.txt")
	content := "ls\necho test\n# comment line\ngit status\n"
	if err := os.WriteFile(histFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cmd, out, err := getLastFailedCommand()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "git status" {
		t.Errorf("expected last command 'git status', got %q", cmd)
	}
	if out != "" {
		t.Errorf("Windows shell history should not provide output, got %q", out)
	}
}

// TestGetLastFailedCommand_Unix проверяет чтение bash/zsh/fish истории на Unix.
func TestGetLastFailedCommand_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}

	oldHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	// Тест 1: bash history (.bash_history)
	bashHist := filepath.Join(tmpDir, ".bash_history")
	bashContent := "cd /tmp\nls -la\nmake build\n"
	if err := os.WriteFile(bashHist, []byte(bashContent), 0600); err != nil {
		t.Fatal(err)
	}

	cmd, out, err := getLastFailedCommand()
	if err != nil {
		t.Fatalf("bash history read failed: %v", err)
	}
	if cmd != "make build" {
		t.Errorf("bash: expected 'make build', got %q", cmd)
	}
	if out != "" {
		t.Errorf("shell history should not provide output, got %q", out)
	}

	// Тест 2: zsh extended history (.zsh_history)
	os.Remove(bashHist)
	zshHist := filepath.Join(tmpDir, ".zsh_history")
	// zsh extended format: : timestamp:elapsed;command
	zshContent := ": 1234567890:0;git clone repo\n: 1234567891:0;npm install\n"
	if err := os.WriteFile(zshHist, []byte(zshContent), 0600); err != nil {
		t.Fatal(err)
	}

	cmd, _, err = getLastFailedCommand()
	if err != nil {
		t.Fatalf("zsh history read failed: %v", err)
	}
	if cmd != "npm install" {
		t.Errorf("zsh: expected 'npm install', got %q", cmd)
	}
	// Убеждаемся, что не возвращается timestamp
	if strings.Contains(cmd, ":") || strings.Contains(cmd, "1234567891") {
		t.Errorf("zsh: returned timestamp/metadata instead of command: %q", cmd)
	}

	// Тест 3: fish history (YAML format)
	os.Remove(zshHist)
	fishDir := filepath.Join(tmpDir, ".local", "share", "fish")
	if err := os.MkdirAll(fishDir, 0700); err != nil {
		t.Fatal(err)
	}
	fishHist := filepath.Join(fishDir, "fish_history")
	fishContent := "- cmd: git add .\n  when: 1234567890\n- cmd: git commit -m test\n  when: 1234567891\n"
	if err := os.WriteFile(fishHist, []byte(fishContent), 0600); err != nil {
		t.Fatal(err)
	}

	cmd, _, err = getLastFailedCommand()
	if err != nil {
		t.Fatalf("fish history read failed: %v", err)
	}
	if cmd != "git commit -m test" {
		t.Errorf("fish: expected 'git commit -m test', got %q", cmd)
	}
}

// TestGetLastFailedCommand_BashOrderSensitivity проверяет правильность выбора последней команды
func TestGetLastFailedCommand_BashOrderSensitivity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash history test requires Unix environment")
	}

	oldHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	bashHist := filepath.Join(tmpDir, ".bash_history")
	// Три разные команды - должна вернуться последняя
	bashContent := "first command\nsecond command\nthird command\n"
	if err := os.WriteFile(bashHist, []byte(bashContent), 0600); err != nil {
		t.Fatal(err)
	}

	cmd, _, err := getLastFailedCommand()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "third command" {
		t.Errorf("expected last command 'third command', got %q (possible reversed iteration)", cmd)
	}
}

// TestGetLastFailedCommand_EmptyHistory проверяет поведение при пустой истории.
func TestGetLastFailedCommand_EmptyHistory(t *testing.T) {
	tmpDir := t.TempDir()

	var oldEnv, envKey string
	if runtime.GOOS == "windows" {
		envKey = "USERPROFILE"
		oldEnv = os.Getenv("USERPROFILE")
		histPath := filepath.Join(tmpDir, "AppData", "Roaming", "Microsoft", "Windows", "PowerShell", "PSReadLine")
		os.MkdirAll(histPath, 0700)
		os.WriteFile(filepath.Join(histPath, "ConsoleHost_history.txt"), []byte(""), 0600)
	} else {
		envKey = "HOME"
		oldEnv = os.Getenv("HOME")
		os.WriteFile(filepath.Join(tmpDir, ".bash_history"), []byte(""), 0600)
	}

	os.Setenv(envKey, tmpDir)
	t.Cleanup(func() { os.Setenv(envKey, oldEnv) })

	_, _, err := getLastFailedCommand()
	if err == nil {
		t.Error("expected error for empty history file")
	}
	if !strings.Contains(err.Error(), "empty") && !strings.Contains(err.Error(), "no commands") && !strings.Contains(err.Error(), "no PowerShell history") {
		t.Errorf("expected 'empty' or 'no commands' or 'no PowerShell history' in error, got: %v", err)
	}
}

// TestGetLastFailedCommand_WhitespaceOnly проверяет обработку строк с пробелами.
func TestGetLastFailedCommand_WhitespaceOnly(t *testing.T) {
	tmpDir := t.TempDir()

	var oldEnv, envKey, histFile string
	if runtime.GOOS == "windows" {
		envKey = "USERPROFILE"
		oldEnv = os.Getenv("USERPROFILE")
		histPath := filepath.Join(tmpDir, "AppData", "Roaming", "Microsoft", "Windows", "PowerShell", "PSReadLine")
		os.MkdirAll(histPath, 0700)
		histFile = filepath.Join(histPath, "ConsoleHost_history.txt")
	} else {
		envKey = "HOME"
		oldEnv = os.Getenv("HOME")
		histFile = filepath.Join(tmpDir, ".bash_history")
	}

	os.Setenv(envKey, tmpDir)
	t.Cleanup(func() { os.Setenv(envKey, oldEnv) })

	// История с пустыми строками и пробелами
	content := "   \n\t\n  ls  \n"
	if err := os.WriteFile(histFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cmd, _, err := getLastFailedCommand()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "ls" {
		t.Errorf("expected trimmed 'ls', got %q", cmd)
	}
}

// TestGetLastFailedCommand_ZshExtendedFormat проверяет парсинг zsh extended_history.
func TestGetLastFailedCommand_ZshExtendedFormat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}

	tmpDir := t.TempDir()

	oldEnv := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	t.Cleanup(func() { os.Setenv("HOME", oldEnv) })

	zshFile := filepath.Join(tmpDir, ".zsh_history")
	content := ": 1609459200:0;ls -la\n: 1609459300:5;git commit -m 'test'\n"
	if err := os.WriteFile(zshFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cmd, _, err := getLastFailedCommand()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "git commit -m 'test'" {
		t.Errorf("expected 'git commit -m 'test'', got %q", cmd)
	}
	if strings.Contains(cmd, ":") || strings.Contains(cmd, "1609459300") {
		t.Errorf("zsh timestamp metadata should be stripped, got %q", cmd)
	}
}

// TestParseFixArgs проверяет, что неизвестный аргумент — ошибка, а не игнор.
// Опечатка в имени флага, молча превратившаяся в «отправить без спроса», —
// именно тот отказ, ради предотвращения которого флаг существует.
func TestParseFixArgs(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		wantYes   bool
		wantLocal bool
		wantErr   bool
	}{
		{args: nil},
		{args: []string{"--yes"}, wantYes: true},
		{args: []string{"-y"}, wantYes: true},
		{args: []string{"--local"}, wantLocal: true},
		{args: []string{"--local", "--yes"}, wantYes: true, wantLocal: true},
		{args: []string{"--ys"}, wantErr: true},
		{args: []string{"--yes", "extra"}, wantErr: true},
		{args: []string{"-Y"}, wantErr: true},
	} {
		parsed, err := parseFixArgs(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseFixArgs(%q): expected error, got %+v", tc.args, parsed)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFixArgs(%q): unexpected error %v", tc.args, err)
			continue
		}
		if parsed.yes != tc.wantYes || parsed.local != tc.wantLocal {
			t.Errorf("parseFixArgs(%q) = %+v, want yes=%v local=%v", tc.args, parsed, tc.wantYes, tc.wantLocal)
		}
	}
}

// TestConfirmFixSend_DefaultsToNo закрепляет самое важное свойство гейта:
// умолчание — отказ. Отправка необратима (команда уходит в чужие логи), отказ
// стоит одного повторного нажатия.
func TestConfirmFixSend_DefaultsToNo(t *testing.T) {
	restore := fakeFixTerminal(t, true)
	defer restore()

	for _, in := range []string{"\n", "\r\n", "n\n", "N\n", "no\n", "  \n", "maybe\n", "yy\n", ""} {
		if confirmFixSend(false, strings.NewReader(in)) {
			t.Errorf("confirmFixSend(%q) = true, want false", in)
		}
	}
}

// TestConfirmFixSend_AcceptsYes проверяет, что явное согласие всё же работает —
// гейт, который нельзя пройти, это не гейт, а удалённая функция.
func TestConfirmFixSend_AcceptsYes(t *testing.T) {
	restore := fakeFixTerminal(t, true)
	defer restore()

	for _, in := range []string{"y\n", "Y\n", "yes\n", "YES\n", " y \n", "y"} {
		if !confirmFixSend(false, strings.NewReader(in)) {
			t.Errorf("confirmFixSend(%q) = false, want true", in)
		}
	}
}

// TestConfirmFixSend_NonTTYRefuses: в пайплайне вопроса никто не увидит, и
// «нет ответа» нельзя трактовать как согласие. Обойти можно только --yes.
func TestConfirmFixSend_NonTTYRefuses(t *testing.T) {
	restore := fakeFixTerminal(t, false)
	defer restore()

	if confirmFixSend(false, strings.NewReader("y\n")) {
		t.Error("non-TTY stdin must refuse even when the reader says yes")
	}
	if !confirmFixSend(true, strings.NewReader("")) {
		t.Error("--yes must bypass the gate on non-TTY stdin")
	}
}

// TestConfirmFixSend_YesSkipsPrompt: с --yes ввод не читается вообще, иначе
// неинтерактивный вызов съел бы байт из stdin, принадлежащий кому-то другому.
func TestConfirmFixSend_YesSkipsPrompt(t *testing.T) {
	restore := fakeFixTerminal(t, true)
	defer restore()

	r := strings.NewReader("n\n")
	if !confirmFixSend(true, r) {
		t.Fatal("--yes must return true")
	}
	if r.Len() != len("n\n") {
		t.Errorf("--yes must not consume stdin, %d bytes read", len("n\n")-r.Len())
	}
}

// fakeFixTerminal подменяет предикат TTY. Настоящий под `go test` всегда ложен,
// поэтому без шва ветка подтверждения была бы непроверяемой в принципе.
func fakeFixTerminal(t *testing.T, isTTY bool) func() {
	t.Helper()
	old := fixStdinIsTerminal
	fixStdinIsTerminal = func() bool { return isTTY }
	return func() { fixStdinIsTerminal = old }
}

// TestFixBanner_EscapesCommand: файл истории пишет shell из всего, что было
// набрано или вставлено, то есть это вход, который CLI не контролирует. Баннер —
// единственная строка, по которой пользователь узнаёт, какую команду
// собираются отправить провайдеру, и CSI-последовательность внутри неё
// перерисовала бы ровно это подтверждение.
func TestFixBanner_EscapesCommand(t *testing.T) {
	hostile := "git status\x1b[2K\x1b[1Aecho benign"
	got := fixBanner(hostile, "")

	if strings.Contains(got, "\x1b[2K") || strings.Contains(got, "\x1b[1A") {
		t.Errorf("banner leaked a raw CSI sequence from history: %q", got)
	}
	if !strings.Contains(got, `\x1b`) {
		t.Errorf("banner must escape rather than strip, so the user sees the attempt: %q", got)
	}
	if !strings.Contains(got, "echo benign") {
		t.Errorf("banner must still show the whole command: %q", got)
	}
}

// TestFixBanner_CommandStaysOneLine: подтверждение обязано занимать одну строку.
// Перевод строки внутри команды вытолкнул бы настоящую команду за пределы
// видимого и оставил бы на её месте правдоподобную.
func TestFixBanner_CommandStaysOneLine(t *testing.T) {
	got := fixBanner("rm -rf /\nls", "")
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("command line spans %d rows, want 1: %q", len(lines), got)
	}
	if !strings.Contains(got, "ls") {
		t.Errorf("the hidden tail must remain visible: %q", got)
	}
}

// TestFixBanner_OutputKeepsNewlines: у вывода противоположное правило — он
// многострочный по определению, и склеенный в одну строку становится
// нечитаемым именно тем, ради чего показывается. Но управляющие байты, не
// являющиеся переводом строки, всё равно экранируются.
func TestFixBanner_OutputKeepsNewlines(t *testing.T) {
	got := fixBanner("make", "line one\nline two\x1b[31m")
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("output preview lost content: %q", got)
	}
	if strings.Count(got, "line") != 2 {
		t.Errorf("expected both output lines: %q", got)
	}
	if strings.Contains(got, "\x1b[31m") {
		t.Errorf("output preview leaked a raw escape: %q", got)
	}
}

// TestFixBanner_TruncatesLongOutput закрепляет потолок превью.
func TestFixBanner_TruncatesLongOutput(t *testing.T) {
	long := strings.Repeat("a", fixPreviewBytes*3)
	got := fixBanner("make", long)
	if !strings.Contains(got, "...") {
		t.Errorf("long output must be marked as truncated: %q", got)
	}
	if strings.Contains(got, strings.Repeat("a", fixPreviewBytes+1)) {
		t.Error("output preview exceeded fixPreviewBytes")
	}
}

// TestReadHistoryTail_SmallFile: файл в пределах лимита читается целиком и не
// считается усечённым — иначе первая строка отбрасывалась бы у каждого, у кого
// история короткая.
func TestReadHistoryTail_SmallFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hist")
	if err := os.WriteFile(p, []byte("one\ntwo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, truncated, err := readHistoryTail(p)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("a file under the limit must not report truncation")
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("got %q", data)
	}
}

// TestReadHistoryTail_LargeFileReadsTail: огромная история — всё ещё законная
// история, и отказ работать оставил бы без команды именно того пользователя, у
// кого её больше всего. Читается хвост, потому что искомая команда последняя.
func TestReadHistoryTail_LargeFileReadsTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hist")
	filler := strings.Repeat("x", maxHistoryTailBytes*2)
	if err := os.WriteFile(p, []byte(filler+"\ngit push\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, truncated, err := readHistoryTail(p)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("an oversized file must report truncation")
	}
	if len(data) > maxHistoryTailBytes {
		t.Errorf("read %d bytes, limit is %d", len(data), maxHistoryTailBytes)
	}
	if !strings.Contains(string(data), "git push") {
		t.Error("the tail must contain the last command")
	}
}

// TestHistoryLines_DropsPartialFirstLine: усечение с середины делает первую
// строку огрызком команды, и половина команды, отправленная модели как целая,
// хуже отсутствия ответа.
func TestHistoryLines_DropsPartialFirstLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hist")
	// Хвост начинается посреди строки из «x», за ней идут целые команды.
	body := strings.Repeat("x", maxHistoryTailBytes+64) + "\nls -la\ngit push\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := historyLines(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatal("no lines returned")
	}
	if strings.HasPrefix(lines[0], "x") {
		t.Errorf("the partial first line must be dropped, got %q", lines[0][:min(20, len(lines[0]))])
	}
	if lines[0] != "ls -la" {
		t.Errorf("first whole line should be %q, got %q", "ls -la", lines[0])
	}
}

// TestHistoryLines_MissingFile: отсутствующий файл — ошибка, а не пустой срез.
// Вызывающий перебирает оболочки по очереди и обязан отличать «этой оболочки
// нет» от «история пуста».
func TestHistoryLines_MissingFile(t *testing.T) {
	if _, err := historyLines(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("expected an error for a missing history file")
	}
}

// TestRedactHome: текст этой ошибки — то, что человек вставляет в публичный
// issue, когда `qeuro fix` не нашёл историю. Имя пользователя из пути должно
// исчезнуть, а сам путь — остаться, иначе сообщение не подсказывает, где
// именно история ожидалась.
func TestRedactHome(t *testing.T) {
	home := filepath.Join("C:", "Users", "AdminSecret")
	full := filepath.Join(home, "AppData", "hist.txt")

	got := redactHome(full, home)
	if strings.Contains(got, "AdminSecret") {
		t.Errorf("username survived redaction: %q", got)
	}
	if !strings.HasPrefix(got, "~") {
		t.Errorf("redacted path must start with ~, got %q", got)
	}
	if !strings.Contains(got, "hist.txt") {
		t.Errorf("the file being looked for must stay visible: %q", got)
	}

	// Путь вне дома трогать нечего — подменять его на «~» было бы ложью.
	outside := filepath.Join("C:", "ProgramData", "hist.txt")
	if got := redactHome(outside, home); got != outside {
		t.Errorf("a path outside home must pass through, got %q", got)
	}

	// Пустой дом (UserHomeDir не смог) не должен превращать путь в «~» целиком.
	if got := redactHome(full, ""); got != full {
		t.Errorf("empty home must pass through, got %q", got)
	}
}
