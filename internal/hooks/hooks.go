// Package hooks реализует систему расширения через executable hooks.
// Поддерживаемые точки: pre-run, post-diff, pre-commit.
package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Point представляет точку вызова hook.
type Point string

const (
	PreRun    Point = "pre-run"
	PostDiff  Point = "post-diff"
	PreCommit Point = "pre-commit"
)

// String возвращает строковое представление точки.
func (p Point) String() string {
	return string(p)
}

// IsValid проверяет, что точка hook поддерживается.
func (p Point) IsValid() bool {
	switch p {
	case PreRun, PostDiff, PreCommit:
		return true
	default:
		return false
	}
}

// Manager управляет поиском и выполнением hooks.
type Manager struct {
	// Каталоги для поиска hooks (приоритет: первый побеждает)
	searchPaths []string
	// Таймаут для выполнения hook (по умолчанию 30s)
	timeout time.Duration
}

// New создает новый Manager с указанными путями поиска.
// Пути проверяются в порядке указания: первый найденный hook выполняется.
func New(searchPaths []string) *Manager {
	return &Manager{
		searchPaths: searchPaths,
		timeout:     30 * time.Second,
	}
}

// SetTimeout устанавливает таймаут для выполнения hooks.
func (m *Manager) SetTimeout(d time.Duration) {
	m.timeout = d
}

// Event содержит контекст для hook.
type Event struct {
	Point Point
	// Data передается в hook через stdin (JSON)
	Data map[string]string
	// Env дополнительные переменные окружения
	Env map[string]string
}

// Result содержит результат выполнения hook.
type Result struct {
	// Executed true если hook был найден и выполнен
	Executed bool
	// Output stdout hook
	Output string
	// Stderr stderr hook
	Stderr string
	// ExitCode код возврата
	ExitCode int
	// Duration время выполнения
	Duration time.Duration
}

// Execute ищет и выполняет hook для указанной точки.
// Возвращает Result и ошибку. Не-нулевой exit code не является ошибкой,
// но записывается в Result.ExitCode.
func (m *Manager) Execute(ctx context.Context, event Event) (Result, error) {
	if !event.Point.IsValid() {
		return Result{}, fmt.Errorf("invalid hook point: %s", event.Point)
	}

	hookPath, err := m.findHook(event.Point)
	if err != nil {
		// Hook не найден — не ошибка
		if errors.Is(err, os.ErrNotExist) {
			return Result{Executed: false}, nil
		}
		return Result{}, err
	}

	return m.executeHook(ctx, hookPath, event)
}

// findHook ищет executable hook в searchPaths.
// Возвращает полный путь к первому найденному hook или os.ErrNotExist.
func (m *Manager) findHook(point Point) (string, error) {
	hookName := string(point)

	for _, dir := range m.searchPaths {
		// Проверяем dir/.qeuro/hooks/<hookName>
		hookPath := filepath.Join(dir, ".qeuro", "hooks", hookName)

		// На Windows пробуем с расширениями .bat, .cmd, .exe
		if runtime.GOOS == "windows" {
			for _, ext := range []string{".bat", ".cmd", ".exe", ""} {
				candidate := hookPath + ext
				if isExecutable(candidate) {
					return candidate, nil
				}
			}
		} else {
			if isExecutable(hookPath) {
				return hookPath, nil
			}
		}
	}

	return "", os.ErrNotExist
}

// isExecutable проверяет, что файл существует и является executable.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}

	mode := info.Mode()

	// На Windows расширения .bat, .cmd, .exe всегда executable
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".bat" || ext == ".cmd" || ext == ".exe" {
		return true
	}

	// На Unix проверяем execute bit
	// На Windows для файлов без известного расширения тоже проверяем права (Git Bash скрипты)
	return mode&0111 != 0
}

// executeHook выполняет найденный hook.
func (m *Manager) executeHook(ctx context.Context, hookPath string, event Event) (Result, error) {
	// Валидация пути — защита от path traversal
	if !isValidHookPath(hookPath) {
		return Result{}, fmt.Errorf("invalid hook path: %s", hookPath)
	}

	// Контекст с таймаутом
	execCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(execCtx, hookPath)
	// A hook may spawn a child that inherits stdout/stderr. On Windows the
	// parent is killed when the context expires, but Cmd.Wait otherwise waits
	// forever for those inherited pipe handles to close. WaitDelay bounds that
	// cleanup and keeps a timed-out hook from wedging the whole CLI test/run.
	cmd.WaitDelay = time.Second

	// Передаем event.Data через environment variables (безопаснее stdin для shell injection)
	env := os.Environ()
	env = append(env, fmt.Sprintf("QEURO_HOOK_POINT=%s", event.Point))
	for k, v := range event.Env {
		// Префикс QEURO_HOOK_ для всех переменных hook
		if !strings.HasPrefix(k, "QEURO_HOOK_") {
			k = "QEURO_HOOK_" + k
		}
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range event.Data {
		env = append(env, fmt.Sprintf("QEURO_HOOK_DATA_%s=%s", strings.ToUpper(k), v))
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	duration := time.Since(start)

	result := Result{
		Executed: true,
		Output:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		// A context timeout is a hook execution failure even when the killed
		// process is reported as an ordinary ExitError (notably on Windows).
		if execCtx.Err() != nil {
			return result, fmt.Errorf("hook execution failed: %w", execCtx.Err())
		}
		// ExitError — не ошибка Go, а exit code hook
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		// Остальные ошибки (timeout, killed) возвращаем как ошибку
		return result, fmt.Errorf("hook execution failed: %w", err)
	}

	result.ExitCode = 0
	return result, nil
}

// isValidHookPath проверяет путь на безопасность.
// Отвергает пути с '..' компонентами для предотвращения path traversal.
func isValidHookPath(path string) bool {
	// Путь должен быть абсолютным (уже resolved через searchPaths)
	if !filepath.IsAbs(path) {
		return false
	}

	// Нормализация пути — Clean убирает ./ и лишние слэши
	clean := filepath.Clean(path)

	// Отвергаем пути с '..' компонентами (path traversal)
	// Проверяем после Clean, чтобы поймать ../../../etc/passwd
	if strings.Contains(filepath.ToSlash(clean), "/..") || strings.Contains(clean, "\\..") {
		return false
	}

	return true
}

// DefaultSearchPaths возвращает стандартные пути поиска hooks:
// 1. Текущий рабочий каталог
// 2. Домашний каталог пользователя (~/.qeuro)
func DefaultSearchPaths() ([]string, error) {
	paths := make([]string, 0, 2)

	// 1. Текущий каталог
	cwd, err := os.Getwd()
	if err == nil {
		paths = append(paths, cwd)
	}

	// 2. Домашний каталог
	home, err := os.UserHomeDir()
	if err == nil {
		paths = append(paths, home)
	}

	if len(paths) == 0 {
		return nil, errors.New("no valid search paths found")
	}

	return paths, nil
}
