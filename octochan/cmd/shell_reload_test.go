package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ochan/modules/pluginhost"

	"github.com/spf13/viper"
)

// runShellScript — строки оболочки без терминала: вывод и ошибка ввода.
func runShellScript(t *testing.T, script string) (string, error) {
	t.Helper()
	defer setContext(rootCmd, context.Background())
	var err error
	// Вывод команд — в stdout, как у настоящей оболочки: e.must оставляет
	// rootCmd писать в свой буфер.
	out := captureStdout(t, func() {
		rootCmd.SetOut(nil)
		err = runLineShell(context.Background(), strings.NewReader(script), false)
	})
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
	// «>>|» дописывает в конец, файла нет — создаёт.
	if _, err := runShellScript(t, "ping >>| ping.txt\nprint(\"ещё\") >>| ping.txt\nping >>| new.txt\n"+
		"for x in [3] {\n  print(x)\n} >>| block.txt\n"); err != nil {
		t.Fatalf(">>|: %v", err)
	}
	for name, want := range map[string]string{"ping.txt": "новое\npong\nещё\n", "new.txt": "pong\n", "block.txt": "1\n2\n3\n"} {
		if got := read(name); got != want {
			t.Fatalf(">>| %s: %q, ожидалось %q", name, got, want)
		}
	}
	// Пустой вывод — не «+0 байт», а предупреждение в stderr.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldErr := os.Stderr
	os.Stderr = w
	_, runErr := runShellScript(t, "for x in [] {\n  print(x)\n} >>| ping.txt\nfor x in [] {\n  print(x)\n} >| empty.txt\n")
	os.Stderr = oldErr
	w.Close()
	warned, _ := io.ReadAll(r)
	if runErr != nil || !strings.Contains(string(warned), "вывод пуст — "+e.path("ping.txt")+" не изменился") ||
		!strings.Contains(string(warned), "вывод пуст — "+e.path("empty.txt")+" пуст") || strings.Contains(string(warned), "+0 байт") {
		t.Fatalf("пустой вывод: %v\n%s", runErr, warned)
	}
	// Ошибка строки — не в файл: по пустому файлу не понять, что не прошло.
	if _, err := runShellScript(t, "fail(\"сломано\") >| err.txt\n"); err == nil || read("err.txt") != "" {
		t.Fatalf("ошибка ушла в файл или не засчитана: %v %q", err, read("err.txt"))
	}

	// Неверное перенаправление и «>|» внутри блока — ошибка строки, файла нет.
	for _, script := range []string{"ping >|\n", ">| x.txt\n", "ping >| a.txt b.txt\n", "ping >| a.txt >| b.txt\n",
		"ping >>| a.txt >>| b.txt\n", "if 1 > 0 { >>| x.txt\n}\n",
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
	for _, op := range []string{">|", ">>|"} {
		if _, err := runShellScript(t, "ping "+op+" reader.txt\n"); err == nil {
			t.Fatalf("роль только для чтения записала файл через %s", op)
		}
	}
	if _, err := os.Stat(e.path("reader.txt")); err == nil {
		t.Fatal("файл роли только для чтения создан")
	}
	e.must("set", "role", "master")
}

// Ошибки неверного перенаправления называют, что не так; «>>|» —
// дописать, «>|» — перезаписать.
func TestSplitRedirect(t *testing.T) {
	for line, want := range map[string]string{
		"ping >|":           "после >| нет файла",
		"ping >>|":          "после >>| нет файла",
		">| x":              "перед >| нет команды",
		">>| x":             "перед >>| нет команды",
		"ping >| a b":       "один файл, а их 2",
		"ping >| a >| b":    "одно в строке",
		"ping >>| a >| b":   "одно в строке",
		"ping >| 'a b'":     "",
		"print(\">| x\")":   "-",
		"print(\">>| x\")":  "-",
		"ping # >| x":       "-",
		"stats db1 >| ~/s":  "",
		"stats db1 >>| ~/s": "",
	} {
		command, r, err := splitRedirect(line)
		switch want {
		case "-":
			if r != nil || err != nil || command != line {
				t.Fatalf("%q: перенаправления нет, а разобрано %q %+v %v", line, command, r, err)
			}
		case "":
			if r == nil || err != nil || r.target == "" || strings.Contains(command, ">|") ||
				r.append != strings.Contains(line, ">>|") {
				t.Fatalf("%q: %q %+v %v", line, command, r, err)
			}
		default:
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%q: ожидалось %q, получено %v", line, want, err)
			}
		}
	}
	if _, r, _ := splitRedirect("ping >| 'a b'"); r == nil || r.target != "a b" || r.append {
		t.Fatalf("путь в кавычках: %+v", r)
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

	// Файл модуля сломали (собрали библиотеку вместо модуля): !!reload
	// говорит почему, а загруженный модуль продолжает работать.
	if err := os.WriteFile(installed+".new", []byte("!<arch>\nне программа"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(installed+".new", installed); err != nil {
		t.Fatal(err)
	}
	if err := pluginhost.WriteChecksum(installed); err != nil {
		t.Fatal(err)
	}
	out, err = runShellScript(t, "!!reload\npgstat where\n")
	if err == nil || !strings.Contains(out, "pgstat:") {
		t.Fatalf("сломанный файл модуля унёс рабочий модуль: %v\n[%s]", err, out)
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

// Вывод команды модуля — и cmd.Println, и fmt.Println мимо cmd — уходит в
// файл перенаправления с первой же строки: раньше печать мимо cmd go-plugin
// пересылал позже ответа, и первый файл оставался пустым.
func TestShellRedirectModuleOutput(t *testing.T) {
	module := buildModule(t, "ochan/examples/classmodule", "pgstat")
	e := newScriptEnv(t)
	t.Cleanup(shellReset)
	t.Cleanup(func() {
		pluginhost.GetModuleManager().Cleanup()
		refreshModuleCommands()
		syncRunners()
	})
	setting(t, warnMissingKey, false)
	e.must("!!install", module)
	out, err := runShellScript(t, "pgraw >| first.txt\npgstat show >| second.txt\npgraw >>| all.txt\npgraw >>| all.txt\n")
	if err != nil || strings.Contains(out, "pgraw") {
		t.Fatalf("вывод мимо файлов: %v\n%s", err, out)
	}
	for name, want := range map[string]string{"first.txt": "pgraw: выполнено\n", "all.txt": "pgraw: выполнено\npgraw: выполнено\n",
		"second.txt": "pg_stat: соединений 42, попаданий в кеш 99.1%\n"} {
		data, err := os.ReadFile(e.path(name))
		if err != nil || string(data) != want {
			t.Fatalf("%s: %q, ожидалось %q (%v)", name, data, want, err)
		}
	}
	// ochan() сценария тоже получает печать мимо cmd.
	if out, err := runShellScript(t, `print("[" + ochan("pgraw") + "]")`+"\n"); err != nil || out != "[pgraw: выполнено]\n" {
		t.Fatalf("ochan(pgraw): %v %q", err, out)
	}
}
