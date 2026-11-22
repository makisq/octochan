package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// moduleTool — инструмент для команд модулей класса read.
//
// Таблица инструментов закрыта (checkTable), а модули ставятся на ходу
// (!!install). Поэтому команды модулей сервер узнаёт у самого ochan — ochan
// modules --json — при каждом tools/list и каждом вызове: модуль, который
// объявил команду классом read (аннотация class), модель видит сразу, без
// правки ядра и сервера. Исполнительную команду и команду без класса сервер
// не покажет и не запустит; ochan в роли llm проверит права ещё раз.
const moduleTool = "module_command"

// moduleClass — единственный класс команд модулей, доступный по MCP.
// Потолок llm шире (plan, request, net), но инструменты сервера — чтение и
// заявки, и команда модуля, которая что-то оформляет, сюда не попадает.
const moduleClass = "read"

// maxModuleArgs — предел аргументов команды модуля.
const maxModuleArgs = 32

type moduleCommand struct {
	Command string   `json:"command"`
	Class   string   `json:"class"`
	Short   string   `json:"short"`
	Flags   []string `json:"flags"`
}

// moduleCommands — read-команды модулей, которые сейчас загружены у ochan.
func (s *server) moduleCommands(ctx context.Context) ([]moduleCommand, error) {
	out := s.runner.run(ctx, []string{"modules", "--json"}, "")
	if out.code != 0 {
		return nil, fmt.Errorf("ochan modules --json: код %d: %s", out.code, strings.TrimSpace(out.stderr))
	}
	var listing struct {
		Commands []moduleCommand `json:"commands"`
	}
	if err := json.Unmarshal([]byte(out.stdout), &listing); err != nil {
		return nil, fmt.Errorf("ochan modules --json: %w", err)
	}
	var read []moduleCommand
	for _, c := range listing.Commands {
		if c.Class == moduleClass {
			read = append(read, c)
		}
	}
	sort.Slice(read, func(i, j int) bool { return read[i].Command < read[j].Command })
	return read, nil
}

// moduleDescriptor — описание инструмента со списком команд; nil — команд нет.
func moduleDescriptor(commands []moduleCommand) map[string]any {
	if len(commands) == 0 {
		return nil
	}
	names := make([]any, len(commands))
	var list strings.Builder
	for i, c := range commands {
		names[i] = c.Command
		fmt.Fprintf(&list, "\n- %s", c.Command)
		if c.Short != "" {
			fmt.Fprintf(&list, " — %s", c.Short)
		}
		if len(c.Flags) > 0 {
			fmt.Fprintf(&list, " (flags: %s)", strings.Join(c.Flags, " "))
		}
	}
	return map[string]any{
		"name":  moduleTool,
		"title": "Module command (read)",
		"description": "Runs a read-only command of an installed ochan module. Modules declare these " +
			"commands as read class themselves; the list changes as modules are installed. " +
			"File paths in arguments must be inside the allowed directories. Available:" + list.String(),
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "enum": names,
					"description": "The module command, words separated by spaces"},
				"args": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"maxItems":    maxModuleArgs,
					"description": "Arguments and the command's own flags (--name or --name=value)"},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
		"annotations": map[string]any{"title": "Module command (read)", "readOnlyHint": true, "openWorldHint": true},
	}
}

// moduleArgv проверяет вызов и собирает аргументы ochan.
func (s *server) moduleArgv(ctx context.Context, arguments map[string]any) ([]string, error) {
	name, _ := arguments["command"].(string)
	for key := range arguments {
		if key != "command" && key != "args" {
			return nil, slotError(key, "unknown argument")
		}
	}
	commands, err := s.moduleCommands(ctx)
	if err != nil {
		return nil, err
	}
	var found *moduleCommand
	for i := range commands {
		if commands[i].Command == name {
			found = &commands[i]
		}
	}
	if found == nil {
		names := make([]string, len(commands))
		for i, c := range commands {
			names[i] = c.Command
		}
		return nil, slotError("command", "%q is not a read-class module command; available: %s",
			name, strings.Join(names, ", "))
	}

	var raw []any
	if v, ok := arguments["args"]; ok && v != nil {
		if raw, ok = v.([]any); !ok {
			return nil, slotError("args", "must be an array of strings")
		}
	}
	if len(raw) > maxModuleArgs {
		return nil, slotError("args", "at most %d arguments", maxModuleArgs)
	}
	argv := strings.Fields(found.Command)
	for i, item := range raw {
		arg, ok := item.(string)
		if !ok {
			return nil, slotError("args", "argument %d is not a string", i)
		}
		if err := s.moduleArg(*found, arg); err != nil {
			return nil, err
		}
		argv = append(argv, arg)
	}
	return argv, nil
}

// moduleArg — флаг только свой у команды (глобальные флаги ochan вроде
// --config не проходят), а путь — только внутри разрешённых каталогов.
func (s *server) moduleArg(c moduleCommand, arg string) error {
	if strings.ContainsRune(arg, 0) {
		return slotError("args", "NUL character")
	}
	value := arg
	if strings.HasPrefix(arg, "-") {
		flag, v, hasValue := strings.Cut(arg, "=")
		known := false
		for _, f := range c.Flags {
			known = known || f == flag
		}
		if !known {
			return slotError("args", "flag %s is not a flag of %s (its flags: %s)", flag, c.Command, strings.Join(c.Flags, " "))
		}
		if !hasValue {
			return nil
		}
		value = v
	}
	if strings.HasPrefix(value, "~") || (len(value) >= 2 && value[1] == ':' && !filepath.IsAbs(value)) {
		// ~ раскроет модуль, C:файл — от текущего каталога диска: куда это
		// ведёт, серверу не проверить.
		return slotError("args", "%s: use a path relative to the allowed directory", value)
	}
	if !pathLike(value, s.box.roots[0]) {
		return nil
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.box.roots[0], path)
	}
	path = filepath.Clean(path)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		dir, derr := filepath.EvalSymlinks(filepath.Dir(path))
		if derr != nil {
			return slotError("args", "the directory for %s does not exist", value)
		}
		real = filepath.Join(dir, filepath.Base(path))
	}
	if !s.box.inside(real) {
		return slotError("args", "%s is outside the allowed directories (%s)", value, strings.Join(s.box.roots, ", "))
	}
	return nil
}

// pathLike — значение похоже на путь: с разделителем, с «..», начинается с
// «~» или есть такой файл в корне.
func pathLike(value, root string) bool {
	if value == "" {
		return false
	}
	if strings.ContainsAny(value, `/\`) || strings.Contains(value, "..") || strings.HasPrefix(value, "~") || filepath.IsAbs(value) {
		return true
	}
	_, err := os.Lstat(filepath.Join(root, value))
	return err == nil
}
