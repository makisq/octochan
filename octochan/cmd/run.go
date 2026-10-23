package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"octochan/config"
	"octochan/lang"

	"github.com/spf13/cobra"
)

// Реестр алиасов на процесс. Заполняется лениво: большинству команд
// алиасы не нужны, а разбор файлов стоит времени.
var (
	aliasRegistryOnce sync.Once
	aliasRegistry     *lang.Registry
	aliasLoadErrs     []error
)

// aliases возвращает реестр алиасов и ошибки, накопленные при загрузке.
//
// Каждый вызов получает свою копию: сценарий регистрирует в реестре свои
// алиасы, и в интерактивной оболочке второй запуск того же файла иначе
// упал бы на «алиас объявлен дважды».
//
// Источники:
//
//	~/.octochan/aliases/*.ochan        алиасы пользователя, без префикса
//	~/.octochan/modules/<пакет>/*.ochan  пакет алиасов, доступен как пакет.алиас
//	~/.octochan/modules/<пакет>.ochan    однофайловый пакет
//
// Пакет алиасов — второй вид модуля наряду с .hcplugin: Go-плагин расширяет
// octochan новыми командами, а пакет алиасов — готовыми сценариями поверх
// уже существующих операций. Второй безопаснее: это текст, который видно
// глазами, и он умеет ровно то, что дают встроенные функции.
func aliases() (*lang.Registry, []error) {
	aliasRegistryOnce.Do(func() {
		fm, err := mustFileManager()
		if err != nil {
			aliasRegistry = lang.NewRegistry()
			aliasLoadErrs = append(aliasLoadErrs, err)
			return
		}
		aliasRegistry, aliasLoadErrs = loadAliasRegistry(fm.AliasesDir(), fm.ModulesDir())
	})

	return aliasRegistry.Clone(), aliasLoadErrs
}

// loadAliasRegistry читает библиотеку алиасов из каталогов. Её же читает
// процесс песочницы: рабочих каталогов ochan, кроме этих двух, он не видит.
func loadAliasRegistry(aliasesDir, modulesDir string) (*lang.Registry, []error) {
	registry := lang.NewRegistry()
	var errs []error

	if err := registry.LoadDir("", aliasesDir); err != nil {
		errs = append(errs, err)
	}

	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		return registry, errs
	}

	for _, entry := range entries {
		path := filepath.Join(modulesDir, entry.Name())

		switch {
		case entry.IsDir():
			if err := registry.LoadDir(entry.Name(), path); err != nil {
				errs = append(errs, err)
			}
		case filepath.Ext(entry.Name()) == lang.AliasExtension:
			pkg := strings.TrimSuffix(entry.Name(), lang.AliasExtension)
			if _, err := registry.LoadFile(pkg, path); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return registry, errs
}

// reportAliasErrors печатает проблемы загрузки, не прерывая работу:
// один сломанный пакет не должен блокировать остальные.
func reportAliasErrors(cmd *cobra.Command, errs []error) {
	for _, err := range errs {
		cmd.PrintErrf("внимание: алиасы: %v\n", err)
	}
}

var runOpts scriptOptions

var runCmd = &cobra.Command{
	Use:   "run <файл.ochan | алиас | -> [аргументы...]",
	Short: "Выполнить сценарий или алиас",
	Args:  cobra.MinimumNArgs(1),
	Long: "Если первый аргумент — существующий файл или «-» (stdin), выполняется он.\n" +
		"Иначе имя трактуется как алиас, а остальные аргументы — как его параметры.\n" +
		"Числа распознаются автоматически, всё остальное передаётся строкой.\n\n" +
		"Что сценарий изменил бы, не выполняя его: ochan plan файл.ochan.",
	Example: "ochan run сценарий.ochan\n" +
		"ochan run синхронизировать prod.conf test.conf\n" +
		"ochan run --sandbox --root ./configs сценарий.ochan",
	RunE: func(cmd *cobra.Command, args []string) error {
		box, err := runOpts.build()
		if err != nil {
			return err
		}

		registry, errs := aliases()
		reportAliasErrors(cmd, errs)

		target := args[0]
		if box != nil {
			return runSandboxed(cmd, box, target, args[1:])
		}

		interp := lang.NewInterpreter(cmd.Context(), registry, cmd.OutOrStdout())
		interp.SetErr(cmd.ErrOrStderr())
		interp.SetProtected(protectedPaths())

		if target == "-" || config.FileExists(target) {
			src, err := runOpts.source(target)
			if err != nil {
				return err
			}
			file, err := prepareScript(registry, src, false)
			if err != nil {
				return err
			}
			return interp.RunFile(file)
		}

		values := make([]lang.Value, 0, len(args)-1)
		for _, arg := range args[1:] {
			values = append(values, lang.FromText(arg))
		}

		result, err := interp.CallAlias(target, values)
		if err != nil {
			return err
		}
		return printResult(cmd, result)
	},
}

// runSandboxed выполняет сценарий или алиас в процессе песочницы.
func runSandboxed(cmd *cobra.Command, box *lang.Sandbox, target string, args []string) error {
	spec := isolatedSpec{Mode: "call", Alias: target, Args: args}
	if target == "-" || config.FileExists(target) {
		src, err := runOpts.source(target)
		if err != nil {
			return err
		}
		spec = isolatedSpec{Mode: "run", Name: src.Name, Content: src.Content}
	}
	spec.setBox(box)

	result, err := isolated(cmd, spec, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	if err := result.err(); err != nil {
		return err
	}
	if result.HasValue {
		cmd.Println(result.Value)
	}
	return nil
}

// prepareScript разбирает сценарий и регистрирует объявленные в нём
// алиасы. Перед планом сценарий проверяется целиком.
func prepareScript(registry *lang.Registry, src source, check bool) (*lang.File, error) {
	if check {
		return loadScript(registry, src)
	}

	file, err := lang.Parse(src.Name, src.Content)
	if err != nil {
		return nil, err
	}
	for _, decl := range file.Aliases {
		if err := registry.Add("", decl); err != nil {
			return nil, err
		}
	}
	return file, nil
}

func printResult(cmd *cobra.Command, result lang.Value) error {
	// null — обычный результат алиаса без return, печатать его незачем.
	if _, isNull := result.(lang.NullValue); isNull {
		return nil
	}
	cmd.Println(lang.Display(result))
	return nil
}

var evalExpr string

var evalCmd = &cobra.Command{
	Use:   "eval <выражение>",
	Short: "Вычислить выражение языка алиасов",
	// Аргументы склеиваются обратно в одну строку: оболочки по-разному
	// разбивают выражение по пробелам и запятым, и требовать от пользователя
	// точного экранирования ради одной команды незачем.
	Args:    cobra.ArbitraryArgs,
	Example: "ochan eval \"diff('prod.conf', 'test.conf')\"",
	Long: "Строки в языке можно писать и в одинарных кавычках — это удобно,\n" +
		"когда двойные съедает оболочка (например, Windows PowerShell).",
	RunE: func(cmd *cobra.Command, args []string) error {
		source := evalExpr
		if source == "" && len(args) > 0 {
			source = strings.Join(args, " ")
		}
		if source == "" {
			return fmt.Errorf("укажите выражение")
		}

		registry, errs := aliases()
		reportAliasErrors(cmd, errs)

		file, err := lang.Parse("<eval>", source)
		if err != nil {
			return err
		}
		for _, decl := range file.Aliases {
			if err := registry.Add("", decl); err != nil {
				return err
			}
		}

		interp := lang.NewInterpreter(cmd.Context(), registry, cmd.OutOrStdout())
		interp.SetErr(cmd.ErrOrStderr())
		interp.SetProtected(protectedPaths())

		// Одиночное выражение печатаем: иначе eval был бы бесполезен.
		if len(file.Stmts) == 1 {
			if stmt, ok := file.Stmts[0].(*lang.ExprStmt); ok {
				result, err := interp.EvalExpr(stmt.Expr)
				if err != nil {
					return err
				}
				return printResult(cmd, result)
			}
		}
		return interp.RunFile(file)
	},
}

var aliasesCmd = &cobra.Command{
	Use:   "aliases [пакет]",
	Short: "Показать доступные алиасы и встроенные функции",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		registry, errs := aliases()
		reportAliasErrors(cmd, errs)

		filter := ""
		if len(args) == 1 {
			filter = args[0]
		}

		infos := registry.List()
		shown := 0
		currentModule := "\x00" // заведомо не совпадёт с первым пакетом

		for _, info := range infos {
			if filter != "" && info.Module != filter {
				continue
			}

			if info.Module != currentModule {
				currentModule = info.Module
				title := "Алиасы пользователя"
				if info.Module != "" {
					title = "Пакет " + info.Module
					if pkg, ok := registry.Package(info.Module); ok {
						title += " " + pkg.Version
						if pkg.Description != "" {
							title += " — " + pkg.Description
						}
					} else {
						title += " (без версии)"
					}
				}
				cmd.Printf("\n%s\n", title)
			}

			cmd.Printf("  %-44s %s\n", info.Signature(), info.Doc)
			shown++
		}

		if shown == 0 {
			fm, err := mustFileManager()
			if err == nil && filter == "" {
				cmd.Printf("Алиасов нет. Положите файлы %s в %s\n",
					lang.AliasExtension, fm.AliasesDir())
			} else if filter != "" {
				return fmt.Errorf("пакет алиасов %q не найден", filter)
			}
		}

		if filter != "" {
			return nil
		}

		cmd.Println("\nВстроенные функции")
		interp := lang.NewInterpreter(cmd.Context(), registry, cmd.OutOrStdout())
		builtins := interp.Builtins()
		for _, name := range interp.BuiltinNames() {
			cmd.Printf("  %-44s %s\n", builtins[name].Signature(), builtins[name].Doc)
		}
		return nil
	},
}

func init() {
	evalCmd.Flags().StringVarP(&evalExpr, "expr", "e", "", "выражение для вычисления")
	runOpts.register(runCmd)
}
