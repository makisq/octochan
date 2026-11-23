package lang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// a @ (a @ (b | c) | d) — это ab, ac, ab, ac, ad: глубже — раньше, вызовов
// столько, сколько написано.
func TestAtExpansion(t *testing.T) {
	cases := []struct{ src, want string }{
		{`a @ ("b" | "c")`, "ab ac"},
		{`a @ (a @ ("b" | "c") | "d")`, "ab ac ab ac ad"},
		{`a @ (x @ ("b" | "c") | "d")`, "xb xc ab ac ad"},
		{`a @ (("b" | "c") | "d")`, "ab ac ad"},
		{`a @ (a @ (a @ ("b") | "c"))`, "ab ab ac ab ac"},
		{`a @ ("b"
            | "c")`, "ab ac"},
	}
	for _, c := range cases {
		src := `
var log = []
alias a(s) { return "a" + s }
alias x(s) { return "x" + s }
print(join(` + c.src + `, " "))
`
		out, err := run(t, src)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got := strings.TrimSpace(out); got != c.want {
			t.Errorf("%s: получено %q, ожидалось %q", c.src, got, c.want)
		}
	}
}

// Общая строка: c ждёт string_i (?), его ответ даёт b. Вызов c ждёт, пока
// строка найдётся, — даже если написан раньше b, и даже если между ними
// другие вызовы.
func TestAtSharedStrings(t *testing.T) {
	src := `
alias node(p) {
    if p["name"] == "b" { return {"status": 200, "body": "{\"result\": {\"string_i\": \"s3cr3t\"}}"} }
    if p["name"] == "c" {
        if p["parameters"]["string_i"] != "s3cr3t" { return {"status": 500, "body": "нет string_i"} }
        return {"status": 200, "body": "ok c"}
    }
    return {"status": 200, "body": "ok " + p["name"]}
}
b = {"name": "b", "parameters": {}}
c = {"name": "c", "parameters": {"string_i": "?"}}
e = {"name": "e", "parameters": {}}
for r in node @ (c | e | b) { print(r["status"], r["body"]) }
`
	out, err := run(t, src)
	if err != nil {
		t.Fatal(err)
	}
	want := "200 ok e\n200 {\"result\": {\"string_i\": \"s3cr3t\"}}\n200 ok c"
	if got := strings.TrimSpace(out); got != want {
		t.Fatalf("получено:\n%s\nожидалось:\n%s", got, want)
	}

	// Строки нет ни в одном ответе — ошибка с её именем и вызовом.
	_, err = run(t, `
alias node(p) { return "ok" }
node @ ({"name": "c", "token": "?"} | "x")
`)
	if err == nil || !strings.Contains(err.Error(), "общая строка не найдена") || !strings.Contains(err.Error(), "token") {
		t.Fatalf("ошибка: %v", err)
	}
}

// Ответ-текст: строка «ключ: значение» или JSON; слово целиком.
func TestSharedInText(t *testing.T) {
	cases := []struct{ text, want string }{
		{"string_i: abc\n", "abc"},
		{`{"data": {"string_i": "q-1"}}`, "q-1"},
		{"my_string_i: no\nstring_i = yes", "yes"},
		{`"string_i": "with space"`, "with space"},
		{"string_i: ?\nstring_i: real", "real"},
	}
	for _, c := range []struct{ text, want string }{
		{`{"session": {"token": "t-1"}, "token": "other"}`, "t-1"},
		{"session.token: lit", "lit"},
	} {
		if got, ok := sharedIn(StringValue(c.text), "session.token"); !ok || got != c.want {
			t.Errorf("путь %q: %q %v, ожидалось %q", c.text, got, ok, c.want)
		}
	}
	if got, ok := sharedIn(MapValue{"session": MapValue{"token": StringValue("m-1")}}, "session.token"); !ok || got != "m-1" {
		t.Errorf("путь в словаре: %q %v", got, ok)
	}
	for _, c := range cases {
		if got, ok := sharedInText(c.text, "string_i"); !ok || got != c.want {
			t.Errorf("%q: %q %v, ожидалось %q", c.text, got, ok, c.want)
		}
	}
	if got, ok := sharedInText("nothing here", "string_i"); ok {
		t.Errorf("нашлось лишнее: %q", got)
	}
}

// Плейбук-файл: копия c.at.yaml с заполненной строкой, сам c.yaml не
// меняется; комментарии остаются.
func TestAtPlaybookFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("b.yaml", "service: node_api\nparameters:\n  action: login\n")
	write("c.yaml", "# сценарий c\nservice: node_api\nparameters:\n  action: tune\n  string_i: ?\n")
	write("d.json", "{\"service\": \"node_api\", \"parameters\": {\"string_i\": \"?\"}}\n")
	d := filepath.ToSlash(dir) + "/"
	src := `
alias a(path) {
    text = read(path)
    if contains(text, "login") { return "HTTP/1.1 200\nstring_i: 'k-42'\n" }
    return "got " + path
}
for r in a @ ("` + d + `b.yaml" | "` + d + `c.yaml" | "` + d + `d.json") { print(r) }
`
	out, err := runWith(t, src, func(*Interpreter) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "c.at.yaml") || !strings.Contains(out, "d.at.json") {
		t.Fatalf("вывод:\n%s", out)
	}
	filled, _ := os.ReadFile(filepath.Join(dir, "c.at.yaml"))
	if !strings.Contains(string(filled), `string_i: "k-42"`) || !strings.Contains(string(filled), "# сценарий c") {
		t.Fatalf("c.at.yaml:\n%s", filled)
	}
	orig, _ := os.ReadFile(filepath.Join(dir, "c.yaml"))
	if !strings.Contains(string(orig), "string_i: ?") {
		t.Fatalf("c.yaml изменён:\n%s", orig)
	}
	js, _ := os.ReadFile(filepath.Join(dir, "d.at.json"))
	if !strings.Contains(string(js), `"string_i": "k-42"`) {
		t.Fatalf("d.at.json:\n%s", js)
	}
}

// Команда модуля называет общие строки сама (аннотация shared): ? в
// плейбуке не нужен, параметр добавляется в parameters.
func TestFillPlaybookAddsDeclared(t *testing.T) {
	out, err := fillPlaybook([]byte("service: x\nparameters:\n  a: 1\n"), map[string]string{"token": "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `token: "t1"`) {
		t.Fatalf("плейбук:\n%s", out)
	}
	keys := ParseShared(" token , node_tune:sid=session.id,")
	if len(keys) != 2 || keys[0] != (SharedKey{"", "token", "token"}) || keys[1] != (SharedKey{"node_tune", "sid", "session.id"}) {
		t.Fatalf("ParseShared: %+v", keys)
	}
}

// Команда ochan слева от @: approve @ ("r1" | "r2") — по очереди.
func TestAtCommand(t *testing.T) {
	cmds := &atCommands{shared: map[string][]SharedKey{"node call": {{Service: "node_tune", Param: "token", From: "token"}}}}
	src := `
r = approve @ ("r1" | "r2")
print(join(r, ";"))
s = node.call @ ({"service": "node_tune", "parameters": {}} | {"service": "node_login", "parameters": {}})
print(join(s, ";"))
`
	out, err := runWith(t, src, func(in *Interpreter) { in.SetCommands(cmds) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "approve r1;approve r2\n") {
		t.Fatalf("approve: %q", out)
	}
	// node_tune ждёт token, который даёт ответ node_login: вызывается вторым.
	if len(cmds.runs) != 4 || !strings.Contains(cmds.runs[2][2], "node_login") ||
		!strings.Contains(cmds.runs[3][2], "token=T") {
		t.Fatalf("порядок и подстановка: %q", cmds.runs)
	}
}

type atCommands struct {
	shared map[string][]SharedKey
	runs   [][]string
}

func (f *atCommands) Resolve(args []string) ([]string, string, bool, error) {
	if len(args) == 0 || (args[0] != "approve" && args[0] != "node") {
		return nil, "", false, os.ErrNotExist
	}
	return args, strings.Join(args, "."), true, nil
}

func (f *atCommands) Run(_ context.Context, argv []string) (string, error) {
	f.runs = append(f.runs, argv)
	if argv[0] == "approve" {
		return strings.Join(argv, " "), nil
	}
	last := argv[len(argv)-1]
	if strings.Contains(last, "login") {
		return "token: T", nil
	}
	return "call " + last, nil
}

func (f *atCommands) Shared(argv []string) []SharedKey {
	return f.shared[strings.Join(argv, " ")]
}

// Проверка до запуска: @ — список; неизвестное имя слева — не ошибка
// (это может быть команда ochan), неизвестное имя в операнде — ошибка.
func TestCheckAt(t *testing.T) {
	if problems := check(t, "alias a(s) { return s }\nr = a @ (a @ (\"b\" | \"c\") | \"d\")\nn = len(r)\nx = approve @ (\"r1\")\n"); len(problems) > 0 {
		t.Fatalf("лишние замечания:\n%s", joinProblems(problems))
	}
	if problems := check(t, "alias a(s) { return s }\nr = a @ (нет_такого | \"d\")\n"); len(problems) == 0 {
		t.Fatal("неизвестное имя в операнде прошло проверку")
	}
	if _, err := Parse("<тест>", "x = a @ \"b\"\n"); err == nil {
		t.Fatal("@ без скобки разобрался")
	}
}

// Алиас библиотеки слева от @ — зависимость заявки, как обычный вызов.
func TestAtUsesLibraryAlias(t *testing.T) {
	lib, err := Parse("lib.ochan", "alias send(s) { return s }\n")
	if err != nil {
		t.Fatal(err)
	}
	file, err := Parse("<тест>", "r = devops.send @ (\"a\" | \"b\")\n")
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := registry.AddFile("devops", lib); err != nil {
		t.Fatal(err)
	}
	used := Uses(file, registry)
	if len(used) != 1 || used[0].Name != "send" {
		t.Fatalf("алиас библиотеки слева от @ не попал в зависимости: %v", used)
	}
}
