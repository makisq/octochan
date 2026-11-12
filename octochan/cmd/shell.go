package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"

	"ochan/i18n"
	"ochan/pluginhost"
	"ochan/workspace"

	"github.com/c-bata/go-prompt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/term"
)

var shellCmd = &cobra.Command{
	Use:   "shell",
	Short: "Запустить оболочку: интерактивную на терминале, построчную без него",
	Long: "На терминале — интерактивная оболочка с автодополнением и историей.\n\n" +
		"Без терминала — в конвейере, в задании CI, с вводом из файла — оболочка\n" +
		"читает stdin построчно и выполняет те же строки: команды ochan и\n" +
		"инструкции языка алиасов, с переменными между строками и блоками\n" +
		"{ … } в несколько строк. Пустые строки и строки с # пропускаются, exit\n" +
		"кончает ввод. Ошибки идут в stderr, вывод команд — в stdout; код\n" +
		"возврата — 1, если хоть одна строка кончилась ошибкой. С\n" +
		"--stop-on-error оболочка останавливается на первой. В историю такие\n" +
		"строки не пишутся, а stdin командам недоступен: из него читаются сами\n" +
		"команды.",
	Example: "ochan shell\n" +
		"echo 'logs rank --min warning' | ochan shell\n" +
		"ochan shell --stop-on-error < проверки.txt",
	Args: cobra.NoArgs,
}

// shellStopOnError — построчная оболочка останавливается на первой ошибке.
var shellStopOnError bool

// interactiveTerminal — у оболочки есть терминал: и ввод, и вывод. Без него
// go-prompt открывал /dev/tty и паниковал (no such device or address).
func interactiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// Построчная оболочка: shellLines — она работает; shellErrors — сколько
// строк кончилось ошибкой.
var (
	shellLines  bool
	shellErrors int
)

// shellFail сообщает об ошибке строки оболочки и считает её. Без терминала
// ошибка идёт в stderr — stdout остаётся выводу команд, — а по счёту
// решается код возврата.
func shellFail(err error) {
	shellErrors++
	out := os.Stdout
	if shellLines {
		out = os.Stderr
	}
	fmt.Fprintf(out, i18n.T("ошибка: %v\n"), err)
}

// runLineShell — оболочка без терминала: строки читаются из in и
// выполняются так же, как набранные. Ошибка — если строка кончилась
// ошибкой (с stopOnError — на первой же) или ввод оборвался внутри блока.
func runLineShell(ctx context.Context, in io.Reader, stopOnError bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	shellLines, stdinTaken, shellErrors, shellActive = true, true, 0, true
	defer func() { shellLines, stdinTaken, shellActive = false, false, false }()
	// Ввод кончился в режиме нейросети — режим закрывается вместе с ним.
	defer closeShellChat()
	shellReset()

	failed := 0
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maxTextLine)
	// Метка порядка байт в начале ввода: её ставят PowerShell в конвейере и
	// редакторы Windows в файле.
	bom := string(rune(0xFEFF))
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		if n == 1 {
			line = strings.TrimPrefix(line, bom)
		}
		if input := strings.TrimSpace(line); shellPending == "" {
			if input == "" || strings.HasPrefix(input, "#") {
				continue
			}
			if input == "exit" || input == "quit" {
				break
			}
		}
		before := shellErrors
		// Строка выполняется в своём контексте: Ctrl+C и SIGTERM прерывают
		// её, а оболочка узнаёт о них по общему контексту и кончает ввод.
		executeShellInput(context.WithoutCancel(ctx), line)
		if ctx.Err() != nil {
			return context.Canceled
		}
		if shellErrors > before {
			failed++
			if stopOnError {
				return i18n.Errorf("строка %d кончилась ошибкой — остановлено (--stop-on-error)", n)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("ввод оболочки: %w", err)
	}
	if shellPending != "" {
		shellPending = ""
		return i18n.Errorf("ввод кончился внутри блока: не хватает закрывающей скобки")
	}
	if failed > 0 {
		return i18n.Errorf("строк с ошибкой: %d", failed)
	}
	return nil
}

// commandHistory — команды оболочки: прежних запусков и этого.
var commandHistory []string

// historySecrets — пароли в ссылках и строках подключения. В историю на
// диске они не попадают: postgres://dba:пароль@db1 становится
// postgres://dba:***@db1.
var historySecrets = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(://[^/@:\s]+:)[^@\s]+@`), "${1}***@"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd)=([^\s'"]+)`), "${1}=***"},
}

func redactSecrets(line string) string {
	for _, s := range historySecrets {
		line = s.re.ReplaceAllString(line, s.with)
	}
	return line
}

// loadShellHistory читает историю прежних запусков.
func loadShellHistory() {
	fm, err := mustFileManager()
	if err != nil {
		return
	}
	state, err := workspace.LoadState(fm.StatePath())
	if err != nil {
		fmt.Println(i18n.Sprintf("внимание: история оболочки недоступна: %v", err))
		return
	}
	commandHistory = state.History
}

// rememberCommand запоминает команду и сразу пишет её на диск: выход из
// оболочки — os.Exit, и отложенная запись не успела бы.
//
// Строка, начатая с пробела, не запоминается — как в bash с
// HISTCONTROL=ignorespace: так набирают то, чему не место в истории.
func rememberCommand(raw, input string) {
	// Строки конвейера и CI — не то, что набирал человек: в историю не идут.
	if shellLines || strings.HasPrefix(raw, " ") || strings.HasPrefix(input, "!!") {
		return
	}
	input = redactSecrets(input)
	if n := len(commandHistory); n > 0 && commandHistory[n-1] == input {
		return
	}
	commandHistory = append(commandHistory, input)

	fm, err := mustFileManager()
	if err != nil {
		return
	}
	if err := workspace.AppendHistory(fm.StatePath(), input, viper.GetInt("shell.history_size")); err != nil {
		fmt.Println(i18n.Sprintf("внимание: история оболочки не сохранена: %v", err))
	}
}

func init() {
	shellCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if shellLogo && shellNoLogo {
			return i18n.Errorf("--logo и --no-logo вместе: выберите одно")
		}
		if shellLines {
			return i18n.Errorf("оболочка уже читает команды из stdin: вторая читала бы из него же")
		}
		if !interactiveTerminal() {
			return runLineShell(cmd.Context(), os.Stdin, shellStopOnError)
		}
		if err := rememberLogo(); err != nil {
			cmd.PrintErrln(i18n.Sprintf("внимание: выбор логотипа не сохранён: %v", err))
		}
		return runInteractiveShell(cmd.Context())
	}
	shellCmd.Flags().BoolVar(&shellNoLogo, "no-logo", false, "не показывать логотип при входе — и в следующих сессиях")
	shellCmd.Flags().BoolVar(&shellLogo, "logo", false, "показывать логотип при входе — и в следующих сессиях, даже если shell.logo: false")
	shellCmd.Flags().BoolVar(&shellStopOnError, "stop-on-error", false, "без терминала: остановиться на первой строке с ошибкой")
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
	shellActive = true
	defer func() { shellActive = false }()
	defer closeShellChat()

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
		fmt.Println(i18n.Sprintf("внимание: горячая перезагрузка модулей недоступна: %v", err))
	}

	startTokenReminder(func(msg string) { fmt.Println("\n" + msg) })
	defer stopTokenReminder()

	// Логотип — только здесь, при входе в оболочку: у отдельных команд он
	// мешал бы выводу и попадал бы в журналы.
	printShellLogo(os.Stdout)
	fmt.Println(i18n.T("Добро пожаловать в OctoChan. Введите 'help' для списка команд, 'exit' для выхода."))

	loadShellHistory()
	refreshShellPrefix()
	options := []prompt.Option{
		prompt.OptionPrefix("ochan> "),
		// Стрелка вверх достаёт и команды прежних запусков.
		prompt.OptionHistory(append([]string(nil), commandHistory...)),
		// В приглашении видна роль: после set role slave легко забыть,
		// почему команды перестали выполняться.
		prompt.OptionLivePrefix(func() (string, bool) { return shellPrefix, true }),
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
	closeShellChat()
	stopTokenReminder()
	pluginhost.GetModuleManager().Cleanup()
	shellTerminal.restore()
	os.Exit(code)
}

// executeShellInput выполняет одну строку оболочки.
func executeShellInput(ctx context.Context, raw string) {
	input := strings.TrimSpace(raw)
	// Блок инструкции ещё не закрыт: строка — его продолжение.
	if shellPending != "" {
		if input != "" {
			rememberCommand(raw, input)
		}
		if src, done := continueStatement(raw); done {
			if terminated := runShellStatement(ctx, src); terminated {
				exitShell(143)
			}
			refreshShellPrefix()
		}
		return
	}
	// Комментарий — не инструкция: права на язык ему не нужны.
	if input == "" || strings.HasPrefix(input, "#") {
		return
	}

	// Режим нейросети (opmode llm): строка — реплика модели; команда ochan
	// и инструкция языка — с «?» в начале.
	if shellChat != nil {
		command := chatModeLine(ctx, input)
		if command == "" {
			rememberCommand(raw, input)
			refreshShellPrefix()
			return
		}
		raw, input = command, command
	}

	// Перезагрузка модулей применяется здесь, в главной горутине,
	// а не в горутине наблюдателя.
	applyModuleChanges(func(msg string) { fmt.Println(msg) })

	switch input {
	case "exit", "quit":
		fmt.Println(i18n.T("До встречи!"))
		exitShell(0)
		return
	case "!!clear", "clr":
		fmt.Print("\x1b[2J\x1b[H")
		return
	case "!!history", "his":
		showCommandHistory()
		return
	case "!!reset":
		shellReset()
		fmt.Println(i18n.T("Сеанс языка сброшен: переменных и алиасов оболочки больше нет"))
		return
	case "!clear", "!history":
		shellErrors++
		fmt.Println(i18n.Sprintf("ошибка: команды оболочки пишутся с «!!»: !%s — одиночный «!» теперь означает «не»", input))
		return
	}

	rememberCommand(raw, input)

	// Инструкция языка алиасов: язык в оболочке тот же, что в файле.
	if shellStatement(input) {
		if blockDepth(input) > 0 {
			shellPending = input
			shellPrefix = "...     "
			return
		}
		if terminated := runShellStatement(ctx, input); terminated {
			exitShell(143)
		}
		refreshShellPrefix()
		return
	}

	args, err := splitArgs(input)
	if err != nil {
		shellFail(err)
		return
	}

	if terminated := runShellCommand(ctx, args); terminated {
		exitShell(143)
	}
	refreshShellPrefix()
}

// shellPrefix — приглашение оболочки с текущей ролью.
var shellPrefix = "ochan> "

// refreshShellPrefix перечитывает роль после каждой команды, а не на
// каждое нажатие клавиши: приглашение перерисовывается часто.
func refreshShellPrefix() {
	if shellLines {
		return // без терминала приглашения нет
	}
	if shellChat != nil {
		shellPrefix = "llm> "
		return
	}
	store, _, err := loadRoles()
	if err != nil {
		shellPrefix = "ochan> "
		return
	}
	role, _, err := activeRole(store)
	if err != nil {
		shellPrefix = "ochan> "
		return
	}
	shellPrefix = fmt.Sprintf("ochan[%s]> ", role.Name)
}

// runShellCommand выполняет одну команду в собственном контексте и
// сообщает, просили ли процесс завершиться.
//
// Ctrl+C отменяет только эту команду. Если команда на отмену не реагирует,
// второй Ctrl+C завершает оболочку — иначе из зависшей команды было бы не
// выйти. SIGTERM во время команды завершает оболочку после неё: раньше он
// просто отменял общий контекст, и процесс продолжал жить.
func runShellCommand(base context.Context, args []string) (terminated bool) {
	return runInterruptible(base, func(ctx context.Context) {
		// cobra запоминает контекст подкоманды при первом запуске и больше
		// его не меняет, а флаги не сбрасывает вовсе. Оба состояния
		// переходили бы из строки в строку.
		setContext(rootCmd, ctx)
		resetFlags(rootCmd)

		rootCmd.SetArgs(args)
		if out, ok := reply(args); ok {
			fmt.Print(out)
			return
		}
		if err := singleBang(args); err != nil {
			shellFail(err)
			return
		}
		if err := rootCmd.ExecuteContext(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Println(i18n.T("Прервано"))
			} else {
				shellFail(err)
			}
		}
	})
}

// runInterruptible выполняет строку оболочки — команду или инструкцию — в
// собственном контексте и сообщает, просили ли процесс завершиться.
func runInterruptible(base context.Context, run func(ctx context.Context)) (terminated bool) {
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
					fmt.Println(i18n.T("\nКоманда не остановилась — выход из оболочки"))
					exitShell(130)
				}
				interrupted = true
				cancel()
			case <-done:
				return
			}
		}
	}()

	run(ctx)
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
		fmt.Println(i18n.T("История пуста"))
		return
	}

	fmt.Println(i18n.T("История команд:"))
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
		prompt.Suggest{Text: "!!clear", Description: "Очистить экран"},
		prompt.Suggest{Text: "!!history", Description: "История команд"},
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
