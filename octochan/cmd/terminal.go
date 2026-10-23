package cmd

import (
	"os"
	"strings"
	"sync"

	"github.com/c-bata/go-prompt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

// terminalState — исходный режим терминала, сохранённый до запуска оболочки.
//
// Восстанавливать режим самостоятельно приходится из-за ошибки в go-prompt
// v0.2.6 (последняя версия, библиотека не развивается): SetRaw получает
// указатель на сохранённый исходный режим и меняет его прямо по этому
// указателю. После этого «исходным» оказывается уже сырой режим, и Restore
// восстанавливает сырой. Итог — после выхода из оболочки терминал остаётся
// без эха и без Ctrl+C, а команды внутри оболочки выполняются в сыром
// режиме, где Ctrl+C не порождает сигнала.
type terminalState struct {
	mu    sync.Mutex
	fd    int
	state *term.State
	file  *os.File
}

// saveTerminal запоминает режим терминала. Возвращает nil, если терминала
// нет — тогда и восстанавливать нечего.
func saveTerminal() *terminalState {
	// /dev/tty — тот же терминал, с которым работает go-prompt, даже если
	// stdin перенаправлен. На Windows его нет, там используется stdin.
	if file, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		if state, err := term.GetState(int(file.Fd())); err == nil {
			return &terminalState{fd: int(file.Fd()), state: state, file: file}
		}
		file.Close()
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	state, err := term.GetState(fd)
	if err != nil {
		return nil
	}
	return &terminalState{fd: fd, state: state}
}

// restore возвращает терминал в исходный режим. Вызывать можно сколько
// угодно раз и на nil.
func (t *terminalState) restore() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = term.Restore(t.fd, t.state)
}

// restoringParser — ввод go-prompt, который при каждом снятии сырого режима
// возвращает терминал в исходное состояние.
//
// go-prompt вызывает TearDown на всех своих путях: перед выполнением каждой
// команды, при выходе по Ctrl+D, при SIGINT/SIGTERM/SIGQUIT и при панике в
// команде (через defer). Поэтому исправление в одном месте закрывает их все.
type restoringParser struct {
	prompt.ConsoleParser
	terminal *terminalState
}

func (p *restoringParser) TearDown() error {
	err := p.ConsoleParser.TearDown()
	p.terminal.restore()
	return err
}

// resetFlags возвращает флаги всех команд к значениям по умолчанию.
//
// cobra не сбрасывает флаги между запусками: в оболочке, где одна и та же
// команда выполняется много раз, значение из прошлой строки переходило в
// следующую. После `validate --help` обычный `validate` снова печатал
// справку, а `--force`, однажды переданный `rollback` или `patch`,
// действовал и на все следующие вызовы.
func resetFlags(cmd *cobra.Command) {
	reset := func(flag *pflag.Flag) {
		if !flag.Changed {
			return
		}
		if slice, ok := flag.Value.(pflag.SliceValue); ok {
			_ = slice.Replace(defaultSlice(flag.DefValue))
		} else {
			_ = flag.Value.Set(flag.DefValue)
		}
		flag.Changed = false
	}

	cmd.Flags().VisitAll(reset)
	cmd.PersistentFlags().VisitAll(reset)
	for _, sub := range cmd.Commands() {
		resetFlags(sub)
	}
}

// defaultSlice разбирает значение по умолчанию флага-списка вида "[a,b]".
func defaultSlice(value string) []string {
	value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	if value == "" {
		return []string{}
	}
	return strings.Split(value, ",")
}
