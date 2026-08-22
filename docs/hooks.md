# Hooks

Qeuro CLI поддерживает расширение через **hooks** — исполняемые скрипты, которые вызываются в ключевые моменты работы CLI.

## Поддерживаемые hook points

### `pre-run`
Выполняется **перед запуском** CLI, но только когда `qeuro` вызван без аргументов вообще (`args := os.Args[1:]` пустой). `qeuro chat` тоже открывает TUI, но идёт другим путём в `main()` и **не** вызывает этот hook — известное несоответствие, см. "Известные ограничения" ниже.

- **Блокирует запуск** если вернёт non-zero exit code
- **Timeout:** 10 секунд
- **Environment variables:**
  - `QEURO_HOOK_CWD` — текущий рабочий каталог

**Примеры использования:**
- Проверка обновлений конфигурации
- Валидация окружения
- Автоматическое обновление зависимостей
- Проверка аутентификации

### `post-diff`
Выполняется **после изменения файла** (write_file или patch_file tool calls).

- **Не блокирует** основную операцию (выполняется асинхронно)
- **Timeout:** 5 секунд
- **Environment variables:**
  - `QEURO_HOOK_OLD_CONTENT` — старое содержимое файла (пустое для новых файлов)
  - `QEURO_HOOK_NEW_CONTENT` — новое содержимое файла
  - `QEURO_HOOK_DATA_FILE` — путь к изменённому файлу

**Примеры использования:**
- Автоматическое форматирование (prettier, gofmt)
- Линтинг изменённых файлов
- Уведомления о изменениях
- Запись в change log

### `pre-commit`
Выполняется **перед git commit** (run_command tool call с командой `git commit`).

- **Блокирует commit** если вернёт non-zero exit code
- **Timeout:** 30 секунд
- **Environment variables:**
  - `QEURO_HOOK_CWD` — текущий рабочий каталог
  - `QEURO_HOOK_DATA_COMMAND` — полная команда git commit

**Примеры использования:**
- Запуск тестов перед коммитом
- Валидация commit message
- Проверка code style
- Запуск линтеров

## Расположение hooks

Hooks ищутся в следующих каталогах (в порядке приоритета):

1. **Project hooks:** `.qeuro/hooks/` (в корне проекта)
2. **User hooks:** `~/.qeuro/hooks/` (глобальные для пользователя)

Если hook найден в обоих местах, выполняется только **project hook** (приоритет у локальных настроек проекта).

## Создание hook

### Windows (`.bat` файл)

```batch
@echo off
REM .qeuro\hooks\pre-run.bat

echo Running pre-run hook...

REM Проверяем что node установлен
where node >nul 2>nul
if %ERRORLEVEL% NEQ 0 (
    echo ERROR: Node.js not found
    exit /b 1
)

echo Environment OK
exit /b 0
```

### Unix (shell script)

```bash
#!/bin/bash
# .qeuro/hooks/pre-run

echo "Running pre-run hook..."

# Проверяем что node установлен
if ! command -v node &> /dev/null; then
    echo "ERROR: Node.js not found"
    exit 1
fi

echo "Environment OK"
exit 0
```

**Важно:**
- На Unix системах hook должен быть **исполняемым**: `chmod +x .qeuro/hooks/pre-run`
- На Windows hook должен иметь расширение `.bat` или `.cmd`
- Hook должен называться **точно как hook point** (без расширения на Unix)

## Примеры

### Автоформатирование после изменения файла

**post-diff** (Unix):
```bash
#!/bin/bash
# .qeuro/hooks/post-diff

FILE="${QEURO_HOOK_DATA_FILE:-}"
if [[ "$FILE" == *.js || "$FILE" == *.ts ]]; then
    echo "Formatting $FILE with prettier..."
    npx prettier --write "$FILE" 2>/dev/null || true
fi
```

**post-diff.bat** (Windows):
```batch
@echo off
if "%QEURO_HOOK_DATA_FILE:~-3%"==".js" (
    echo Formatting %QEURO_HOOK_DATA_FILE% with prettier...
    npx prettier --write "%QEURO_HOOK_DATA_FILE%" 2>nul
)
```

### Запуск тестов перед коммитом

**pre-commit** (Unix):
```bash
#!/bin/bash
# .qeuro/hooks/pre-commit

echo "Running tests before commit..."
npm test
if [ $? -ne 0 ]; then
    echo "Tests failed! Commit blocked."
    exit 1
fi
echo "Tests passed!"
exit 0
```

### Проверка доступности backend

**pre-run** (Unix):
```bash
#!/bin/bash
# .qeuro/hooks/pre-run

BACKEND_URL="${QEURO_BACKEND_URL:-http://localhost:8080}"
if ! curl -sf "$BACKEND_URL/healthz" > /dev/null 2>&1; then
    echo "WARNING: Backend at $BACKEND_URL is not responding"
    echo "Starting anyway..."
fi
exit 0
```

## Безопасность

### Валидация путей
- Hooks должны находиться **строго** в `.qeuro/hooks/` или `~/.qeuro/hooks/`
- Path traversal (`../`) в самом hook-пути **запрещён**
- **Symlinks не проверяются отдельно.** Поиск hook идёт через `os.Stat`, который
  следует symlink до цели — если `.qeuro/hooks/pre-run` окажется симлинком на
  произвольный executable, он выполнится. Это осознанно принятый риск, а не
  недосмотр: у того, кто может создать такой symlink в `.qeuro/hooks/`, уже
  есть право записи туда же, где можно просто положить вредоносный скрипт
  напрямую — symlink ничего дополнительно не даёт атакующему.

### Execution
- Hooks выполняются с правами текущего пользователя
- Нет sandbox или изоляции — полный доступ к файловой системе
- Timeout прерывает сам процесс hook по истечении лимита, но **не** его
  потомков — hook, запустивший фоновый процесс и завершившийся сам, не
  остановит этот процесс таймаутом

### Рекомендации
1. **Не храните секреты** в hooks (они часть репозитория в `.qeuro/`)
2. **Проверяйте project hooks** перед первым запуском в новом проекте — они
   часть репозитория и выполняются с полным доступом без песочницы
3. **Используйте user hooks** (`~/.qeuro/hooks/`) для личных инструментов
4. **Будьте аккуратны с блокирующими hooks** (pre-run, pre-commit)

## Отладка

### Вывод hook
Весь stdout и stderr hook печатается в консоль при выполнении.

### Проверка что hook найден
```bash
# Unix
ls -la .qeuro/hooks/
ls -la ~/.qeuro/hooks/

# Windows
dir .qeuro\hooks\
dir %USERPROFILE%\.qeuro\hooks\
```

### Ручной тест hook
```bash
# Unix
.qeuro/hooks/pre-run

# Windows
.qeuro\hooks\pre-run.bat
```

## Интеграция с MCP

Hooks дополняют MCP серверы (`qeuro mcp <subcommand>`, см. `README.md` и `mcp.json.example`) как второй механизм расширения:

- **MCP** — для новых инструментов (tools) и ресурсов, которые модель может вызывать
- **Hooks** — для автоматизации на стороне клиента в ключевые моменты workflow

Оба механизма работают независимо и не конфликтуют.
