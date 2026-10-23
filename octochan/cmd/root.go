// Package cmd содержит команды CLI octochan.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"octochan/i18n"
	"octochan/pluginhost"

	"github.com/spf13/cobra"
)

// Version — версия ochan. При сборке релиза задаётся через
// -ldflags "-X octochan/cmd.Version=…"; ochan --version печатает её.
var Version = "0.1.0-alpha"

var (
	cfgFile string
	verbose bool
)

var rootCmd = &cobra.Command{
	Use:   "ochan",
	Short: "Минималистичный оркестратор для командной строки и CI",
	Long: `Octo-chan — минималистичный оркестратор для командной строки и CI.
Сценарии на языке алиасов со строгими типами, план до выполнения, заявки
с согласованием, откат, роли и серверный режим. Из коробки работает с
конфигурациями (YAML, JSON, ENV, conf, живой PostgreSQL) — остальное
подключается модулями и пакетами алиасов.`,
	SilenceErrors: true,
	SilenceUsage:  true,
}

func init() {
	// RunE назначается здесь, а не в литерале: иначе rootCmd, helpCmd и
	// shellCmd ссылаются друг на друга через замыкания и Go отвергает
	// пакет с "initialization cycle".
	rootCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		// Без аргументов на терминале открываем интерактивную оболочку,
		// в конвейере — показываем справку.
		if stdinIsPipe() {
			return cmd.Help()
		}
		return runInteractiveShell(cmd.Context())
	}
	helpCmd.RunE = runHelp

	// Права проверяются перед каждой командой — и в командной строке,
	// и в оболочке: там каждая строка тоже проходит через rootCmd.
	rootCmd.PersistentPreRunE = authorize

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"путь до конфига (по умолчанию ~/.octochan/config.yaml)")
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false,
		"дублировать журнал в stderr")

	rootCmd.Version = Version
	rootCmd.SetVersionTemplate("ochan {{.Version}}\n")

	rootCmd.SetHelpCommand(helpCmd)
	rootCmd.AddCommand(
		helpCmd,
		diffCmd,
		patchCmd,
		applyCmd,
		statusCmd,
		servicesCmd,
		requestCmd,
		requestsCmd,
		approveCmd,
		rejectCmd,
		validateCmd,
		findCmd,
		statsCmd,
		printCmd,
		teeCmd,
		ifCmd,
		elseCmd,
		authCmd,
		timerCmd,
		logsCmd,
		installCmd,
		listModulesCmd,
		inspectCmd,
		saveScenarioCmd,
		runCmd,
		evalCmd,
		aliasesCmd,
		checkCmd,
		fpCmd,
		roleCmd,
		langCmd,
		setCmd,
		backCmd,
		serveCmd,
		tlsCmd,
		connectCmd,
		remoteCmd,
		planCmd,
		historyCmd,
		rollbackCmd,
		shellCmd,
	)
}

// Execute запускает CLI и возвращает код выхода процесса.
//
// Прежняя версия печатала ошибку и завершалась с кодом 0, поэтому любой
// вызов из CI считался успешным. Плюс defer с Cleanup стоял в main, а часть
// путей выполнения уходила через os.Exit — процессы плагинов оставались жить.
func Execute() int {
	// cobra по умолчанию пишет Print/Printf/Println в stderr (OutOrStderr
	// возвращает os.Stderr, пока выход не задан явно). Без этой строки весь
	// полезный вывод уходил в stderr, и конвейеры вида
	// `ochan print a.conf | ochan diff - b.conf` получали пустой stdin,
	// а перенаправление в файл давало пустой файл.
	rootCmd.SetOut(os.Stdout)
	rootCmd.SetErr(os.Stderr)

	if err := initConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		return 1
	}
	if err := initLogger(); err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		return 1
	}

	defer pluginhost.GetModuleManager().Cleanup()

	// Команды модулей регистрируются до Execute: cobra выбирает команду
	// раньше, чем отрабатывают инициализаторы из OnInitialize, поэтому
	// прежняя схема не давала вызвать ни одну команду плагина.
	loadModules()
	// Язык — после модулей: язык может прийти из модуля.
	applyLanguage()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, i18n.T("\nПрервано пользователем"))
			return 130
		}
		fmt.Fprintf(os.Stderr, i18n.T("ошибка: %v\n"), err)
		return 1
	}
	return 0
}

var helpCmd = &cobra.Command{
	Use:   "help [команда]",
	Short: "Отобразить список всех доступных команд",
}

func runHelp(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		target, _, err := rootCmd.Find(args)
		if err != nil {
			return err
		}
		return target.Help()
	}

	cmd.Println(i18n.T("Доступные команды:"))
	cmd.Println("------------------")
	for _, subCmd := range rootCmd.Commands() {
		if subCmd.Hidden {
			continue
		}
		cmd.Printf("%-24s %s\n", subCmd.Name(), subCmd.Short)
		if subCmd.Example != "" {
			for _, line := range strings.Split(subCmd.Example, "\n") {
				cmd.Printf(i18n.T("%-24s Пример: %s\n"), "", line)
			}
		}
	}
	return nil
}
