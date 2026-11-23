package cmd

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"ochan/lang"
	"ochan/rbac"
	"ochan/scenario"

	"github.com/spf13/cobra"
)

// Команды ochan для сценария — ochan() языка алиасов (lang/command.go).
// Сценарий выполняет их тем же путём, что командная строка: права роли и
// журнал аудита — authorize, флаги — по умолчанию, вывод — значение
// ochan().

// scriptForbidden — команды, которых у сценария нет: те, что сами
// запускают сценарии (иначе сценарий шёл бы по кругу, а флаги внешней
// команды сбрасывались бы под ней), живут долго или меняют роль процесса
// посреди сценария.
var scriptForbidden = []string{"agent", "apply", "back", "completion", "connect", "eval", "help", "plan", "remote",
	"run", "serve", "set", "shell", "watch"}

// scriptCommands — команды ochan, которые видит сценарий. shell — строка
// оболочки или ochan eval: внешней команды, которая выполняет сценарий, нет,
// и apply, plan и run ей доступны (apply @ ("a.yaml" | "b.yaml")). Внутри
// сценария, который выполняет run или apply, они по-прежнему запрещены:
// согласованный сценарий не запустит несогласованный.
type scriptCommands struct{ shell bool }

// shellAllowed — команды, которые строка оболочки может выполнить, хотя
// сценарию они запрещены.
var shellAllowed = []string{"apply", "plan", "run"}

// scriptShared и scriptResponses — общие строки и ответы системы на apply,
// выполненный из языка (f @ (…)): их забирает TakeShared. В вывод они не
// попадают.
var (
	scriptShared    map[string]string
	scriptResponses []string
)

// keepShared запоминает общие строки и ответы исполнителя, если apply
// выполняется из языка.
func keepShared(result *scenario.Result) {
	if result == nil || scriptDepth == 0 {
		return
	}
	if len(result.Shared) > 0 && scriptShared == nil {
		scriptShared = map[string]string{}
	}
	for k, v := range result.Shared {
		scriptShared[k] = v
	}
	scriptResponses = append(scriptResponses, result.Responses...)
}

// TakeShared отдаёт языку общие строки и ответы системы последней команды
// и забывает их.
func (scriptCommands) TakeShared() (map[string]string, []string) {
	shared, responses := scriptShared, scriptResponses
	scriptShared, scriptResponses = nil, nil
	return shared, responses
}

// scriptDepth — сколько команд сейчас выполняется из сценария: команда,
// которая ждала бы вечно (text tail -f) или stdin, по нему отказывает.
var scriptDepth int

// queryGroups — права читающих команд: их ochan() выполняет и в плане.
var queryGroups = []string{"read", "net"}

func (c scriptCommands) Resolve(args []string) ([]string, string, bool, error) {
	argv := args
	if len(args) == 1 {
		split, err := splitArgs(args[0])
		if err != nil {
			return nil, "", false, fmt.Errorf("ochan %s: %w", args[0], err)
		}
		argv = split
	}
	if len(argv) == 0 {
		return nil, "", false, fmt.Errorf("ochan(): пустая команда")
	}
	target, _, err := rootCmd.Find(argv)
	if err != nil || target == rootCmd {
		return nil, "", false, fmt.Errorf("ochan %s: такой команды нет", strings.Join(argv, " "))
	}
	key := commandKey(target)
	top, _, _ := strings.Cut(key, ".")
	allowed := c.shell && scriptDepth == 0 && slices.Contains(shellAllowed, top)
	if !allowed && (slices.Contains(scriptForbidden, top) || slices.Contains(scriptForbidden, key)) {
		return nil, "", false, fmt.Errorf("ochan %s: из сценария не выполняется — команда сама запускает сценарии, "+
			"живёт долго или меняет роль процесса", key)
	}
	// Ввод сценария — не терминал: команда, которая ждёт stdin, ждала бы
	// вечно.
	if slices.Contains(argv, "-") {
		return nil, "", false, fmt.Errorf("ochan %s: stdin сценарию недоступен — передайте файл", strings.Join(argv, " "))
	}
	query := slices.Contains(rbac.Free, key)
	for _, group := range queryGroups {
		query = query || rbac.Covers(group, key)
	}
	return argv, key, query, nil
}

func (scriptCommands) Run(ctx context.Context, argv []string) (string, error) {
	target, _, err := rootCmd.Find(argv)
	if err != nil {
		return "", err
	}
	// Флаги и контекст — только у этой команды: внешняя, которая выполняет
	// сценарий, свои уже прочитала, и сбрасывать их под ней нельзя. Контекст
	// сценария после команды кончится — её прежний контекст возвращается.
	resetFlags(target)
	restore := saveContexts(target)
	setContext(target, ctx)
	var out bytes.Buffer
	prev := rootCmd.OutOrStdout()
	rootCmd.SetOut(&out)
	rootCmd.SetArgs(argv)
	scriptDepth++
	defer func() {
		scriptDepth--
		rootCmd.SetOut(prev)
		resetFlags(target)
		restore()
	}()
	err = rootCmd.ExecuteContext(ctx)
	return strings.TrimRight(out.String(), "\n"), err
}

// Shared — общие строки команды модуля для f @ (…): аннотация shared у
// неё или у команды над ней (lang.SharedAnnotation).
func (scriptCommands) Shared(argv []string) []lang.SharedKey {
	target, _, err := rootCmd.Find(argv)
	if err != nil {
		return nil
	}
	for c := target; c != nil && c != rootCmd; c = c.Parent() {
		if text := c.Annotations[lang.SharedAnnotation]; text != "" {
			return lang.ParseShared(text)
		}
	}
	return nil
}

// saveContexts запоминает контексты команд дерева и возвращает, чем их
// вернуть.
func saveContexts(cmd *cobra.Command) func() {
	saved := map[*cobra.Command]context.Context{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		saved[c] = c.Context()
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd)
	return func() {
		for c, ctx := range saved {
			c.SetContext(ctx)
		}
	}
}
