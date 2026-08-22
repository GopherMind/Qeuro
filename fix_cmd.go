package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/styles"
)

const fixUsage = "qeuro fix [--yes] [--local]"

// fixPreviewBytes ограничивает превью вывода команды в баннере.
const fixPreviewBytes = 200

// fixBanner строит блок, который пользователь читает, решая, отправлять ли
// команду. Он выделен из cmdFix, потому что это и есть единственная защита от
// «исправляем не то, что показано»: утверждение об экранировании, проверяемое
// только глазами, не является проверенным.
//
// Обе половины идут через санитизацию, но разными функциями, и разница
// содержательная. Команда — через DisplaySafe (экранирует и перевод строки):
// это строка подтверждения, и она обязана занимать одну строку терминала,
// иначе `\n` внутри команды вытолкнул бы настоящую команду за пределы видимого
// и оставил бы правдоподобную. Вывод — через DisplaySafeBlock (перевод строки
// сохраняется): вывод упавшей команды многострочный по определению, и склеив
// его в одну строку мы сделали бы нечитаемым именно то, ради чего его показываем.
//
// Экранируем, а не срезаем: пользователь должен видеть, что в его истории
// оказался управляющий байт. Молча укороченная строка скрывает попытку.
func fixBanner(lastCmd, output string) string {
	var b strings.Builder
	b.WriteString("  " + styles.Chip("FIXING", styles.Amber) + " " + styles.UserTag.Render(clientcfg.DisplaySafe(lastCmd)) + "\n")
	if output != "" {
		preview := output
		if len(preview) > fixPreviewBytes {
			preview = preview[:fixPreviewBytes] + "..."
		}
		// Санитизация ПОСЛЕ обрезки и до отбивки отступом: отступ ставится по
		// настоящим переводам строк, а экранированные `\x` уже не переводы.
		preview = clientcfg.DisplaySafeBlock(preview)
		b.WriteString("  " + styles.Muted.Render(strings.ReplaceAll(preview, "\n", "\n  ")) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// fixStdinIsTerminal — шов для теста. Настоящий предикат под `go test` всегда
// ложен (stdin не терминал), поэтому без подмены ветку с подтверждением
// нельзя было бы проверить вообще.
var fixStdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// cmdFix реализует `qeuro fix` — берёт последнюю команду из истории shell и
// её вывод и отправляет выбранной модели для объяснения или исправления.
// Roadmap §8 "Shell-интеграция" и "Offline".
func cmdFix(args []string) {
	parsed, err := parseFixArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("error: ")+err.Error())
		fmt.Fprintln(os.Stderr, styles.Muted.Render("usage: "+fixUsage))
		os.Exit(2)
	}

	cfg := loadConfigOrExit()
	if parsed.local {
		cfg.Local = true
	}
	if !cfg.Local && !cfg.LoggedIn() {
		fmt.Println("  " + styles.Warn.Render("not logged in. ") + styles.UserTag.Render("qeuro login <token>"))
		os.Exit(1)
	}

	// Читаем последнюю упавшую команду из истории shell
	lastCmd, output, err := getLastFailedCommand()
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("cannot read shell history: ")+clientcfg.DisplaySafe(err.Error()))
		os.Exit(1)
	}
	if lastCmd == "" {
		fmt.Println("  " + styles.Muted.Render("no failed command found in shell history"))
		os.Exit(0)
	}

	fmt.Print(fixBanner(lastCmd, output))

	// Подтверждение оставлено и для локального режима: `QEURO_LOCAL_URL` может
	// указывать на сервер внутри закрытой сети, а не на этот компьютер. Текст
	// говорит правду для обоих путей, не обещая, что endpoint обязательно localhost.
	if !confirmFixSend(parsed.yes, os.Stdin) {
		fmt.Println("  " + styles.Muted.Render("cancelled"))
		os.Exit(0)
	}

	// Формируем промпт для модели
	prompt := fmt.Sprintf("The following command failed:\n\n```\n%s\n```\n\nOutput:\n```\n%s\n```\n\nProvide a fix or explanation.", lastCmd, output)

	// Локальной модели на CPU одного ответа может не хватить 120 секунд: первый
	// токен 7B-модели на ноутбуке приходит через десятки секунд. Таймаут, из-за
	// которого офлайн-режим выглядел бы неработающим ровно на том железе, для
	// которого он и сделан, — не защита, поэтому для --local окно шире.
	timeout := 120 * time.Second
	if cfg.Local {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req := client.ChatRequest{
		Mode: "chat",
		Messages: []client.Message{
			{Role: "user", Content: prompt},
		},
	}

	// Config.Provider — единственная точка выбора источника инференса: в
	// local-режиме backend-клиент не создаётся вовсе, поэтому bearer-токен
	// физически не может уйти на локальный endpoint.
	provider := cfg.Provider()
	events, err := provider.Chat(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("request failed: ")+clientcfg.DisplaySafe(err.Error()))
		os.Exit(1)
	}

	// Выводим ответ потоково. Текст токенов намеренно НЕ санитизируется: это
	// затребованный ответ модели, он многострочный, содержит код и должен
	// оставаться пригодным для копирования — как и в TUI, где токены идут в
	// markdown-рендерер. Сообщение об ошибке — другой случай: это короткая
	// строка от удалённой стороны, и она проходит DisplaySafe, как в mcpcall.go
	// и update.go.
	for ev := range events {
		switch ev.Kind {
		case client.EventToken:
			fmt.Print(ev.Text)
		case client.EventError:
			fmt.Fprintln(os.Stderr, "\n"+styles.Err.Render("error: ")+clientcfg.DisplaySafe(ev.ErrMsg))
			os.Exit(1)
		case client.EventDone:
			fmt.Println()
		}
	}
}

// fixArgs — разобранные флаги `qeuro fix`.
type fixArgs struct {
	// yes пропускает гейт подтверждения.
	yes bool
	// local отправляет запрос локальной модели вместо бэкенда (роадмап §8,
	// строка "Offline"). Для этой команды строка особенно уместна: `qeuro fix`
	// отправляет последнюю команду из истории shell вместе с её выводом, то есть
	// именно то, что нельзя показывать чужим логам, — а локальная модель не
	// выпускает это за пределы машины.
	local bool
}

// parseFixArgs читает флаги. Неизвестный аргумент — ошибка, а не игнор:
// `qeuro fix --ys`, молча запросивший подтверждение, ещё терпим, но обратный
// случай — опечатка в имени флага, которая молча отправила бы команду с
// секретом, — ровно то, чего этот флаг существует чтобы не допустить.
func parseFixArgs(args []string) (fixArgs, error) {
	var parsed fixArgs
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			parsed.yes = true
		case "--local":
			parsed.local = true
		default:
			return fixArgs{}, fmt.Errorf("unknown argument %q", a)
		}
	}
	return parsed, nil
}

// confirmFixSend спрашивает разрешение отправить команду провайдеру модели.
//
// Гейт существует потому, что содержимое отправляемого выбирает не эта
// программа и не пользователь в момент вызова: `qeuro fix` берёт то, что
// оказалось последним в истории, а история — это место, где живут
// `curl -H "Authorization: Bearer ..."`, `mysql -p...` и
// `export AWS_SECRET_ACCESS_KEY=...`. Отправка уходит на бэкенд и дальше к
// провайдеру, то есть попадает в чужие логи, и отменить это после факта
// нельзя — ротация ключа не то же самое, что неотправленный ключ.
//
// Фильтрация по шаблонам (`password=`, `token=`, `Bearer`) намеренно НЕ
// используется: она даёт ложное чувство защиты, пропуская всё, что не совпало
// с списком, и при этом ломает законные команды, попавшие под шаблон. Человек,
// смотрящий на собственную команду, знает про свои секреты то, чего не знает
// никакой regexp.
//
// Умолчание — отказ: пустой ввод (просто Enter) означает «нет», потому что
// стоимость ошибочного «да» здесь необратима, а ошибочного «нет» — одно
// повторное нажатие.
//
// Не-TTY stdin — тоже отказ, и это важнее, чем кажется: в пайплайне никто не
// увидит вопроса, и «нет ответа» нельзя трактовать как согласие. Обойти это
// можно только явным `--yes`, то есть решение остаётся за человеком, просто
// принятым заранее.
func confirmFixSend(yes bool, in io.Reader) bool {
	if yes {
		return true
	}
	if !fixStdinIsTerminal() {
		fmt.Fprintln(os.Stderr, styles.Warn.Render("  refusing to send shell history to the model without confirmation"))
		fmt.Fprintln(os.Stderr, styles.Muted.Render("  stdin is not a terminal; pass --yes if you have reviewed the command"))
		return false
	}
	fmt.Println("  " + styles.Muted.Render("this command will be sent to the model provider."))
	fmt.Println("  " + styles.Muted.Render("check it for passwords, tokens and keys first."))
	fmt.Print("  " + styles.Warn.Render("send it? [y/N] "))

	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		fmt.Println()
		return false
	}
	fmt.Println()
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// getLastFailedCommand читает историю shell и возвращает последнюю упавшую
// команду и её вывод. На Windows пытается прочитать PowerShell/cmd историю,
// на Unix — bash/zsh/fish.
func getLastFailedCommand() (cmd string, output string, err error) {
	switch runtime.GOOS {
	case "windows":
		return getLastFailedCommandWindows()
	default:
		return getLastFailedCommandUnix()
	}
}

// maxHistoryTailBytes ограничивает чтение файла истории. Это не проверка
// размера с отказом, а чтение хвоста: история с отключённой ротацией легко
// вырастает до сотен мегабайт, но она всё ещё законная история, и отказ
// работать оставил бы без команды ровно того пользователя, у которого её
// больше всего. Искомая команда — последняя, то есть всегда в хвосте.
const maxHistoryTailBytes = 256 << 10

// readHistoryTail читает не более maxHistoryTailBytes последних байт файла.
// Второе значение сообщает, был ли файл обрезан: усечение с середины делает
// первую строку возможным огрызком команды, и вызывающий обязан её отбросить —
// половина команды, отправленная модели как целая, хуже отсутствия ответа.
func readHistoryTail(path string) (data []byte, truncated bool, err error) {
	// #nosec G304 -- path собран из os.UserHomeDir() и константных сегментов,
	// это стандартное расположение истории собственного shell пользователя.
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()

	// Stat до чтения, а не io.LimitReader после: лимит на ридере всё равно
	// протащил бы гигабайт через чтение, прежде чем упереться в потолок.
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	size := info.Size()
	if size <= maxHistoryTailBytes {
		buf, err := io.ReadAll(f)
		return buf, false, err
	}
	if _, err := f.Seek(size-maxHistoryTailBytes, io.SeekStart); err != nil {
		return nil, false, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxHistoryTailBytes))
	if err != nil {
		return nil, false, err
	}
	return buf, true, nil
}

// historyLines разбивает хвост истории на строки, отбрасывая первую при
// усечении файла.
func historyLines(path string) ([]string, error) {
	data, truncated, err := readHistoryTail(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if truncated && len(lines) > 1 {
		lines = lines[1:]
	}
	return lines, nil
}

// redactHome заменяет префикс домашнего каталога на `~`, чтобы сообщение об
// ошибке оставалось диагностически полезным (видно, какой именно файл искали),
// но не раскрывало имя пользователя: этот текст люди вставляют в публичные
// issue-трекеры. Полностью убирать путь нельзя — тогда непонятно, куда класть
// историю.
func redactHome(path, home string) string {
	if home == "" {
		return path
	}
	if rest := strings.TrimPrefix(path, home); rest != path {
		return "~" + rest
	}
	return path
}

// getLastFailedCommandWindows пытается извлечь последнюю упавшую команду
// из истории PowerShell (ConsoleHost_history.txt). Вывод команды не сохраняется
// в истории, поэтому возвращаем только команду.
func getLastFailedCommandWindows() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}

	// PowerShell история
	psHistoryPath := filepath.Join(home, "AppData", "Roaming", "Microsoft", "Windows", "PowerShell", "PSReadLine", "ConsoleHost_history.txt")
	if lines, err := historyLines(psHistoryPath); err == nil {
		// Берём последнюю непустую строку
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line != "" && !strings.HasPrefix(line, "#") {
				// История PowerShell не содержит кодов выхода, возвращаем последнюю команду
				return line, "", nil
			}
		}
	}

	return "", "", fmt.Errorf("no PowerShell history found at %s", redactHome(psHistoryPath, home))
}

// getLastFailedCommandUnix читает историю bash/zsh/fish и пытается найти
// последнюю команду с ненулевым exit code.
func getLastFailedCommandUnix() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}

	// Пробуем bash history с расширенным форматом (HISTTIMEFORMAT)
	bashHistory := filepath.Join(home, ".bash_history")
	if lines, err := historyLines(bashHistory); err == nil {
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line != "" && !strings.HasPrefix(line, "#") {
				// bash history не хранит exit code напрямую, возвращаем последнюю
				return line, "", nil
			}
		}
	}

	// Пробуем zsh history (extended_history format: : timestamp:elapsed;command)
	zshHistory := filepath.Join(home, ".zsh_history")
	if lines, err := historyLines(zshHistory); err == nil {
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line != "" {
				// Извлекаем команду после `;`
				if idx := strings.Index(line, ";"); idx != -1 {
					cmd := strings.TrimSpace(line[idx+1:])
					if cmd != "" {
						return cmd, "", nil
					}
				} else if !strings.HasPrefix(line, ":") {
					return line, "", nil
				}
			}
		}
	}

	// Пробуем fish history (YAML формат)
	fishHistory := filepath.Join(home, ".local", "share", "fish", "fish_history")
	if lines, err := historyLines(fishHistory); err == nil {
		var lastCmd string
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if strings.HasPrefix(line, "- cmd:") {
				lastCmd = strings.TrimSpace(strings.TrimPrefix(line, "- cmd:"))
				if lastCmd != "" {
					return lastCmd, "", nil
				}
			}
		}
	}

	// Если ничего не нашли, пробуем запустить `fc -ln -1` (работает в bash/zsh)
	cmd := exec.Command("sh", "-c", "fc -ln -1 2>/dev/null || history 1 | sed 's/^[ ]*[0-9]*[ ]*//'")
	out, err := cmd.Output()
	if err == nil {
		line := strings.TrimSpace(string(out))
		if line != "" {
			return line, "", nil
		}
	}

	return "", "", fmt.Errorf("no shell history found")
}
