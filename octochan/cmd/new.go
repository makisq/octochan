package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"ochan/core/i18n"

	"github.com/spf13/cobra"
)

// ochan new module — заготовка модуля команд: main.go со скелетом
// (команда с классом read, настройка, токен), go.mod и Makefile. Раньше
// модуль начинали, копируя чужой.

var (
	newModuleDir string
	newModuleSrc string
)

var newCmd = &cobra.Command{
	Use:   "new",
	Short: "Заготовки: модуль команд",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var newModuleCmd = &cobra.Command{
	Use:   "module <имя>",
	Short: "Создать заготовку модуля команд: main.go, go.mod, Makefile",
	Long: "Создаёт каталог <имя> с модулем, который сразу собирается и ставится:\n" +
		"main.go (команда <имя> hello с классом read, настройка greeting в\n" +
		"modules.<имя>, токен модуля), go.mod и Makefile (make — собрать,\n" +
		"make inspect — проверить, make install — поставить). Модуль импортирует\n" +
		"ochan/modules/pluginhost, поэтому go.mod ссылается на исходники ochan:\n" +
		"--src, иначе — каталог исходников, в котором вы сейчас, иначе ../octochan.\n" +
		"Каталог, который уже есть и не пуст, не трогается.",
	Example: "ochan new module mymod\nochan new module mymod --src ~/src/octochan/octochan",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if !moduleNamePattern.MatchString(name) {
			return i18n.Errorf("имя модуля %q: буквы, цифры, «-» и «_», первой — буква", name)
		}
		dir := newModuleDir
		if dir == "" {
			dir = name
		}
		dir = expandHome(dir)
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return i18n.Errorf("каталог %s уже есть и не пуст — заготовка его не трогает", dir)
		}
		src, guessed := newModuleSource(dir)
		files := map[string]string{
			"main.go":  moduleMainTemplate(name),
			"go.mod":   moduleGoMod(name, src),
			"Makefile": moduleMakefile(name),
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, file := range []string{"main.go", "go.mod", "Makefile"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(files[file]), 0o644); err != nil {
				return err
			}
		}
		cmd.Println(i18n.Sprintf("Заготовка модуля %s: %s (main.go, go.mod, Makefile)", name, dir))
		cmd.Println(i18n.Sprintf("  исходники ochan: %s", src))
		if guessed {
			cmd.Println(i18n.T("  внимание: путь угадан — поправьте replace в go.mod или задайте --src"))
		}
		cmd.Println(i18n.Sprintf("Дальше: cd %s && make inspect && make install, затем ochan %s hello", dir, name))
		return nil
	},
}

// moduleNamePattern — имя модуля: оно же каталог, имя команды и раздел
// modules.<имя> в config.yaml.
var moduleNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// newModuleSource — путь к исходникам ochan для replace в go.mod, от
// каталога модуля: --src, иначе исходники вокруг текущего каталога, иначе
// ../octochan (guessed — угадан).
func newModuleSource(dir string) (string, bool) {
	from := func(path string) string {
		absDir, err1 := filepath.Abs(dir)
		absSrc, err2 := filepath.Abs(path)
		if err1 != nil || err2 != nil {
			return path
		}
		// Рядом — относительный путь (переносится вместе с деревом), далеко
		// — абсолютный: «../../../../..» не прочитать.
		if rel, err := filepath.Rel(absDir, absSrc); err == nil && strings.Count(rel, "..") <= 2 {
			return filepath.ToSlash(rel)
		}
		return filepath.ToSlash(absSrc)
	}
	if newModuleSrc != "" {
		return from(expandHome(newModuleSrc)), false
	}
	if cwd, err := os.Getwd(); err == nil {
		for d := cwd; ; d = filepath.Dir(d) {
			if data, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil &&
				strings.HasPrefix(strings.TrimSpace(string(data)), "module ochan\n") {
				return from(d), false
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "../octochan", true
}

// moduleGoMod — go.mod модуля: ochan через replace на исходники и cobra
// той версии, с которой собран этот ochan; остальное доберёт go mod tidy.
func moduleGoMod(name, src string) string {
	cobraVersion := "v1.10.1"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/spf13/cobra" {
				cobraVersion = dep.Version
			}
		}
	}
	return fmt.Sprintf("module %s\n\ngo 1.26.0\n\nrequire (\n\tgithub.com/spf13/cobra %s\n\tochan v0.0.0\n)\n\n"+
		"// Исходники ochan: модуль импортирует ochan/modules/pluginhost.\nreplace ochan => %s\n", name, cobraVersion, src)
}

func moduleMakefile(name string) string {
	return "# Модуль ochan " + name + ": make — собрать, make inspect — проверить,\n" +
		"# make install — поставить (!!install), make clean — убрать сборку.\n" +
		"NAME := " + name + "\nPLUGIN := $(NAME).hcplugin\n\n" +
		".PHONY: build inspect install clean\n\n" +
		"build:\n\tgo mod tidy\n\tgo build -o $(PLUGIN) .\n\n" +
		"inspect: build\n\tochan inspect $(PLUGIN)\n\n" +
		"install: build\n\tochan '!!install' $(PLUGIN)\n\n" +
		"clean:\n\tgo clean\n\t$(RM) $(PLUGIN)\n"
}

func moduleMainTemplate(name string) string {
	return strings.NewReplacer("{{name}}", name).Replace(`// Модуль ochan {{name}}: команды, которые ochan подключает после !!install.
// Собрать — make, проверить — make inspect, поставить — make install.
// Как устроены модули — docs/guide/modules.md в исходниках ochan.
package main

import (
	"fmt"

	"ochan/modules/pluginhost"

	"github.com/spf13/cobra"
)

type module struct{}

// initSettings — настройки, которые ochan передал при загрузке (Init).
var initSettings = map[string]string{}

func (module) Name() string    { return "{{name}}" }
func (module) Version() string { return "0.1.0" }

// Init — значения настроек при загрузке модуля. Ошибка — отказ загрузиться.
func (module) Init(values map[string]any) error {
	for k, v := range values {
		initSettings[k] = fmt.Sprint(v)
	}
	return nil
}

// Settings — что настраивается: !!install пишет это в config.yaml, раздел
// modules.{{name}}. Секреты сюда не кладут — токен задаёт
// ochan auth --executor {{name}}.
func (module) Settings() []pluginhost.Setting {
	return []pluginhost.Setting{
		{Key: "greeting", Default: "привет", Description: "чем {{name}} hello здоровается"},
	}
}

// setting — значение настройки в команде: как сейчас в config.yaml, а с
// ochan старше 0.3.1 — то, что пришло в Init.
func setting(cmd *cobra.Command, key string) string {
	if values := pluginhost.Settings(cmd.Context()); values != nil {
		return values[key]
	}
	return initSettings[key]
}

func (module) GetCommands() []*cobra.Command {
	root := &cobra.Command{
		Use:   "{{name}}",
		Short: "Команды модуля {{name}}",
		// Класс прав: read — открывается правом read; execution — правом apply.
		Annotations: map[string]string{"class": "read"},
	}
	root.AddCommand(&cobra.Command{
		Use:   "hello [имя]",
		Short: "Поздороваться — пример команды",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			who := "мир"
			if len(args) == 1 {
				who = args[0]
			}
			token := "нет"
			if pluginhost.Token(cmd.Context()) != "" {
				token = "есть"
			}
			// Печатайте через cmd: вывод уходит туда же, куда вывод команд
			// ochan, — в файл по >|, в конвейер, в сценарий.
			cmd.Printf("%s, %s! (токен: %s)\n", setting(cmd, "greeting"), who, token)
			return nil
		},
	})
	return []*cobra.Command{root}
}

func main() { pluginhost.ServeCommandModule(module{}) }
`)
}

func init() {
	newModuleCmd.Flags().StringVar(&newModuleDir, "dir", "", "куда положить модуль (по умолчанию ./<имя>)")
	newModuleCmd.Flags().StringVar(&newModuleSrc, "src", "", "каталог исходников ochan (где go.mod с module ochan)")
	newCmd.AddCommand(newModuleCmd)
	rootCmd.AddCommand(newCmd)
}
