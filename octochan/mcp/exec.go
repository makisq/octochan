package main

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
	"unicode/utf8"
)

// llmRole — роль, в которой сервер запускает ochan.
const llmRole = "OCTOCHAN_ROLE=llm"

// runner запускает ochan.
type runner struct {
	// binary — путь к исполняемому файлу ochan.
	binary string
	// dir — рабочий каталог ochan: первый разрешённый каталог.
	dir string
	// env — дополнительные переменные окружения (для тестов).
	env []string
	// timeout — предел времени одного вызова.
	timeout time.Duration
	// maxOutput — предел объёма вывода, возвращаемого модели.
	maxOutput int
}

// outcome — результат запуска.
type outcome struct {
	stdout   string
	stderr   string
	code     int
	timedOut bool
}

// run выполняет ochan с готовыми аргументами.
//
// Командная оболочка не участвует: аргументы передаются процессу как есть,
// поэтому кавычки, точки с запятой и подстановки в значениях ничего не
// значат. На stdin ochan получает только текст сценария, если он есть, —
// и никогда не поток протокола.
func (r *runner) run(ctx context.Context, args []string, stdin string) outcome {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Dir = r.dir
	// ochan работает в роли llm, что бы ни было текущей ролью пользователя
	// и что бы ни стояло в окружении сервера: из повторяющихся переменных
	// действует последняя. Права роли проверяет сам ochan — это второй
	// рубеж за закрытой таблицей инструментов.
	cmd.Env = append(os.Environ(), llmRole)
	if client := clientAddr(ctx); client != "" {
		// Адрес удалённого клиента — в журнал аудита ochan.
		cmd.Env = append(cmd.Env, "OCTOCHAN_REMOTE_ADDR=mcp "+client)
	}
	cmd.Env = append(cmd.Env, r.env...)
	cmd.Stdin = nil
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Сначала просим ochan завершиться сам: он гасит процессы своих модулей.
	// На Windows послать прерывание другому процессу нельзя, там остаётся
	// только принудительное завершение.
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	}
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()

	result := outcome{
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.code = 0
	case errors.As(err, &exitErr):
		result.code = exitErr.ExitCode()
		if result.code == 0 {
			// Процесс завершён сигналом или ожидание прервано.
			result.code = -1
		}
	default:
		result.code = -1
		if result.stderr == "" {
			result.stderr = err.Error()
		}
	}
	return result
}

// text собирает ответ для модели.
//
// При успехе — вывод команды, а замечания из stderr (например, про
// параметры, ждущие перезапуска) дописываются следом: они так же важны,
// как сам результат. При ошибке — сначала ошибка, затем то, что команда
// успела вывести.
func (r *runner) text(result outcome) (string, bool) {
	var b strings.Builder

	switch {
	case result.timedOut:
		fmt.Fprintf(&b, "Time limit exceeded (%s): the command was stopped.\n", r.timeout)
		writeSection(&b, result.stderr)
		writeSection(&b, result.stdout)
		return r.limit(b.String()), true

	case result.code != 0:
		writeSection(&b, result.stderr)
		writeSection(&b, result.stdout)
		if b.Len() == 0 {
			fmt.Fprintf(&b, "ochan exited with code %d and no message.\n", result.code)
		}
		return r.limit(b.String()), true
	}

	writeSection(&b, result.stdout)
	writeSection(&b, result.stderr)
	if b.Len() == 0 {
		b.WriteString("The command succeeded with no output.\n")
	}
	return r.limit(b.String()), false
}

func writeSection(b *strings.Builder, text string) {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(text)
	b.WriteString("\n")
}

// limit обрезает вывод по границе символа и говорит об этом прямо.
func (r *runner) limit(text string) string {
	if r.maxOutput <= 0 || len(text) <= r.maxOutput {
		return text
	}
	cut := r.maxOutput
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n… output truncated: %d of %d bytes shown\n", cut, len(text))
}

// findOchan ищет исполняемый файл ochan: явно указанный, рядом с сервером
// или в PATH.
func findOchan(explicit string) (string, error) {
	if explicit != "" {
		path, err := exec.LookPath(explicit)
		if err != nil {
			return "", fmt.Errorf("ochan не найден по пути %s: %w", explicit, err)
		}
		return filepath.Abs(path)
	}

	name := "ochan"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), name)
		if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() {
			return sibling, nil
		}
	}

	path, err := exec.LookPath("ochan")
	if err != nil {
		return "", errors.New("ochan не найден ни рядом с сервером, ни в PATH; укажите --ochan")
	}
	return filepath.Abs(path)
}
