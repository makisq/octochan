package cmd

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"octochan/lang"
	"octochan/rbac"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Справочник команд и встроенных функций собирается из кода: он не может
// разойтись с тем, что ochan умеет на самом деле. После правки команды:
//
//	go test ./cmd -run TestReference -update
var updateReference = flag.Bool("update", false, "переписать docs/reference по коду")

const generatedNote = "<!-- Файл собран из кода: go test ./cmd -run TestReference -update. Руками не править. -->\n\n"

func TestReference(t *testing.T) {
	docs := map[string]string{
		filepath.Join("..", "docs", "reference", "commands.md"): commandReference(rootCmd),
		filepath.Join("..", "docs", "reference", "builtins.md"): builtinReference(),
	}
	for path, want := range docs {
		if *updateReference {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		have, err := os.ReadFile(path)
		if err != nil || string(have) != want {
			t.Errorf("%s устарел: go test ./cmd -run TestReference -update", path)
		}
	}
}

func commandReference(root *cobra.Command) string {
	var b strings.Builder
	b.WriteString(generatedNote)
	b.WriteString("# Справочник команд\n\n")
	b.WriteString("Все команды ochan с флагами и правом, которое их открывает (см. [роли](../guide/roles.md)).\n")
	b.WriteString("Справка по команде — `ochan help <команда>` или `ochan <команда> --help`; версия — `ochan --version`.\n\n")

	b.WriteString("## Общие флаги\n\n")
	flagTable(&b, root.PersistentFlags())

	var commands []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Name() == "completion" {
				continue
			}
			commands = append(commands, sub)
			walk(sub)
		}
	}
	walk(root)
	sort.SliceStable(commands, func(i, j int) bool { return commands[i].CommandPath() < commands[j].CommandPath() })

	b.WriteString("## Оглавление\n\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "- [`%s`](#%s) — %s\n", c.CommandPath(), anchor(c.CommandPath()), c.Short)
	}
	b.WriteString("\n")

	for _, c := range commands {
		fmt.Fprintf(&b, "## %s\n\n", c.CommandPath())
		fmt.Fprintf(&b, "%s.\n\n", strings.TrimSuffix(c.Short, "."))
		parent := strings.TrimSuffix(c.CommandPath(), " "+c.Name())
		fmt.Fprintf(&b, "```\n%s %s\n```\n\n", parent, c.Use)
		if c.Long != "" {
			b.WriteString(c.Long + "\n\n")
		}
		if key := commandKey(c); key != "" && c.Runnable() {
			fmt.Fprintf(&b, "Право: `%s`.\n\n", permissionFor(key))
		}
		if c.Example != "" {
			fmt.Fprintf(&b, "Примеры:\n\n```\n%s\n```\n\n", c.Example)
		}
		if hasOwnFlags(c.LocalNonPersistentFlags()) {
			flagTable(&b, c.LocalNonPersistentFlags())
		}
		var subs []string
		for _, sub := range c.Commands() {
			if !sub.Hidden {
				subs = append(subs, fmt.Sprintf("[`%s`](#%s)", sub.Name(), anchor(sub.CommandPath())))
			}
		}
		if len(subs) > 0 {
			fmt.Fprintf(&b, "Подкоманды: %s.\n\n", strings.Join(subs, ", "))
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// permissionFor — право, которое открывает команду: свободная или группа.
func permissionFor(key string) string {
	for _, free := range rbac.Free {
		if free == key {
			return "свободная — доступна любой роли"
		}
	}
	return requiredPerm(key)
}

func hasOwnFlags(flags *pflag.FlagSet) bool {
	own := false
	flags.VisitAll(func(f *pflag.Flag) {
		own = own || !f.Hidden && f.Name != "help" && f.Name != "version"
	})
	return own
}

func flagTable(b *strings.Builder, flags *pflag.FlagSet) {
	b.WriteString("| Флаг | Тип | По умолчанию | Описание |\n|---|---|---|---|\n")
	flags.VisitAll(func(f *pflag.Flag) {
		// --help и --version cobra добавляет сама, и только когда команда
		// выполнялась: в справочнике их нет, они есть у всех.
		if f.Hidden || f.Name == "help" || f.Name == "version" {
			return
		}
		name := "`--" + f.Name + "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		def := f.DefValue
		switch def {
		case "", "[]", "false":
			def = "—"
		default:
			def = "`" + def + "`"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", name, f.Value.Type(), def, strings.ReplaceAll(f.Usage, "|", "\\|"))
	})
	b.WriteString("\n")
}

// anchor повторяет то, как GitHub делает якорь из заголовка.
func anchor(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func builtinReference() string {
	var b strings.Builder
	b.WriteString(generatedNote)
	b.WriteString("# Встроенные функции языка алиасов\n\n")
	b.WriteString("Арность — сколько аргументов функция принимает; `…` — сколько угодно. Тип результата — тот,\n")
	b.WriteString("что видит проверка до запуска (`ochan check`); «—» — узнаётся при выполнении.\n")
	b.WriteString("Язык описан в [руководстве](../guide/language.md).\n\n")
	b.WriteString("| Функция | Аргументы | Результат | Что делает |\n|---|---|---|---|\n")

	builtins := lang.DefaultBuiltins()
	names := make([]string, 0, len(builtins))
	for name := range builtins {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := builtins[name]
		arity := fmt.Sprintf("%d", f.MinArgs)
		switch {
		case f.MaxArgs < 0:
			arity = fmt.Sprintf("%d…", f.MinArgs)
		case f.MaxArgs != f.MinArgs:
			arity = fmt.Sprintf("%d–%d", f.MinArgs, f.MaxArgs)
		}
		returns := "—"
		if f.Returns != "" {
			returns = "`" + string(f.Returns) + "`"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", name, arity, returns, strings.ReplaceAll(f.Doc, "|", "\\|"))
	}
	return b.String()
}
