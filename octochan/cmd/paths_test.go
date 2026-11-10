package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ochan/workspace"
)

// reinit заново определяет пути, как новый запуск ochan. Конфиг теста
// удаляется: прочитанный с диска, он остался бы в памяти viper и дошёл бы
// до следующих тестов вместе с путями этого.
func reinit(t *testing.T) {
	t.Helper()
	if err := tryInit(t); err != nil {
		t.Fatal(err)
	}
	resetAliasCache()
}

func tryInit(t *testing.T) error {
	t.Helper()
	if err := os.Remove(os.Getenv("OCTOCHAN_CONFIG")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return initConfig()
}

// Каталог модулей, вынесенный окружением: пакет ставится и читается там, а
// защита рабочего каталога действует и на него — для сценариев и ролей.
func TestRelocatedModules(t *testing.T) {
	e := newScriptEnv(t)
	modules := filepath.Join(t.TempDir(), "modules")
	t.Setenv(workspace.DirModules.Env(), modules)
	reinit(t)

	pkg := filepath.Join(e.work, "util")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(pkg, "util.ochan"), "alias double(x: int) { return x * 2 }\n")
	e.write(filepath.Join(pkg, "package.yaml"), "version: 1.0\n")
	e.must("!!install", pkg)
	installed := filepath.Join(modules, "util", "util.ochan")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("пакет не в вынесенном каталоге: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".octochan", "modules", "util")); err == nil {
		t.Fatal("пакет встал в рабочий каталог, хотя модули вынесены")
	}
	if out := e.must("run", "util.double", "21"); strings.TrimSpace(out) != "42" {
		t.Fatalf("алиас из вынесенного каталога: %q", out)
	}
	out := e.must("paths")
	for _, want := range []string{modules, "окружение OCTOCHAN_MODULES", "по умолчанию; меняется: OCTOCHAN_HOME",
		"по умолчанию; меняется: OCTOCHAN_LOGS, paths.logs", "Только вместе с рабочим каталогом: роли, журнал аудита"} {
		if !strings.Contains(out, want) {
			t.Fatalf("paths: нет %q:\n%s", want, out)
		}
	}

	// Сценарию вынесенные модули закрыты, как рабочий каталог: записав в
	// них, он подложил бы плагин.
	e.write(e.path("plant.ochan"), "write(\""+filepath.ToSlash(filepath.Join(modules, "evil.hcplugin"))+"\", \"x\")\n")
	e.fails("рабочий каталог ochan", "run", "plant.ochan")
	e.must("role", "create", "dan", "--allow", "read,apply")
	t.Setenv(EnvRole, "dan")
	e.fails("рабочий каталог ochan: роли dan он закрыт", "print", installed)
	e.fails("содержит рабочий каталог ochan", "run", "--sandbox", "--root", filepath.Dir(modules), "plant.ochan")
	if _, err := os.Stat(filepath.Join(modules, "evil.hcplugin")); err == nil {
		t.Fatal("сценарий записал в каталог модулей")
	}
}

// Каталоги узла из config.yaml: paths.*; окружение сильнее; прежний ключ
// журналов читается; относительный путь — отказ при запуске.
func TestPathsFromConfig(t *testing.T) {
	e := newScriptEnv(t)
	reports, logs, other := filepath.Join(t.TempDir(), "reports"), filepath.Join(t.TempDir(), "logs"), filepath.Join(t.TempDir(), "x")
	setting(t, "paths.reports", reports)
	setting(t, "defaults.log_path", logs)
	reinit(t)
	fm, _ := mustFileManager()
	if fm.ReportsDir() != reports || fm.LogsDir() != logs {
		t.Fatalf("из конфига: отчёты %s, журналы %s", fm.ReportsDir(), fm.LogsDir())
	}
	e.write(e.path("test.conf"), "max_connections = 200\n")
	if out := e.must("diff", "--no-snapshot", "prod.conf", "test.conf"); !strings.Contains(out, "Отчёт сохранён: "+reports) {
		t.Fatalf("отчёт не в вынесенном каталоге:\n%s", out)
	}
	if out := e.must("paths"); !strings.Contains(out, "config.yaml paths.reports") || !strings.Contains(out, "config.yaml defaults.log_path") {
		t.Fatalf("paths:\n%s", out)
	}

	// Новый ключ сильнее прежнего, окружение — сильнее конфига.
	setting(t, "paths.logs", other)
	t.Setenv(workspace.DirReports.Env(), other)
	reinit(t)
	fm, _ = mustFileManager()
	if fm.LogsDir() != other || fm.ReportsDir() != other {
		t.Fatalf("порядок: журналы %s, отчёты %s", fm.LogsDir(), fm.ReportsDir())
	}

	setting(t, "paths.modules", "mods")
	if err := tryInit(t); err == nil || !strings.Contains(err.Error(), "config.yaml paths.modules: mods — нужен полный путь") {
		t.Fatalf("относительный путь из конфига: %v", err)
	}
	setting(t, "paths.modules", "")
	reinit(t)
}

// Оболочка без терминала: строки из ввода выполняются, как набранные, —
// команды, инструкции, блоки; ошибки считаются и дают код возврата, stdin
// командам недоступен, в историю ничего не пишется.
func TestLineShell(t *testing.T) {
	e := newScriptEnv(t)
	t.Cleanup(shellReset)
	t.Cleanup(func() { setContext(rootCmd, context.Background()) })
	run := func(script string, stop bool) (string, error) {
		t.Helper()
		defer setContext(rootCmd, context.Background())
		var err error
		out := captureStdout(t, func() { err = runLineShell(context.Background(), strings.NewReader(script), stop) })
		return out, err
	}

	// Метка порядка байт в начале, комментарий, переменная между строками,
	// блок в несколько строк, exit кончает ввод.
	out, err := run("\uFEFFping\n# комментарий\n\nn = 20\nif n > 10 {\n  print(n + 1)\n}\nexit\nprint(\"после exit\")\n", false)
	if err != nil || out != "pong\n21\n" {
		t.Fatalf("сценарий: %v\n%q", err, out)
	}
	if shellLines || stdinTaken {
		t.Fatal("построчный режим не снят после ввода")
	}
	fm, _ := mustFileManager()
	if state, err := workspace.LoadState(fm.StatePath()); err != nil || len(state.History) != 0 {
		t.Fatalf("строки конвейера попали в историю: %v %v", state.History, err)
	}

	// Ошибка не останавливает ввод, но даёт код возврата; print без файла
	// не съедает остаток сценария.
	out, err = run("ping\nprint\nfail(\"сломано\")\nping\n", false)
	if err == nil || !strings.Contains(err.Error(), "строк с ошибкой: 2") || out != "pong\npong\n" {
		t.Fatalf("ошибки: %v\n%q", err, out)
	}
	out, err = run("ping\nfail(\"сломано\")\nping\n", true)
	if err == nil || !strings.Contains(err.Error(), "строка 2 кончилась ошибкой — остановлено") || out != "pong\n" {
		t.Fatalf("--stop-on-error: %v\n%q", err, out)
	}
	if _, err = run("if 1 > 0 {\n  print(1)\n", false); err == nil || !strings.Contains(err.Error(), "ввод кончился внутри блока") {
		t.Fatalf("незакрытый блок: %v", err)
	}
	// Вторая оболочка читала бы из того же stdin.
	if _, err = run("shell\n", false); err == nil {
		t.Fatal("вложенная оболочка в построчном режиме принята")
	}

	// Комментарию право на язык не нужно; инструкции — нужно.
	e.must("role", "create", "slave", "--allow", "read,plan")
	e.must("set", "role", "slave")
	if out, err = run("# только чтение\nping\n", false); err != nil || out != "pong\n" {
		t.Fatalf("комментарий у роли без права на язык: %v\n%q", err, out)
	}
	if _, err = run("print(1)\n", false); err == nil {
		t.Fatal("инструкция у роли без права на язык принята")
	}
	e.must("set", "role", "master")
}
