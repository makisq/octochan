package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ochan/pluginhost"

	"github.com/spf13/viper"
)

// runShellScript — строки оболочки без терминала: вывод и ошибка ввода.
func runShellScript(t *testing.T, script string) (string, error) {
	t.Helper()
	defer setContext(rootCmd, context.Background())
	var err error
	out := captureStdout(t, func() { err = runLineShell(context.Background(), strings.NewReader(script), false) })
	return out, err
}

// «строка >| файл» пишет вывод строки в файл: команды, инструкции языка,
// блок целиком; ошибки — на экран; внутри кавычек «>|» — текст.
func TestShellRedirect(t *testing.T) {
	e := newScriptEnv(t)
	t.Cleanup(shellReset)
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(e.path(name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return string(data)
	}

	out, err := runShellScript(t, "ping >| ping.txt\n"+
		"print(1 + 1) >| 'два файла.txt'\n"+
		"for x in [1, 2] {\n  print(x)\n} >| block.txt\n"+
		"print(\"a >| b\")\n"+
		"ping\n")
	if err != nil || out != "a >| b\npong\n" {
		t.Fatalf("вывод мимо файлов: %v\n%q", err, out)
	}
	for name, want := range map[string]string{"ping.txt": "pong\n", "два файла.txt": "2\n", "block.txt": "1\n2\n"} {
		if got := read(name); got != want {
			t.Fatalf("%s: %q, ожидалось %q", name, got, want)
		}
	}

	// Перезапись, а не дописывание: как «>|» в bash.
	if _, err := runShellScript(t, "print(\"новое\") >| ping.txt\n"); err != nil || read("ping.txt") != "новое\n" {
		t.Fatalf("файл не перезаписан: %v %q", err, read("ping.txt"))
	}
	// Ошибка строки — не в файл: по пустому файлу не понять, что не прошло.
	if _, err := runShellScript(t, "fail(\"сломано\") >| err.txt\n"); err == nil || read("err.txt") != "" {
		t.Fatalf("ошибка ушла в файл или не засчитана: %v %q", err, read("err.txt"))
	}

	// Неверное перенаправление и «>|» внутри блока — ошибка строки, файла нет.
	for _, script := range []string{"ping >|\n", ">| x.txt\n", "ping >| a.txt b.txt\n", "ping >| a.txt >| b.txt\n",
		"if 1 > 0 { >| x.txt\n}\n", "if 1 > 0 {\nprint(1) >| x.txt\n}\n"} {
		if _, err := runShellScript(t, script); err == nil {
			t.Fatalf("%q принято", script)
		}
	}
	for _, name := range []string{"x.txt", "a.txt", "b.txt"} {
		if _, err := os.Stat(e.path(name)); err == nil {
			t.Fatalf("неверное перенаправление создало %s", name)
		}
	}

	// Запись в файл — право tee: роли только для чтения перенаправление закрыто.
	e.must("role", "create", "reader", "--allow", "read")
	e.must("set", "role", "reader")
	if _, err := runShellScript(t, "ping >| reader.txt\n"); err == nil {
		t.Fatal("роль только для чтения записала файл перенаправлением")
	}
	if _, err := os.Stat(e.path("reader.txt")); err == nil {
		t.Fatal("файл роли только для чтения создан")
	}
	e.must("set", "role", "master")
}

// Ошибки неверного перенаправления называют, что не так.
func TestSplitRedirect(t *testing.T) {
	for line, want := range map[string]string{
		"ping >|":          "после >| нет файла",
		">| x":             "перед >| нет команды",
		"ping >| a b":      "один файл, а их 2",
		"ping >| a >| b":   "один раз в строке",
		"ping >| 'a b'":    "",
		"print(\">| x\")":  "-",
		"ping # >| x":      "-",
		"stats db1 >| ~/s": "",
	} {
		command, target, ok, err := splitRedirect(line)
		switch want {
		case "-":
			if ok || err != nil || command != line {
				t.Fatalf("%q: перенаправления нет, а разобрано %q %q %v %v", line, command, target, ok, err)
			}
		case "":
			if !ok || err != nil || target == "" || strings.Contains(command, ">|") {
				t.Fatalf("%q: %q %q %v %v", line, command, target, ok, err)
			}
		default:
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%q: ожидалось %q, получено %v", line, want, err)
			}
		}
	}
	if _, target, _, _ := splitRedirect("ping >| 'a b'"); target != "a b" {
		t.Fatalf("путь в кавычках: %q", target)
	}
}

// !!reload перечитывает конфиг и загружает модули заново: пересобранный
// модуль работает без выхода из оболочки, переменные сеанса остаются.
func TestShellReload(t *testing.T) {
	runner := buildModule(t, "ochan/examples/runnermodule", "demo-runner")
	rebuilt := buildModule(t, "ochan/examples/classmodule", "pgstat")
	e := newScriptEnv(t)
	t.Cleanup(shellReset)
	t.Cleanup(func() {
		pluginhost.GetModuleManager().Cleanup()
		refreshModuleCommands()
		syncRunners()
	})
	e.must("!!install", runner)

	out, err := runShellScript(t, "n = 41\n!!reload\nprint(n + 1)\n")
	if err != nil || !strings.Contains(out, "Перечитано: конфиг") || !strings.Contains(out, "модули: demo-runner") ||
		!strings.HasSuffix(out, "42\n") {
		t.Fatalf("!!reload: %v\n%s", err, out)
	}

	// Модуль пересобран на месте (тот же файл, новый код), конфиг поправлен
	// руками — !!reload видит и то и другое.
	fm, _ := mustFileManager()
	installed := filepath.Join(fm.ModulesDir(), "demo-runner"+pluginhost.ModuleExtension)
	data, err := os.ReadFile(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	// Сборка заменяет файл переименованием: работающий модуль не перезаписать.
	if err := os.WriteFile(installed+".new", data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(installed+".new", installed); err != nil {
		t.Fatal(err)
	}
	if err := pluginhost.WriteChecksum(installed); err != nil {
		t.Fatal(err)
	}
	config := configPath(fm)
	old, _ := os.ReadFile(config)
	if err := os.WriteFile(config, append(old, []byte("\nreload_probe: 7\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(config, old, 0o600); _ = viper.ReadInConfig() })

	out, err = runShellScript(t, "!!reload\n")
	if err != nil || !strings.Contains(out, "модули: pgstat") || viper.GetInt("reload_probe") != 7 {
		t.Fatalf("после пересборки модуля и правки конфига: %v, reload_probe=%d\n%s", err, viper.GetInt("reload_probe"), out)
	}

	// Сломанный конфиг — отказ, прежние настройки остаются.
	if err := os.WriteFile(config, []byte("paths: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runShellScript(t, "!!reload\n"); err == nil {
		t.Fatal("!!reload со сломанным конфигом прошёл")
	}
	if viper.GetInt("reload_probe") != 7 {
		t.Fatal("сломанный конфиг стёр прежние настройки")
	}
}
