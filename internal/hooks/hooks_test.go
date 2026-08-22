package hooks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPoint_IsValid(t *testing.T) {
	tests := []struct {
		point Point
		want  bool
	}{
		{PreRun, true},
		{PostDiff, true},
		{PreCommit, true},
		{Point("invalid"), false},
		{Point(""), false},
		{Point("pre-run-extra"), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.point), func(t *testing.T) {
			if got := tt.point.IsValid(); got != tt.want {
				t.Errorf("IsValid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManager_Execute_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	m := New([]string{tmpDir})

	result, err := m.Execute(context.Background(), Event{Point: PreRun})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if result.Executed {
		t.Error("Execute() result.Executed = true, want false when hook not found")
	}
}

func TestManager_Execute_InvalidPoint(t *testing.T) {
	m := New([]string{t.TempDir()})

	_, err := m.Execute(context.Background(), Event{Point: Point("invalid")})
	if err == nil {
		t.Fatal("Execute() error = nil, want error for invalid point")
	}
	if !strings.Contains(err.Error(), "invalid hook point") {
		t.Errorf("Execute() error = %v, want 'invalid hook point'", err)
	}
}

func TestManager_Execute_Success(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Создаем простой hook
	hookPath := filepath.Join(hookDir, "pre-run")
	script := makeTestScript(t, "echo hello")
	writeExecutable(t, hookPath, script)

	m := New([]string{tmpDir})
	result, err := m.Execute(context.Background(), Event{
		Point: PreRun,
		Data:  map[string]string{"key": "value"},
	})

	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if !result.Executed {
		t.Error("Execute() result.Executed = false, want true")
	}
	if result.ExitCode != 0 {
		t.Errorf("Execute() result.ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("Execute() result.Output = %q, want to contain 'hello'", result.Output)
	}
}

func TestManager_Execute_NonZeroExit(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(hookDir, "pre-run")
	script := makeTestScript(t, "echo error >&2", "exit 42")
	writeExecutable(t, hookPath, script)

	m := New([]string{tmpDir})
	result, err := m.Execute(context.Background(), Event{Point: PreRun})

	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (non-zero exit is not Go error)", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("Execute() result.ExitCode = %d, want 42", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "error") {
		t.Errorf("Execute() result.Stderr = %q, want to contain 'error'", result.Stderr)
	}
}

func TestManager_Execute_Timeout(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(hookDir, "pre-run")
	// Windows timeout завершается корректно с exit 0 даже после таймаута
	// Используем ping для задержки, которая может быть прервана
	var script string
	if runtime.GOOS == "windows" {
		script = "@echo off\nping -n 11 127.0.0.1 >nul\n"
	} else {
		script = "#!/bin/sh\nsleep 10\n"
	}
	writeExecutable(t, hookPath, script)

	m := New([]string{tmpDir})
	m.SetTimeout(100 * time.Millisecond)

	ctx := context.Background()
	start := time.Now()
	result, err := m.Execute(ctx, Event{Point: PreRun})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s; hook process likely remained attached to pipes", elapsed)
	}

	// Timeout должен вернуть ошибку ИЛИ result.ExitCode != 0
	if err == nil && result.ExitCode == 0 {
		t.Fatal("Execute() succeeded, want timeout error or non-zero exit")
	}
}

func TestManager_Execute_Environment(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(hookDir, "post-diff")
	var script string
	if runtime.GOOS == "windows" {
		script = "@echo off\necho %QEURO_HOOK_POINT% %QEURO_HOOK_DATA_FILE% %QEURO_HOOK_CUSTOM%"
	} else {
		script = "#!/bin/sh\necho $QEURO_HOOK_POINT $QEURO_HOOK_DATA_FILE $QEURO_HOOK_CUSTOM"
	}
	writeExecutable(t, hookPath, script)

	m := New([]string{tmpDir})
	result, err := m.Execute(context.Background(), Event{
		Point: PostDiff,
		Data:  map[string]string{"file": "test.go"},
		Env:   map[string]string{"CUSTOM": "value"},
	})

	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if !strings.Contains(result.Output, "post-diff") {
		t.Errorf("Output missing QEURO_HOOK_POINT: %q", result.Output)
	}
	if !strings.Contains(result.Output, "test.go") {
		t.Errorf("Output missing QEURO_HOOK_DATA_FILE: %q", result.Output)
	}
	if !strings.Contains(result.Output, "value") {
		t.Errorf("Output missing QEURO_HOOK_CUSTOM: %q", result.Output)
	}
}

func TestManager_Execute_SearchPathPriority(t *testing.T) {
	// Два каталога: project и home
	projectDir := t.TempDir()
	homeDir := t.TempDir()

	// Hook в обоих местах
	for _, dir := range []string{projectDir, homeDir} {
		hookDir := filepath.Join(dir, ".qeuro", "hooks")
		if err := os.MkdirAll(hookDir, 0755); err != nil {
			t.Fatal(err)
		}
		hookPath := filepath.Join(hookDir, "pre-run")
		script := makeTestScript(t, fmt.Sprintf("echo %s", filepath.Base(dir)))
		writeExecutable(t, hookPath, script)
	}

	// Приоритет: project первый
	m := New([]string{projectDir, homeDir})
	result, err := m.Execute(context.Background(), Event{Point: PreRun})

	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	projectName := filepath.Base(projectDir)
	if !strings.Contains(result.Output, projectName) {
		t.Errorf("Execute() used wrong hook, output = %q, want %q", result.Output, projectName)
	}
}

func TestIsValidHookPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"absolute path unix", "/home/user/.qeuro/hooks/pre-run", true},
		{"windows absolute", `C:\Users\user\.qeuro\hooks\pre-run`, true},
		{"relative path", ".qeuro/hooks/pre-run", false},
		{"path traversal", "/home/user/../etc/passwd", false},
		{"double dots in name", "/home/user/.qeuro/hooks/pre..run", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Нормализуем для платформы
			path := filepath.FromSlash(tt.path)
			// Для Unix-путей на Windows filepath.IsAbs вернет false
			if runtime.GOOS == "windows" && strings.HasPrefix(tt.path, "/") {
				t.Skip("skipping unix path test on windows")
			}
			if got := isValidHookPath(path); got != tt.want {
				t.Errorf("isValidHookPath(%q) = %v, want %v", path, got, tt.want)
			}
		})
	}
}

func TestIsExecutable(t *testing.T) {
	tmpDir := t.TempDir()

	// Обычный файл без execute bit
	regularFile := filepath.Join(tmpDir, "regular.txt")
	if err := os.WriteFile(regularFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// Executable файл (на Unix — с execute bit, на Windows проверка пропущена для .sh)
	execFile := filepath.Join(tmpDir, "executable")
	if runtime.GOOS == "windows" {
		execFile += ".bat"
	}
	if err := os.WriteFile(execFile, []byte("#!/bin/sh\necho test"), 0755); err != nil {
		t.Fatal(err)
	}

	// Директория
	dir := filepath.Join(tmpDir, "dir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"non-existent", filepath.Join(tmpDir, "none"), false},
		{"regular file", regularFile, false},
		{"executable file", execFile, true},
		{"directory", dir, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExecutable(tt.path); got != tt.want {
				t.Errorf("isExecutable(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestDefaultSearchPaths(t *testing.T) {
	paths, err := DefaultSearchPaths()
	if err != nil {
		t.Fatalf("DefaultSearchPaths() error = %v", err)
	}
	if len(paths) == 0 {
		t.Error("DefaultSearchPaths() returned empty slice")
	}
	// Должен быть хотя бы один путь (cwd или home)
	if len(paths) < 1 {
		t.Errorf("DefaultSearchPaths() returned %d paths, want at least 1", len(paths))
	}
}

// makeTestScript создает платформо-специфичный скрипт.
func makeTestScript(t *testing.T, commands ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		script := "@echo off\n"
		for _, cmd := range commands {
			// Windows sleep через timeout
			if strings.HasPrefix(cmd, "sleep ") {
				sec := strings.TrimPrefix(cmd, "sleep ")
				script += fmt.Sprintf("timeout /t %s /nobreak >nul\n", sec)
			} else {
				script += cmd + "\n"
			}
		}
		return script
	}
	script := "#!/bin/sh\n"
	for _, cmd := range commands {
		script += cmd + "\n"
	}
	return script
}

// writeExecutable записывает скрипт как executable файл.
func writeExecutable(t *testing.T, path, script string) {
	t.Helper()

	// На Windows добавляем .bat расширение
	if runtime.GOOS == "windows" && !strings.HasSuffix(path, ".bat") {
		path += ".bat"
	}

	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write hook script: %v", err)
	}
}
