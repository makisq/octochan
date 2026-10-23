package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"octochan/pluginhost"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
	})
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
	// Справка команд модулей — на текущем языке, как и своя.
	localizeCommands(rootCmd)
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

var installCmd = &cobra.Command{
	Use:   "!install <module_file>",
	Short: "Установить модуль команд",
	Args:  cobra.ExactArgs(1),
	Long: "Модуль копируется в ~/.octochan/modules и фиксируется его контрольная\n" +
		"сумма. При запуске загружаются только модули с совпадающей суммой.",
	RunE: func(cmd *cobra.Command, args []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}

		filePath := args[0]
		if filepath.Ext(filePath) != pluginhost.ModuleExtension {
			return fmt.Errorf("поддерживаются только %s модули", pluginhost.ModuleExtension)
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("не удалось прочитать модуль: %w", err)
		}

		// Переустановка поверх уже загруженного модуля: сначала гасим его
		// процесс, иначе Windows не даст перезаписать работающий файл.
		targetPath, err := fm.ModulePath(filepath.Base(filePath))
		if err != nil {
			return err
		}
		pluginhost.GetModuleManager().UnloadModule(targetPath)

		modulePath, err := fm.InstallHashicorpModule(filepath.Base(filePath), data)
		if err != nil {
			return err
		}

		// Сумма фиксируется до загрузки: LoadModule проверяет её и откажется
		// запускать файл, если содержимое изменится позже.
		if err := pluginhost.WriteChecksum(modulePath); err != nil {
			os.Remove(modulePath)
			return err
		}

		if err := pluginhost.GetModuleManager().LoadModule(modulePath); err != nil {
			os.Remove(modulePath)
			os.Remove(pluginhost.ChecksumPath(modulePath))
			return fmt.Errorf("модуль не загрузился и был удалён: %w", err)
		}

		registerModuleCommands()
		cmd.Printf("Модуль %s установлен и загружен\n", filepath.Base(modulePath))
		return nil
	},
}

var listModulesCmd = &cobra.Command{
	Use:   "modules",
	Short: "Показать установленные модули",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}

		entries, err := os.ReadDir(fm.ModulesDir())
		if err != nil {
			return fmt.Errorf("не удалось прочитать директорию модулей: %w", err)
		}

		loaded := make(map[string]pluginhost.ModuleInfo)
		for _, info := range pluginhost.GetModuleManager().LoadedModules() {
			loaded[filepath.Base(info.Path)] = info
		}

		found := 0
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != pluginhost.ModuleExtension {
				continue
			}
			found++

			if info, ok := loaded[entry.Name()]; ok {
				cmd.Printf("  %-32s %s v%s (команд: %d)\n",
					entry.Name(), info.Name, info.Version, info.Commands)
			} else {
				cmd.Printf("  %-32s не загружен\n", entry.Name())
			}
		}

		if found == 0 {
			cmd.Printf("Модулей нет. Директория: %s\n", fm.ModulesDir())
		}
		return nil
	},
}

var inspectCmd = &cobra.Command{
	Use:   "inspect <module_path>",
	Short: "Показать информацию о модуле",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := args[0]
		if filepath.Ext(path) != pluginhost.ModuleExtension {
			return fmt.Errorf("поддерживаются только %s модули", pluginhost.ModuleExtension)
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("модуль недоступен: %w", err)
		}

		// Модуль поднимается во временном менеджере и гасится сразу после
		// осмотра, чтобы не влиять на набор команд текущей сессии.
		inspector := pluginhost.NewModuleManager()
		defer inspector.Cleanup()

		if err := inspector.LoadModule(path); err != nil {
			return err
		}

		infos := inspector.LoadedModules()
		if len(infos) == 0 {
			return fmt.Errorf("модуль не сообщил о себе ничего")
		}
		info := infos[0]

		cmd.Println("\n=== Информация о модуле ===")
		cmd.Printf("Файл:   %s\n", path)
		cmd.Printf("Имя:    %s\n", info.Name)
		cmd.Printf("Версия: %s\n", info.Version)

		commands := inspector.GetCommands()
		if len(commands) == 0 {
			cmd.Println("Модуль не предоставляет команд")
			return nil
		}

		cmd.Println("Команды:")
		printModuleCommands(cmd, commands, "  ")
		return nil
	},
}

func printModuleCommands(cmd *cobra.Command, commands []*cobra.Command, indent string) {
	for _, c := range commands {
		short := c.Short
		if short == "" {
			short = "(нет описания)"
		}
		cmd.Printf("%s%-24s %s\n", indent, c.Name(), short)

		if flags := flagNames(c); flags != "" {
			cmd.Printf("%s%-24s флаги: %s\n", indent, "", flags)
		}
		if sub := c.Commands(); len(sub) > 0 {
			printModuleCommands(cmd, sub, indent+"  ")
		}
	}
}

func flagNames(c *cobra.Command) string {
	var names []string
	c.Flags().VisitAll(func(f *pflag.Flag) {
		names = append(names, "--"+f.Name)
	})
	return strings.Join(names, " ")
}
