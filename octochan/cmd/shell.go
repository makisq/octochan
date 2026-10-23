package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"octochan/pluginhost"

	"github.com/c-bata/go-prompt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var shellCmd = &cobra.Command{
	Use:   "shell",
	Short: "Запустить интерактивную оболочку",
	Args:  cobra.NoArgs,
}

var commandHistory []string

func init() {
	shellCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return runInteractiveShell(cmd.Context())
	}
}

// shellTerminal — исходный режим терминала; восстанавливается при любом
// выходе из оболочки.
var shellTerminal *terminalState

// runInteractiveShell запускает REPL.
//
// Ручного управления терминалом через ioctl здесь нет: из-за него проект
// когда-то собирался только под Linux. Режим сохраняется и
// восстанавливается через golang.org/x/term — см. terminalState.
func runInteractiveShell(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Контекст оболочки не отменяется по Ctrl+C: прерывание касается
	// выполняемой команды, а не самой оболочки. Иначе первый же Ctrl+C
	// отменял бы контекст навсегда, и все следующие команды падали бы сразу.
	shellCtx := context.WithoutCancel(ctx)

	shellTerminal = saveTerminal()
	defer shellTerminal.restore()

	// Закрытое окно или kill -HUP: go-prompt этот сигнал не ловит, и без
	// обработки процесс завершился бы в сыром режиме терминала.
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)
	defer signal.Stop(hangup)
	go func() {
		if _, ok := <-hangup; ok {
			exitShell(129)
		}
	}()

	if err := startModuleWatcher(shellCtx); err != nil {
		fmt.Printf("⚠️ Горячая перезагрузка модулей недоступна: %v\n", err)
	}

	startTokenReminder(func(msg string) { fmt.Println("\n" + msg) })
	defer stopTokenReminder()

	fmt.Println("🐙 Добро пожаловать в OctoChan! Введите 'help' для списка команд, 'exit' для выхода.")

	options := []prompt.Option{
		prompt.OptionPrefix("ochan> "),
		prompt.OptionTitle("OctoChan Interactive Shell"),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.ControlD,
			Fn:  func(*prompt.Buffer) { exitShell(0) },
		}),
	}
	if shellTerminal != nil {
		options = append(options, prompt.OptionParser(&restoringParser{
			ConsoleParser: prompt.NewStandardInputParser(),
			terminal:      shellTerminal,
		}))
	}

	p := prompt.New(func(input string) { executeShellInput(shellCtx, input) }, completer, options...)
	p.Run()
	return nil
}

// exitShell завершает процесс из оболочки.
//
// go-prompt не умеет прерывать Run() изнутри executor'а, поэтому выход —
// это os.Exit. Значит, defer'ы не отработают, и всё нужно сделать здесь
// явно: погасить процессы плагинов и вернуть терминал в исходный режим.
// Ctrl+D вызывает выход прямо из обработчика клавиши, когда терминал ещё
// в сыром режиме, — без восстановления он таким бы и остался.
func exitShell(code int) {
	stopTokenReminder()
	pluginhost.GetModuleManager().Cleanup()
	shellTerminal.restore()
	os.Exit(code)
}

// executeShellInput выполняет одну строку оболочки.
func executeShellInput(ctx context.Context, input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return
	}

	// Перезагрузка модулей применяется здесь, в главной горутине,
	// а не в горутине наблюдателя.
	applyModuleChanges(func(msg string) { fmt.Println(msg) })

	switch input {
	case "exit", "quit":
		fmt.Println("Goodbye! 👋")
		exitShell(0)
		return
	case "!clear", "clr":
		fmt.Print("\x1b[2J\x1b[H")
		return
	case "!history", "his":
		showCommandHistory()
		return
	}

	if !strings.HasPrefix(input, "!") {
		commandHistory = append(commandHistory, input)
	}

	args, err := splitArgs(input)
	if err != nil {
		fmt.Printf("❌ %v\n", err)
		return
	}

	if terminated := runShellCommand(ctx, args); terminated {
		exitShell(143)
	}
}

// runShellCommand выполняет одну команду в собственном контексте и
// сообщает, просили ли процесс завершиться.
//
// Ctrl+C отменяет только эту команду. Если команда на отмену не реагирует,
// второй Ctrl+C завершает оболочку — иначе из зависшей команды было бы не
// выйти. SIGTERM во время команды завершает оболочку после неё: раньше он
// просто отменял общий контекст, и процесс продолжал жить.
func runShellCommand(base context.Context, args []string) (terminated bool) {
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	var stop atomic.Bool
	done := make(chan struct{})
	defer close(done)
	go func() {
		interrupted := false
		for {
			select {
			case sig := <-signals:
				if sig == syscall.SIGTERM {
					stop.Store(true)
				}
				if interrupted {
					fmt.Println("\n⚠️ Команда не остановилась — выход из оболочки")
					exitShell(130)
				}
				interrupted = true
				cancel()
			case <-done:
				return
			}
		}
	}()

	// cobra запоминает контекст подкоманды при первом запуске и больше его
	// не меняет, а флаги не сбрасывает вовсе. Оба состояния переходили бы
	// из строки в строку.
	setContext(rootCmd, ctx)
	resetFlags(rootCmd)

	rootCmd.SetArgs(args)
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println("⚠️ Прервано")
		} else {
			fmt.Printf("❌ %v\n", err)
		}
	}
	return stop.Load()
}

// setContext передаёт контекст всем командам дерева.
func setContext(cmd *cobra.Command, ctx context.Context) {
	cmd.SetContext(ctx)
	for _, sub := range cmd.Commands() {
		setContext(sub, ctx)
	}
}

// splitArgs разбивает строку на аргументы с учётом кавычек.
// Прежний strings.Fields ломал аргументы с пробелами, из-за чего
// `if config.conf port == 8080 "Порт корректен"` превращался в семь
// аргументов вместо пяти и падал на проверке ExactArgs(5).
func splitArgs(input string) ([]string, error) {
	var (
		args    []string
		current strings.Builder
		quote   rune
		hasWord bool
	)

	for _, r := range input {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			hasWord = true
		case r == ' ' || r == '\t':
			if hasWord {
				args = append(args, current.String())
				current.Reset()
				hasWord = false
			}
		default:
			current.WriteRune(r)
			hasWord = true
		}
	}

	if quote != 0 {
		return nil, fmt.Errorf("незакрытая кавычка %c", quote)
	}
	if hasWord {
		args = append(args, current.String())
	}
	return args, nil
}

func showCommandHistory() {
	if len(commandHistory) == 0 {
		fmt.Println("История пуста")
		return
	}

	fmt.Println("История команд:")
	for i, cmd := range commandHistory {
		fmt.Printf("%3d. %s\n", i+1, cmd)
	}
}

// ---------------------------------------------------------------------------
// Автодополнение
// ---------------------------------------------------------------------------

func completer(d prompt.Document) []prompt.Suggest {
	args := strings.Fields(d.TextBeforeCursor())
	if len(args) <= 1 && !strings.HasSuffix(d.TextBeforeCursor(), " ") {
		return completeCommands(d)
	}

	target, _, err := rootCmd.Find(args)
	if err != nil || target == nil {
		return nil
	}

	if strings.HasPrefix(d.GetWordBeforeCursor(), "-") {
		return completeFlags(target, d)
	}

	// Подкоманды текущей команды.
	var suggests []prompt.Suggest
	for _, sub := range target.Commands() {
		if sub.Hidden {
			continue
		}
		suggests = append(suggests, prompt.Suggest{Text: sub.Name(), Description: sub.Short})
	}
	return prompt.FilterHasPrefix(suggests, d.GetWordBeforeCursor(), true)
}

func completeCommands(d prompt.Document) []prompt.Suggest {
	var suggests []prompt.Suggest
	for _, c := range rootCmd.Commands() {
		if c.Hidden {
			continue
		}
		suggests = append(suggests, prompt.Suggest{Text: c.Name(), Description: c.Short})
	}

	suggests = append(suggests,
		prompt.Suggest{Text: "exit", Description: "Выйти из оболочки"},
		prompt.Suggest{Text: "!clear", Description: "Очистить экран"},
		prompt.Suggest{Text: "!history", Description: "История команд"},
	)

	return prompt.FilterHasPrefix(suggests, d.GetWordBeforeCursor(), true)
}

func completeFlags(cmd *cobra.Command, d prompt.Document) []prompt.Suggest {
	var suggests []prompt.Suggest

	add := func(flag *pflag.Flag) {
		suggests = append(suggests, prompt.Suggest{Text: "--" + flag.Name, Description: flag.Usage})
		if flag.Shorthand != "" {
			suggests = append(suggests, prompt.Suggest{Text: "-" + flag.Shorthand, Description: flag.Usage})
		}
	}

	cmd.LocalFlags().VisitAll(add)
	cmd.InheritedFlags().VisitAll(add)

	return prompt.FilterHasPrefix(suggests, d.GetWordBeforeCursor(), false)
}
