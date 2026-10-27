package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"octochan/lang"
	"octochan/pluginhost"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var installRemove bool

var installCmd = &cobra.Command{
	Use:   "!install <модуль.hcplugin | пакет | файл.ochan>",
	Short: "Установить модуль или пакет алиасов",
	Long: "Одна команда ставит всё, чем расширяется ochan, в ~/.octochan/modules:\n\n" +
		"  модуль.hcplugin  модуль gRPC: команды, языки и исполнители сценариев —\n" +
		"                   что он объявит. Фиксируется его контрольная сумма, и\n" +
		"                   подменённый после установки файл не загрузится\n" +
		"  пакет            каталог с алиасами (*.ochan) и package.yaml — скилл для\n" +
		"                   сценариев и формального промпта. Модули .hcplugin из\n" +
		"                   каталога ставятся вместе с ним: скилл и его исполнитель\n" +
		"  файл.ochan       однофайловый пакет алиасов\n\n" +
		"Пакет проверяется до установки: с ошибкой он не ставится и библиотеку не\n" +
		"ломает. Исполнитель модуля выбирается по полю service сценария, как\n" +
		"встроенный; его токен — ochan auth --executor <имя>.\n\n" +
		"С --remove удаляет модуль (имя файла) или пакет (имя пакета).",
	Example: "ochan '!install' ssh-runner.hcplugin\n" +
		"ochan '!install' ./pgconf\n" +
		"ochan '!install' --remove pgconf",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if installRemove {
			return removeInstalled(cmd, args[0])
		}
		info, err := os.Stat(args[0])
		if err != nil {
			return fmt.Errorf("нечего ставить: %w", err)
		}
		switch {
		case info.IsDir():
			return installDir(cmd, args[0])
		case filepath.Ext(args[0]) == pluginhost.ModuleExtension:
			return installPlugin(cmd, args[0])
		case filepath.Ext(args[0]) == lang.AliasExtension:
			return installPackage(cmd, args[0], nil)
		}
		return fmt.Errorf("ставятся модули %s, пакеты алиасов (каталог) и файлы %s",
			pluginhost.ModuleExtension, lang.AliasExtension)
	},
}

// installPlugin ставит модуль gRPC и называет, что он принёс.
func installPlugin(cmd *cobra.Command, filePath string) error {
	fm, err := mustFileManager()
	if err != nil {
		return err
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
	mm := pluginhost.GetModuleManager()
	mm.UnloadModule(targetPath)

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
	if err := mm.LoadModule(modulePath); err != nil {
		os.Remove(modulePath)
		os.Remove(pluginhost.ChecksumPath(modulePath))
		return fmt.Errorf("модуль не загрузился и был удалён: %w", err)
	}

	registerModuleCommands()
	for _, warning := range syncRunners() {
		cmd.PrintErrf("внимание: %s\n", warning)
	}

	cmd.Printf("Модуль %s установлен и загружен\n", filepath.Base(modulePath))
	for _, info := range mm.LoadedModules() {
		if filepath.Base(info.Path) != filepath.Base(modulePath) {
			continue
		}
		if info.Commands > 0 {
			cmd.Printf("  команд: %d — ochan inspect %s\n", info.Commands, filepath.Base(modulePath))
		}
		if len(info.Languages) > 0 {
			cmd.Printf("  языки: %s — lang в config.yaml, со следующего запуска\n", strings.Join(info.Languages, ", "))
		}
		for _, r := range mm.Runners() {
			if r.Module == info.Name {
				cmd.Printf("  исполнитель %s: сервисы %s; токен — ochan auth --executor %s\n",
					r.Kind, strings.Join(r.Services, ", "), strings.ToLower(r.Kind))
			}
		}
	}
	return nil
}

// installDir ставит каталог: пакет алиасов, если в нём есть *.ochan, и
// модули .hcplugin, что лежат рядом.
func installDir(cmd *cobra.Command, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var aliases, plugins []string
	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case lang.AliasExtension:
			if !e.IsDir() {
				aliases = append(aliases, e.Name())
			}
		case pluginhost.ModuleExtension:
			if !e.IsDir() {
				plugins = append(plugins, filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(aliases) == 0 && len(plugins) == 0 {
		return fmt.Errorf("в %s нет ни алиасов (*%s), ни модулей (*%s)", dir, lang.AliasExtension, pluginhost.ModuleExtension)
	}
	if len(aliases) > 0 {
		if err := installPackage(cmd, dir, aliases); err != nil {
			return err
		}
	}
	for _, plugin := range plugins {
		if err := installPlugin(cmd, plugin); err != nil {
			return err
		}
	}
	return nil
}

// installPackage ставит пакет алиасов: каталог (files — его *.ochan) или
// один файл .ochan (files пусто).
//
// Пакет проверяется в отдельном реестре до того, как что-то скопировано:
// сломанный пакет не должен испортить библиотеку, которой пользуются
// сценарии и заявки.
func installPackage(cmd *cobra.Command, src string, files []string) error {
	fm, err := mustFileManager()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(filepath.Base(abs), lang.AliasExtension)
	if !packageName(name) {
		return fmt.Errorf("пакет %q: имя пакета — его префикс в сценариях (%s.алиас), "+
			"допустимы буквы, цифры и «_», первой — буква", name, name)
	}

	check := lang.NewRegistry()
	if files == nil {
		_, err = check.LoadFile(name, abs)
	} else {
		err = check.LoadDir(name, abs)
	}
	if err != nil {
		return fmt.Errorf("пакет %s не установлен: %w", name, err)
	}

	target := filepath.Join(fm.ModulesDir(), name)
	single := target + lang.AliasExtension
	if abs == target || abs == single {
		return fmt.Errorf("пакет %s уже установлен: это его каталог в %s", name, fm.ModulesDir())
	}
	// Пакет одного имени — один: каталог заменяет однофайловый и наоборот.
	for _, old := range []string{target, single} {
		if err := os.RemoveAll(old); err != nil {
			return fmt.Errorf("не удалось убрать прежний пакет %s: %w", name, err)
		}
	}
	if files == nil {
		if err := copyFile(abs, single); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(target, 0o700); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(abs, lang.ManifestName)); err == nil {
			files = append(files, lang.ManifestName)
		}
		for _, f := range files {
			if err := copyFile(filepath.Join(abs, f), filepath.Join(target, f)); err != nil {
				return err
			}
		}
	}
	resetAliasCache()

	version := "без версии"
	if pkg, ok := check.Package(name); ok && pkg.Version != "" {
		version = "v" + pkg.Version
	}
	count := 0
	for _, info := range check.List() {
		if info.Module == name {
			count++
		}
	}
	cmd.Printf("Пакет алиасов %s %s установлен: алиасов %d — ochan aliases %s\n", name, version, count, name)
	return nil
}

// packageName — годится ли имя пакета префиксом в сценарии.
func packageName(name string) bool {
	for i, r := range name {
		if !(unicode.IsLetter(r) || r == '_' || (i > 0 && unicode.IsDigit(r))) {
			return false
		}
	}
	return name != ""
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o600)
}

// removeInstalled удаляет модуль или пакет алиасов.
func removeInstalled(cmd *cobra.Command, name string) error {
	fm, err := mustFileManager()
	if err != nil {
		return err
	}
	if filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("%q: укажите имя модуля или пакета, а не путь", name)
	}
	dir := fm.ModulesDir()

	if filepath.Ext(name) == pluginhost.ModuleExtension {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("модуль %s не установлен", name)
		}
		pluginhost.GetModuleManager().UnloadModule(path)
		if err := errors.Join(os.Remove(path), ignoreMissing(os.Remove(pluginhost.ChecksumPath(path)))); err != nil {
			return fmt.Errorf("модуль %s не удалён: %w", name, err)
		}
		refreshModuleCommands()
		for _, warning := range syncRunners() {
			cmd.PrintErrf("внимание: %s\n", warning)
		}
		cmd.Printf("Модуль %s удалён\n", name)
		return nil
	}

	removed := false
	for _, path := range []string{filepath.Join(dir, name), filepath.Join(dir, name+lang.AliasExtension)} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("пакет %s не удалён: %w", name, err)
		}
		removed = true
	}
	if !removed {
		var installed []string
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if e.IsDir() || slices.Contains([]string{pluginhost.ModuleExtension, lang.AliasExtension}, filepath.Ext(e.Name())) {
					installed = append(installed, e.Name())
				}
			}
		}
		return fmt.Errorf("%s не установлен; установлены: %s", name, orNone(installed))
	}
	resetAliasCache()
	cmd.Printf("Пакет алиасов %s удалён\n", name)
	return nil
}

func ignoreMissing(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// executorSupply — что ядро выдаёт исполнителю модуля в каждом вызове:
// токен (ochan auth --executor или OCTOCHAN_TOKEN_<ИМЯ>) и настройки.
func executorSupply(executor string) pluginhost.Supply {
	supply := pluginhost.Supply{Settings: executorSettings(executor)}
	if fm, err := mustFileManager(); err == nil {
		if token := fm.ExecutorToken(executor); token != "" {
			supply.Credentials = map[string]string{"token": token}
		}
	}
	return supply
}

// executorSettings — настройки исполнителя: executors.<имя> в config.yaml
// поверх общих defaults.api_url, defaults.http_timeout и
// defaults.max_parallel_tasks. Своего конфига у процесса модуля нет.
func executorSettings(executor string) map[string]string {
	settings := map[string]string{
		"http_timeout":       viper.GetString("defaults.http_timeout"),
		"max_parallel_tasks": viper.GetString("defaults.max_parallel_tasks"),
	}
	if url := viper.GetString("defaults.api_url"); url != "" {
		settings["api_url"] = url
	}
	for key, value := range viper.GetStringMapString("executors." + executor) {
		settings[key] = value
	}
	return settings
}

func init() {
	installCmd.Flags().BoolVar(&installRemove, "remove", false, "удалить модуль или пакет алиасов")
}
