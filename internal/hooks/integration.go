package hooks

import (
	"context"
	"fmt"
	"os"
	"time"
)

// RunPreRunHook выполняет pre-run hook перед запуском CLI.
// Возвращает true если hook выполнен успешно (exit 0) или не найден,
// false если hook вернул non-zero exit code (блокирует запуск).
func RunPreRunHook(ctx context.Context) (bool, error) {
	paths, err := DefaultSearchPaths()
	if err != nil {
		// Если не можем определить пути — продолжаем без hooks
		return true, nil
	}

	mgr := New(paths)
	mgr.SetTimeout(10 * time.Second) // pre-run не должен висеть долго

	result, err := mgr.Execute(ctx, Event{
		Point: PreRun,
		Env: map[string]string{
			"CWD": mustGetwd(),
		},
	})

	if err != nil {
		return false, fmt.Errorf("pre-run hook failed: %w", err)
	}

	// Hook не найден — продолжаем
	if !result.Executed {
		return true, nil
	}

	// Печатаем stdout/stderr hook если они есть
	if result.Output != "" {
		fmt.Fprint(os.Stdout, result.Output)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	// Non-zero exit блокирует запуск
	if result.ExitCode != 0 {
		return false, fmt.Errorf("pre-run hook exited with code %d", result.ExitCode)
	}

	return true, nil
}

// RunPostDiffHook выполняет post-diff hook после изменения файла.
// Не блокирует операцию — hook для наблюдения и side effects.
func RunPostDiffHook(ctx context.Context, filePath, oldContent, newContent string) {
	paths, err := DefaultSearchPaths()
	if err != nil {
		return
	}

	mgr := New(paths)
	mgr.SetTimeout(5 * time.Second)

	_, _ = mgr.Execute(ctx, Event{
		Point: PostDiff,
		Data: map[string]string{
			"file": filePath,
		},
		Env: map[string]string{
			"OLD_CONTENT": oldContent,
			"NEW_CONTENT": newContent,
		},
	})
	// Ошибки игнорируем — post-diff hook не должен ломать основной поток
}

// RunPreCommitHook выполняет pre-commit hook перед git commit.
// Возвращает true если разрешено продолжать, false если заблокировано.
func RunPreCommitHook(ctx context.Context, command string) (bool, error) {
	// Проверяем что это git commit команда
	if !isGitCommit(command) {
		return true, nil
	}

	paths, err := DefaultSearchPaths()
	if err != nil {
		return true, nil
	}

	mgr := New(paths)
	mgr.SetTimeout(30 * time.Second) // pre-commit может запускать линтеры

	result, err := mgr.Execute(ctx, Event{
		Point: PreCommit,
		Data: map[string]string{
			"command": command,
		},
		Env: map[string]string{
			"CWD": mustGetwd(),
		},
	})

	if err != nil {
		return false, fmt.Errorf("pre-commit hook failed: %w", err)
	}

	if !result.Executed {
		return true, nil
	}

	// Печатаем вывод hook
	if result.Output != "" {
		fmt.Fprint(os.Stdout, result.Output)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	// Non-zero exit блокирует commit
	return result.ExitCode == 0, nil
}

// mustGetwd возвращает текущий каталог или пустую строку при ошибке.
func mustGetwd() string {
	cwd, _ := os.Getwd()
	return cwd
}

// isGitCommit проверяет, является ли команда git commit.
func isGitCommit(command string) bool {
	// Простая эвристика: команда начинается с "git commit"
	// Более точную проверку можно добавить при необходимости
	if len(command) < 10 {
		return false
	}
	prefix := command
	if len(prefix) > 11 {
		prefix = command[:11]
	}
	return prefix == "git commit " || prefix == "git commit\t" || prefix == "git commit\n" || command == "git commit"
}
