package cmd

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// Команды ochan из сценария: вывод — значение ochan(); меняющая в плане не
// выполняется, а план о ней говорит; права — права самой команды; то, что
// запускает сценарии или ждёт stdin, — отказ.
func TestScriptCommands(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("ping.ochan"), `print("ответ: " + ochan("ping"))`+"\n")
	if out := e.must("run", e.path("ping.ochan")); !strings.Contains(out, "ответ: pong") {
		t.Fatalf("run:\n%s", out)
	}

	e.write(e.path("role.ochan"), `ochan("role", "create", "helper", "--allow", "read")`+"\n")
	out := e.must("plan", "--script", e.path("role.ochan"))
	if !strings.Contains(out, "команда") || !strings.Contains(out, "ochan role create helper --allow read — в плане не выполняется") {
		t.Fatalf("план:\n%s", out)
	}
	if list := e.must("role", "list"); strings.Contains(list, "helper") {
		t.Fatalf("в плане команда выполнилась:\n%s", list)
	}
	e.must("run", e.path("role.ochan"))
	if list := e.must("role", "list"); !strings.Contains(list, "helper") {
		t.Fatalf("при выполнении команды нет:\n%s", list)
	}

	// Права — права команды: роли, которой можно выполнять сценарии, роли
	// создавать нельзя.
	e.must("role", "create", "runner", "--allow", "read,plan,apply")
	e.write(e.path("role2.ochan"), `ochan("role create other --allow read")`+"\n")
	e.must("set", "role", "runner")
	if _, err := e.ochan("run", e.path("role2.ochan")); err == nil || !strings.Contains(err.Error(), "нужно право admin") {
		t.Fatalf("права команды из сценария: %v", err)
	}
	e.must("set", "role", "master")

	for src, want := range map[string]string{
		`ochan("run x.ochan")`:  "из сценария не выполняется",
		`ochan("fp check -")`:   "stdin сценарию недоступен",
		`ochan("nosuch thing")`: "такой команды нет",
	} {
		e.write(e.path("bad.ochan"), src+"\n")
		if _, err := e.ochan("run", e.path("bad.ochan")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", src, err)
		}
	}
}

// Строка оболочки — команда или инструкция языка: слово, которое и
// команда, и имя в языке, — команда, если за ним не ( или =.
func TestShellStatement(t *testing.T) {
	newScriptEnv(t)
	for line, want := range map[string]bool{
		"logs rank --min warning":        false,
		"diff a.conf b.conf":             false,
		`diff("a.conf", "b.conf")`:       true,
		`print("x")`:                     true,
		"print file.txt":                 false,
		"x = 1":                          true,
		"status = 1":                     true,
		`if config.conf port == 8080 ok`: false,
		"if x > 1 { print(x) }":          true,
		"for s in [1, 2] {":              true,
		"ping":                           false,
		"incident --help":                false,
		"!!install pkg":                  false,
		"undefined_thing":                false,
		`twice(21)`:                      true,
	} {
		if got := shellStatement(line); got != want {
			t.Errorf("%q: инструкция — %v, ожидалось %v", line, got, want)
		}
	}
}

// Оболочка: переменные живут между строками, блок пишут в несколько строк,
// алиас объявляют и переобъявляют, команды — и словом, и ochan(); язык в
// консоли — по праву eval.
func TestShellLanguage(t *testing.T) {
	e := newScriptEnv(t)
	t.Cleanup(shellReset)
	shellReset()
	// Оболочка даёт командам контекст строки и отменяет его после неё; тесту
	// дальше нужны живые.
	t.Cleanup(func() { setContext(rootCmd, context.Background()) })
	lines := func(input ...string) string {
		t.Helper()
		defer setContext(rootCmd, context.Background())
		return captureStdout(t, func() {
			for _, line := range input {
				executeShellInput(context.Background(), line)
			}
		})
	}
	out := lines("n = 20", "if n > 10 {", "  print(n + 1)", "}", `print(ochan("ping"))`, "ping",
		"alias twice(x: int) { return x * 2 }", "twice(21)", "alias twice(x: int) { return x * 3 }", "twice(2)", "n")
	for _, want := range []string{"21\n", "pong\npong\n", "42\n", "6\n", "20\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}

	e.must("role", "create", "slave", "--allow", "read,plan")
	e.must("set", "role", "slave")
	if out := lines("print(1)", "ping"); !strings.Contains(out, "роль slave не может выполнять eval: нужно право apply") ||
		!strings.Contains(out, "pong") {
		t.Fatalf("язык без права eval:\n%s", out)
	}
	e.must("set", "role", "master")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}
