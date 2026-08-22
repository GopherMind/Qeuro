package hooks

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunPreRunHook_NotFound(t *testing.T) {
	// Создаём временный каталог без hooks
	tmpDir := t.TempDir()
	oldHome := os.Getenv("USERPROFILE")
	if runtime.GOOS != "windows" {
		oldHome = os.Getenv("HOME")
	}
	defer func() {
		if runtime.GOOS == "windows" {
			_ = os.Setenv("USERPROFILE", oldHome)
		} else {
			_ = os.Setenv("HOME", oldHome)
		}
	}()

	if runtime.GOOS == "windows" {
		_ = os.Setenv("USERPROFILE", tmpDir)
	} else {
		_ = os.Setenv("HOME", tmpDir)
	}

	// Hook не найден — должно продолжить
	ok, err := RunPreRunHook(context.Background())
	if !ok {
		t.Errorf("expected ok=true when hook not found, got false")
	}
	if err != nil {
		t.Errorf("expected no error when hook not found, got %v", err)
	}
}

func TestRunPreRunHook_Success(t *testing.T) {
	tmpDir := t.TempDir()
	hooksDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(hooksDir, "pre-run")
	if runtime.GOOS == "windows" {
		hookPath += ".bat"
		content := "@echo off\necho pre-run executed\nexit /b 0\n"
		if err := os.WriteFile(hookPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	} else {
		content := "#!/bin/sh\necho pre-run executed\nexit 0\n"
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Временно меняем рабочий каталог
	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)

	ok, err := RunPreRunHook(context.Background())
	if !ok {
		t.Errorf("expected ok=true for successful hook, got false")
	}
	if err != nil {
		t.Errorf("expected no error for successful hook, got %v", err)
	}
}

func TestRunPreRunHook_Failure(t *testing.T) {
	tmpDir := t.TempDir()
	hooksDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(hooksDir, "pre-run")
	if runtime.GOOS == "windows" {
		hookPath += ".bat"
		content := "@echo off\necho pre-run failed\nexit /b 1\n"
		if err := os.WriteFile(hookPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	} else {
		content := "#!/bin/sh\necho pre-run failed\nexit 1\n"
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)

	ok, err := RunPreRunHook(context.Background())
	if ok {
		t.Errorf("expected ok=false for failing hook, got true")
	}
	if err == nil {
		t.Errorf("expected error for failing hook, got nil")
	}
}

func TestRunPostDiffHook(t *testing.T) {
	// post-diff hook не должен блокировать — просто проверяем что не паникует
	tmpDir := t.TempDir()
	hooksDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(hooksDir, "post-diff")
	if runtime.GOOS == "windows" {
		hookPath += ".bat"
		content := "@echo off\necho post-diff executed\nexit /b 0\n"
		if err := os.WriteFile(hookPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	} else {
		content := "#!/bin/sh\necho post-diff executed\nexit 0\n"
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)

	// Не должно паниковать
	RunPostDiffHook(context.Background(), "test.txt", "old", "new")
}

// TestRunPostDiffHook_ContentPassing проверяет, что post-diff hook получает
// НЕПЕРЕПУТАННЫЕ значения old/new content — то есть QEURO_HOOK_OLD_CONTENT
// содержит именно старое содержимое, а QEURO_HOOK_NEW_CONTENT — новое, а не
// наоборот. Закрывает mutation-gap из .ai/review-8-cli-plugins.md: мутант,
// меняющий местами аргументы oldContent/newContent в вызове RunPostDiffHook,
// должен обнаруживаться этим тестом.
func TestRunPostDiffHook_ContentPassing(t *testing.T) {
	tmpDir := t.TempDir()
	hooksDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(tmpDir, "captured.txt")

	hookPath := filepath.Join(hooksDir, "post-diff")
	if runtime.GOOS == "windows" {
		hookPath += ".bat"
		// %VAR% раскрывается cmd.exe в момент записи в файл.
		content := "@echo off\r\n" +
			"echo file=%QEURO_HOOK_DATA_FILE%> \"" + outPath + "\"\r\n" +
			"echo old=%QEURO_HOOK_OLD_CONTENT%>> \"" + outPath + "\"\r\n" +
			"echo new=%QEURO_HOOK_NEW_CONTENT%>> \"" + outPath + "\"\r\n" +
			"exit /b 0\r\n"
		if err := os.WriteFile(hookPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	} else {
		content := "#!/bin/sh\n" +
			"echo \"file=$QEURO_HOOK_DATA_FILE\" > \"" + outPath + "\"\n" +
			"echo \"old=$QEURO_HOOK_OLD_CONTENT\" >> \"" + outPath + "\"\n" +
			"echo \"new=$QEURO_HOOK_NEW_CONTENT\" >> \"" + outPath + "\"\n" +
			"exit 0\n"
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)

	const wantOld = "old-value-X"
	const wantNew = "new-value-Y"
	RunPostDiffHook(context.Background(), "test.txt", wantOld, wantNew)

	captured, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("hook did not run or did not write output: %v", err)
	}
	out := string(captured)

	if !strings.Contains(out, "file=test.txt") {
		t.Errorf("captured output missing QEURO_HOOK_DATA_FILE=test.txt, got: %q", out)
	}
	if !strings.Contains(out, "old="+wantOld) {
		t.Errorf("captured output missing QEURO_HOOK_OLD_CONTENT=%s, got: %q", wantOld, out)
	}
	if !strings.Contains(out, "new="+wantNew) {
		t.Errorf("captured output missing QEURO_HOOK_NEW_CONTENT=%s, got: %q", wantNew, out)
	}
	// Явно проверяем, что значения НЕ перепутаны местами.
	if strings.Contains(out, "old="+wantNew) || strings.Contains(out, "new="+wantOld) {
		t.Errorf("old/new content values appear swapped, got: %q", out)
	}
}

func TestRunPreCommitHook_NotGitCommit(t *testing.T) {
	// Команда не git commit — должно пропустить
	ok, err := RunPreCommitHook(context.Background(), "git status")
	if !ok {
		t.Errorf("expected ok=true for non-commit command, got false")
	}
	if err != nil {
		t.Errorf("expected no error for non-commit command, got %v", err)
	}
}

func TestRunPreCommitHook_Success(t *testing.T) {
	tmpDir := t.TempDir()
	hooksDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(hooksDir, "pre-commit")
	if runtime.GOOS == "windows" {
		hookPath += ".bat"
		content := "@echo off\necho pre-commit executed\nexit /b 0\n"
		if err := os.WriteFile(hookPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	} else {
		content := "#!/bin/sh\necho pre-commit executed\nexit 0\n"
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)

	ok, err := RunPreCommitHook(context.Background(), "git commit -m 'test'")
	if !ok {
		t.Errorf("expected ok=true for successful pre-commit, got false")
	}
	if err != nil {
		t.Errorf("expected no error for successful pre-commit, got %v", err)
	}
}

func TestRunPreCommitHook_Blocked(t *testing.T) {
	tmpDir := t.TempDir()
	hooksDir := filepath.Join(tmpDir, ".qeuro", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(hooksDir, "pre-commit")
	if runtime.GOOS == "windows" {
		hookPath += ".bat"
		content := "@echo off\necho tests failed\nexit /b 1\n"
		if err := os.WriteFile(hookPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	} else {
		content := "#!/bin/sh\necho tests failed\nexit 1\n"
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)

	ok, err := RunPreCommitHook(context.Background(), "git commit -m 'test'")
	if ok {
		t.Errorf("expected ok=false for blocked pre-commit, got true")
	}
	if err != nil {
		// Ошибка ожидается
	}
}
