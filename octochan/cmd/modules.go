package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ochan/core/i18n"
	"ochan/core/rbac"
	"ochan/core/workspace"
	"ochan/modules/pluginhost"
	"ochan/modules/runnerhost"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// moduleCommandAnnotation помечает команды, пришедшие из плагинов.
const moduleCommandAnnotation = "source"

var modulesLoadOnce sync.Once

// loadModules поднимает плагины и регистрирует их команды.
//
// Вызывается из Execute до rootCmd.Execute: прежде loadModuleCommands()
// стоял в init() пакета (когда менеджер ещё пуст), а loadModules —
// в cobra.OnInitialize (когда команда уже выбрана), поэтому ни одна
// команда плагина до CLI не доходила.
func loadModules() {
	modulesLoadOnce.Do(func() {
		fm, err := mustFileManager()
		if err != nil {
			log.Printf("модули не загружены: %v", err)
			return
		}

		if err := pluginhost.GetModuleManager().LoadModulesFromDir(fm.ModulesDir()); err != nil {
			log.Printf("ошибки при загрузке модулей: %v", err)
			fmt.Fprintf(os.Stderr, "внимание: часть модулей не загружена: %v\n", err)
		}

		registerModuleCommands()
		for _, warning := range syncRunners() {
			fmt.Fprintf(os.Stderr, "внимание: %s\n", warning)
		}
		warnMissingSettings(func(msg string) { fmt.Fprintf(os.Stderr, "внимание: %s\n", msg) })
	})
}

// warnMissingSettings говорит о модулях без обязательных настроек.
func warnMissingSettings(out func(string)) {
	for _, info := range pluginhost.GetModuleManager().LoadedModules() {
		if warning := moduleMissing(info); warning != "" {
			out(warning)
		}
	}
}

// syncRunners приводит реестр сценариев к исполнителям загруженных
// модулей. Токен и настройки исполнителю выдаются при каждом вызове:
// заданные в оболочке через auth и config.yaml действуют сразу.
func syncRunners() []string {
	warnings := runnerhost.Register(pluginhost.GetModuleManager().Runners(), executorSupply)
	addExecutorFlags()
	return warnings
}

// registerModuleCommands синхронизирует команды плагинов с rootCmd.
// Выполняется только в главной горутине.
func registerModuleCommands() {
	existing := make(map[string]bool)
	for _, c := range rootCmd.Commands() {
		existing[c.Name()] = true
	}

	for _, moduleCmd := range pluginhost.GetModuleManager().GetCommands() {
		if existing[moduleCmd.Name()] {
			log.Printf("команда модуля %q пропущена: имя уже занято", moduleCmd.Name())
			continue
		}

		if moduleCmd.Annotations == nil {
			moduleCmd.Annotations = make(map[string]string)
		}
		moduleCmd.Annotations[moduleCommandAnnotation] = "module"

		rootCmd.AddCommand(moduleCmd)
		existing[moduleCmd.Name()] = true
		log.Printf("добавлена команда модуля: %s", moduleCmd.Name())
	}
	for _, warning := range syncClasses() {
		fmt.Fprintf(os.Stderr, "внимание: %s\n", warning)
	}
	// Справка команд модулей — на текущем языке, как и своя.
	localizeCommands(rootCmd)
}

// syncClasses передаёт rbac классы команд модулей — аннотацию class, с
// которой команда пришла по gRPC: право на группу класса открывает её сразу,
// без правки ядра. Набор пересобирается при каждой загрузке модулей, так что
// перезагруженный модуль приносит свои классы, а выгруженный — уносит.
func syncClasses() []string {
	declared := map[string]string{}
	var walk func(c *cobra.Command, prefix string)
	walk = func(c *cobra.Command, prefix string) {
		key := c.Name()
		if prefix != "" {
			key = prefix + "." + key
		}
		if class := strings.TrimSpace(c.Annotations[rbac.ClassAnnotation]); class != "" {
			declared[key] = class
		}
		for _, sub := range c.Commands() {
			walk(sub, key)
		}
	}
	for _, c := range rootCmd.Commands() {
		if c.Annotations[moduleCommandAnnotation] == "module" {
			walk(c, "")
		}
	}
	var warnings []string
	for _, skipped := range rbac.SetClasses(declared) {
		warnings = append(warnings, "класс команды модуля не принят — "+skipped)
	}
	return warnings
}

// refreshModuleCommands пересобирает список команд плагинов.
// Вызывается только из главной горутины оболочки.
func refreshModuleCommands() {
	var keep []*cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Annotations[moduleCommandAnnotation] != "module" {
			keep = append(keep, c)
		}
	}

	rootCmd.ResetCommands()
	rootCmd.SetHelpCommand(helpCmd)
	rootCmd.AddCommand(keep...)
	registerModuleCommands()
}

// ---------------------------------------------------------------------------
// Наблюдение за директорией модулей
// ---------------------------------------------------------------------------

// moduleChanges собирает пути изменившихся модулей.
//
// Наблюдатель больше не трогает rootCmd сам: раньше он вызывал
// RefreshCommands() из фоновой горутины, пока главная могла находиться
// внутри rootCmd.Execute() — гонка на структуре, которая не рассчитана
// на конкурентный доступ.
var moduleChanges = make(chan string, 32)

func startModuleWatcher(ctx context.Context) error {
	fm, err := mustFileManager()
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("не удалось создать наблюдатель: %w", err)
	}
	if err := watcher.Add(fm.ModulesDir()); err != nil {
		watcher.Close()
		return fmt.Errorf("не удалось наблюдать за %s: %w", fm.ModulesDir(), err)
	}

	go func() {
		defer watcher.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Ext(event.Name) != pluginhost.ModuleExtension {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				select {
				case moduleChanges <- event.Name:
				default: // очередь полна — событие будет учтено следующей перезагрузкой
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("наблюдатель модулей: %v", err)
			}
		}
	}()

	return nil
}

// applyModuleChanges перезагружает изменившиеся модули.
// Вызывается из главной горутины между командами оболочки.
func applyModuleChanges(out func(string)) {
	seen := make(map[string]bool)

	for {
		select {
		case path := <-moduleChanges:
			if seen[path] {
				continue
			}
			seen[path] = true
			reloadModule(path, out)
		default:
			if len(seen) > 0 {
				refreshModuleCommands()
				for _, warning := range syncRunners() {
					out("внимание: " + warning)
				}
			}
			return
		}
	}
}

func reloadModule(path string, out func(string)) {
	mm := pluginhost.GetModuleManager()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		mm.UnloadModule(path)
		out(fmt.Sprintf("Модуль выгружен: %s", filepath.Base(path)))
		return
	}

	if err := mm.LoadModule(path); err != nil {
		out(fmt.Sprintf("внимание: модуль %s не перезагружен: %v", filepath.Base(path), err))
		return
	}
	out(fmt.Sprintf("Модуль перезагружен: %s", filepath.Base(path)))
}

// ---------------------------------------------------------------------------
// Команды
// ---------------------------------------------------------------------------

var listModulesCmd = &cobra.Command{
	Use:   "modules",
	Short: "Показать установленные модули и пакеты алиасов",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if modulesJSON {
			return printModuleCommandsJSON(cmd)
		}
		fm, err := mustFileManager()
		if err != nil {
			return err
		}

		entries, err := os.ReadDir(fm.ModulesDir())
		if err != nil {
			return fmt.Errorf("не удалось прочитать директорию модулей: %w", err)
		}

		mm := pluginhost.GetModuleManager()
		loaded := make(map[string]pluginhost.ModuleInfo)
		for _, info := range mm.LoadedModules() {
			loaded[filepath.Base(info.Path)] = info
		}
		services := map[string][]string{}
		for _, r := range mm.Runners() {
			services[r.Module+"/"+r.Kind] = r.Services
		}

		var plugins []string
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == pluginhost.ModuleExtension {
				plugins = append(plugins, entry.Name())
			}
		}
		registry, errs := aliases()
		reportAliasErrors(cmd, errs)
		packages := map[string]int{}
		for _, info := range registry.List() {
			if info.Module != "" {
				packages[info.Module]++
			}
		}
		if len(plugins) == 0 && len(packages) == 0 {
			cmd.Printf("Модулей нет. Директория: %s\n", fm.ModulesDir())
			return nil
		}

		if len(plugins) > 0 {
			cmd.Println("Модули:")
		}
		for _, file := range plugins {
			info, ok := loaded[file]
			if !ok {
				cmd.Printf("  %-28s не загружен\n", file)
				continue
			}
			var parts []string
			if info.Commands > 0 {
				parts = append(parts, fmt.Sprintf("команд: %d", info.Commands))
			}
			if len(info.Languages) > 0 {
				parts = append(parts, "языки: "+strings.Join(info.Languages, ", "))
			}
			if len(info.Settings) > 0 {
				parts = append(parts, fmt.Sprintf("настроек: %d (%s.%s)", len(info.Settings), modulesKey, info.Name))
			}
			for _, kind := range info.Runners {
				parts = append(parts, fmt.Sprintf("исполнитель %s (%s)", kind,
					strings.Join(services[info.Name+"/"+kind], ", ")))
			}
			if len(parts) == 0 {
				parts = []string{"ничего не объявил"}
			}
			cmd.Printf("  %-28s %s v%s — %s\n", file, info.Name, info.Version, strings.Join(parts, "; "))
		}

		if len(packages) > 0 {
			cmd.Println("Пакеты алиасов:")
		}
		for _, name := range sortedPaths(packages) {
			version := "без версии"
			if pkg, ok := registry.Package(name); ok && pkg.Version != "" {
				version = "v" + pkg.Version
			}
			cmd.Printf("  %-28s %s — алиасов: %d\n", name, version, packages[name])
		}
		return nil
	},
}

var inspectJSON bool

var inspectCmd = &cobra.Command{
	Use:   "inspect <модуль.hcplugin>",
	Short: "Показать, что модуль объявляет, — без установки",
	Long: "Запускает модуль из файла отдельно — установленный или ещё нет — и\n" +
		"показывает, что он объявляет: имя и версию, размер и SHA-256 файла,\n" +
		"команды с классами прав, исполнителей с сервисами и операциями,\n" +
		"настройки (раздел modules.<модуль> в config.yaml) и откуда исполнитель\n" +
		"берёт токен. Набор команд текущей сессии не меняется. Файл, который не\n" +
		"модуль (архив пакета вместо программы, нет рукопожатия, паника при\n" +
		"запуске), — отказ с причиной и stderr модуля: так модуль проверяют до\n" +
		"!!install.",
	Example: "ochan inspect pgstat.hcplugin\nochan inspect --json pgstat.hcplugin",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := args[0]
		if filepath.Ext(path) != pluginhost.ModuleExtension {
			return fmt.Errorf("поддерживаются только %s модули", pluginhost.ModuleExtension)
		}
		stat, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("модуль недоступен: %w", err)
		}
		sum, err := pluginhost.FileChecksum(path)
		if err != nil {
			return err
		}

		// Модуль поднимается во временном менеджере и гасится сразу после
		// осмотра, чтобы не влиять на набор команд текущей сессии.
		inspector, err := pluginhost.OpenProbe(path)
		if err != nil {
			return err
		}
		defer inspector.Cleanup()
		infos := inspector.LoadedModules()
		if len(infos) == 0 {
			return fmt.Errorf("модуль не сообщил о себе ничего")
		}
		info := infos[0]
		report := inspectReport{Path: path, Name: info.Name, Version: info.Version, Size: stat.Size(),
			SHA256: hex.EncodeToString(sum), Commands: []inspectCommand{}, Runners: []inspectRunner{},
			Settings: []inspectSetting{}}
		report.Installed = installedState(info, sum, &report)
		commands := inspector.GetCommands()
		var walk func(c *cobra.Command, path []string)
		walk = func(c *cobra.Command, path []string) {
			path = append(append([]string(nil), path...), c.Name())
			class := ""
			for cur := c; cur != nil && class == ""; cur = cur.Parent() {
				class = strings.TrimSpace(cur.Annotations[rbac.ClassAnnotation])
			}
			if normal, err := rbac.NormalizeClass(class); err == nil {
				class = normal
			} else if class != "" {
				class = "не принят: " + err.Error()
			}
			if c.Runnable() {
				report.Commands = append(report.Commands, inspectCommand{Command: strings.Join(path, " "), Class: class,
					Short: c.Short, Flags: flagNames(c)})
			}
			for _, sub := range c.Commands() {
				walk(sub, path)
			}
		}
		for _, c := range commands {
			walk(c, nil)
		}
		for _, r := range inspector.Runners() {
			name := scenarioExecutorName(r.Kind)
			report.Runners = append(report.Runners, inspectRunner{Kind: r.Kind, Executor: name, Services: r.Services,
				Ops: r.Ops, TokenCommand: "ochan auth --executor " + name, TokenEnv: workspace.EnvExecutorToken(name),
				Settings: "executors." + name})
		}
		for _, s := range info.Settings {
			setting := inspectSetting{Key: moduleSettingKey(info.Name, s.Key),
				Default: s.Default, Description: s.Description, Required: s.Required && s.Default == ""}
			if setting.Required {
				// Тот же текст, что moduleMissing, — каким его увидит пользователь.
				setting.Warning = i18n.Sprintf("модуль %s: не задано обязательное — %s в %s", info.Name, setting.Key,
					viper.ConfigFileUsed())
			}
			report.Settings = append(report.Settings, setting)
		}
		if inspectJSON {
			data, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			cmd.Println(string(data))
			return nil
		}

		cmd.Println("\n=== Модуль ===")
		cmd.Printf("Файл:      %s\n", path)
		cmd.Printf("Имя:       %s\n", info.Name)
		cmd.Printf("Версия:    %s\n", info.Version)
		cmd.Printf("Размер:    %d байт\n", report.Size)
		cmd.Printf("SHA-256:   %s\n", report.SHA256)
		cmd.Printf("Установлен: %s\n", report.Installed)
		if len(report.Commands) > 0 {
			cmd.Printf("\nКоманды (%d):\n", len(report.Commands))
			for _, c := range report.Commands {
				class := c.Class
				if class == "" {
					class = "без класса: право по имени команды или all"
				} else {
					class = "класс " + class
				}
				short := c.Short
				if short == "" {
					short = "(нет описания)"
				}
				cmd.Printf("  %-24s %s — %s\n", c.Command, short, class)
				if c.Flags != "" {
					cmd.Printf("  %-24s флаги: %s\n", "", c.Flags)
				}
			}
		}
		for _, r := range report.Runners {
			cmd.Printf("\nИсполнитель %s (%s):\n", r.Kind, r.Executor)
			cmd.Printf("  сервисы:   %s\n", strings.Join(r.Services, ", "))
			cmd.Printf("  умеет:     %s\n", strings.Join(r.Ops, ", "))
			cmd.Printf("  токен:     %s (или %s)\n", r.TokenCommand, r.TokenEnv)
			cmd.Printf("  настройки: modules.%s, поверх — %s\n", info.Name, r.Settings)
		}
		if len(info.Settings) > 0 {
			cmd.Printf("\nНастройки (config.yaml, раздел %s.%s; значения — в Init модуля%s):\n", modulesKey, info.Name,
				map[bool]string{true: " и в каждом вызове команды и исполнителя", false: " и в каждом вызове команды"}[len(report.Runners) > 0])
			for _, line := range moduleSettingsLines(info) {
				cmd.Printf("  %s\n", line)
			}
			for _, s := range report.Settings {
				if !s.Required {
					continue
				}
				cmd.Printf("\n  Без %s ochan при установке, при каждом запуске и после !!reload пишет в stderr:\n", s.Key)
				cmd.Printf("    внимание: %s\n", s.Warning)
				cmd.Printf("  Выключить — %s: false. Модуль загружается и сам решает, работать ли:\n", warnMissingKey)
				cmd.Printf("  ошибка из его Init — отказ загрузиться с её текстом.\n")
			}
		}
		if len(report.Commands) == 0 && len(report.Runners) == 0 {
			cmd.Println("\nМодуль не объявил ни команд, ни исполнителей")
		}
		return nil
	},
}

// inspectReport — что модуль объявляет: ochan inspect (и --json).
type inspectReport struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Installed string `json:"-"`
	// InstalledPath — установленный модуль с тем же именем; Same — это
	// тот же файл (сумма совпадает).
	InstalledPath string           `json:"installed_path,omitempty"`
	Same          bool             `json:"installed_same,omitempty"`
	Commands      []inspectCommand `json:"commands"`
	Runners       []inspectRunner  `json:"runners"`
	Settings      []inspectSetting `json:"settings"`
}

type inspectCommand struct {
	Command string `json:"command"`
	Class   string `json:"class,omitempty"`
	Short   string `json:"short,omitempty"`
	Flags   string `json:"flags,omitempty"`
}

type inspectRunner struct {
	Kind         string   `json:"kind"`
	Executor     string   `json:"executor"`
	Services     []string `json:"services"`
	Ops          []string `json:"ops"`
	TokenCommand string   `json:"token_command"`
	TokenEnv     string   `json:"token_env"`
	Settings     string   `json:"settings"`
}

type inspectSetting struct {
	Key         string `json:"key"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	// Warning — предупреждение, которое ochan печатает, пока обязательная
	// настройка не задана.
	Warning string `json:"missing_warning,omitempty"`
}

// installedState — стоит ли модуль: та же сборка, другая или нет; путь
// установленного — в report.
func installedState(info pluginhost.ModuleInfo, sum []byte, report *inspectReport) string {
	fm, err := mustFileManager()
	if err != nil {
		return "неизвестно"
	}
	for _, loaded := range pluginhost.GetModuleManager().LoadedModules() {
		if loaded.Name != info.Name {
			continue
		}
		report.InstalledPath = loaded.Path
		installedSum, err := pluginhost.FileChecksum(loaded.Path)
		switch {
		case err != nil:
			return "да (" + loaded.Path + ")"
		case bytes.Equal(installedSum, sum):
			report.Same = true
			return "да, этот файл (" + loaded.Path + ")"
		}
		return fmt.Sprintf("да, другая сборка v%s (%s)", loaded.Version, loaded.Path)
	}
	return "нет (" + fm.ModulesDir() + ")"
}

// scenarioExecutorName — имя исполнителя по его виду, как у флагов status
// и ochan auth --executor.
func scenarioExecutorName(kind string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(kind)), " ", "-")
}

func flagNames(c *cobra.Command) string {
	var names []string
	c.Flags().VisitAll(func(f *pflag.Flag) {
		names = append(names, "--"+f.Name)
	})
	return strings.Join(names, " ")
}

// modulesJSON — ochan modules --json: команды модулей для программ, а не
// для человека.
var modulesJSON bool

func init() {
	inspectCmd.Flags().BoolVar(&inspectJSON, "json", false, "вывести JSON — для программ")
	listModulesCmd.Flags().BoolVar(&modulesJSON, "json", false,
		"команды модулей в JSON: путь, класс, описание и собственные флаги (для ochan-mcp и других программ)")
}

// moduleCommandInfo — команда модуля в ochan modules --json.
type moduleCommandInfo struct {
	// Command — путь команды через пробел: pgstat show.
	Command string `json:"command"`
	// Class — класс по аннотации class (свой или команды выше); пусто — нет.
	Class string `json:"class,omitempty"`
	Short string `json:"short,omitempty"`
	// Flags — собственные флаги команды: --имя, без глобальных ochan.
	Flags []string `json:"flags,omitempty"`
}

// printModuleCommandsJSON печатает выполнимые команды модулей. По этому
// списку ochan-mcp показывает модели read-команды модулей: они появляются,
// как только модуль загружен, без правки ядра и сервера.
func printModuleCommandsJSON(cmd *cobra.Command) error {
	commands := []moduleCommandInfo{}
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		path = append(append([]string(nil), path...), c.Name())
		if c.Runnable() {
			info := moduleCommandInfo{Command: strings.Join(path, " "), Class: rbac.ClassOf(strings.Join(path, ".")), Short: c.Short}
			c.NonInheritedFlags().VisitAll(func(f *pflag.Flag) {
				if f.Name != "help" {
					info.Flags = append(info.Flags, "--"+f.Name)
				}
			})
			commands = append(commands, info)
		}
		for _, sub := range c.Commands() {
			walk(sub, path)
		}
	}
	for _, c := range rootCmd.Commands() {
		if c.Annotations[moduleCommandAnnotation] == "module" {
			walk(c, nil)
		}
	}
	data, err := json.MarshalIndent(map[string]any{"commands": commands}, "", "  ")
	if err != nil {
		return err
	}
	cmd.Println(string(data))
	return nil
}
