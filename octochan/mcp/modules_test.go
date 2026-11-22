package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const fakeModules = `{"commands": [
  {"command": "pgraw", "short": "без класса"},
  {"command": "pgstat reset", "class": "apply", "short": "сбросить"},
  {"command": "pgstat show", "class": "read", "short": "показать", "flags": ["--db", "--verbose-stats"]}
]}`

// Read-команды модулей сервер узнаёт у ochan и показывает модели одним
// инструментом; исполнительные и без класса — нет. Флаги — только свои у
// команды, пути — только внутри корня.
func TestModuleCommands(t *testing.T) {
	e := newEnv(t, withFakeEnv("FAKE_MODULES="+fakeModules))

	responses := e.exchange(request(1, "tools/list", nil))
	var module map[string]any
	for _, tool := range decode(t, responses[0]).Result.Tools {
		if tool["name"] == moduleTool {
			module = tool
		}
	}
	if module == nil {
		t.Fatal("инструмента команд модулей нет в tools/list")
	}
	schema, _ := json.Marshal(module["inputSchema"])
	if !strings.Contains(string(schema), `"enum":["pgstat show"]`) {
		t.Fatalf("в списке не только read-команды: %s", schema)
	}

	text, isError := e.callTool(moduleTool, map[string]any{"command": "pgstat show",
		"args": []any{"--db=main", "sub", "a.conf"}})
	if isError || !strings.Contains(text, `["pgstat","show","--db=main","sub","a.conf"]`) {
		t.Fatalf("read-команда модуля: %v %s", isError, text)
	}

	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"command": "pgstat reset"}, "not a read-class module command"},
		{map[string]any{"command": "pgraw"}, "not a read-class module command"},
		{map[string]any{"command": "pgstat show", "args": []any{"--config=x.yaml"}}, "flag --config is not a flag"},
		{map[string]any{"command": "pgstat show", "args": []any{e.outside}}, "outside the allowed directories"},
		{map[string]any{"command": "pgstat show", "args": []any{"--db=../outside.conf"}}, "outside the allowed directories"},
		{map[string]any{"command": "pgstat show", "args": []any{"~/secret"}}, "relative to the allowed directory"},
		{map[string]any{"command": "pgstat show", "extra": "x"}, "unknown argument"},
	} {
		text, isError := e.callTool(moduleTool, c.args)
		if !isError || !strings.Contains(text, c.want) {
			t.Errorf("%v: ожидался отказ «%s», получено: %s", c.args, c.want, text)
		}
	}
}

// Без read-команд инструмента нет вовсе.
func TestNoModuleCommands(t *testing.T) {
	e := newEnv(t, withFakeEnv(`FAKE_MODULES={"commands": [{"command": "pgraw"}]}`))
	for _, tool := range decode(t, e.exchange(request(1, "tools/list", nil))[0]).Result.Tools {
		if tool["name"] == moduleTool {
			t.Fatal("инструмент команд модулей без read-команд")
		}
	}
}
