package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"octochan/lang"
)

// scriptEnv — домашний каталог с библиотекой алиасов и рабочий каталог
// с конфигом, как у настоящего пользователя.
type scriptEnv struct {
	t       *testing.T
	home    string
	work    string
	library string
}

func newScriptEnv(t *testing.T) *scriptEnv {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OCTOCHAN_CONFIG", filepath.Join(home, "config.yaml"))
	if err := initConfig(); err != nil {
		t.Fatal(err)
	}
	// Реестр алиасов кешируется на процесс; каждому тесту — свой дом.
	resetAliasCache()

	e := &scriptEnv{t: t, home: home, work: t.TempDir()}
	e.library = filepath.Join(home, ".octochan", "aliases", "lib.ochan")
	e.write(e.library, "alias поднять(c: config, n: int) {\n    put(c, \"max_connections\", str(n))\n    return c\n}\n")
	e.write(e.path("prod.conf"), "max_connections = 100\nwork_mem = 4MB\n")
	e.write(e.path("tune.ochan"), `c = поднять(config("prod.conf"), 200)
save(c, "prod.conf")
write("report.txt", "max_connections: " + param("prod.conf", "max_connections"))
`)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(e.work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	return e
}

func (e *scriptEnv) path(name string) string { return filepath.Join(e.work, name) }

func (e *scriptEnv) write(path, content string) {
	e.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *scriptEnv) read(name string) string {
	e.t.Helper()
	data, err := os.ReadFile(e.path(name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

// ochan выполняет команду так, как её выполнила бы строка оболочки.
func (e *scriptEnv) ochan(args ...string) (string, error) {
	e.t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	defer resetFlags(rootCmd)
	err := rootCmd.ExecuteContext(context.Background())
	return out.String(), err
}

func (e *scriptEnv) must(args ...string) string {
	e.t.Helper()
	out, err := e.ochan(args...)
	if err != nil {
		e.t.Fatalf("ochan %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *scriptEnv) fails(want string, args ...string) {
	e.t.Helper()
	out, err := e.ochan(args...)
	if err == nil {
		e.t.Fatalf("ochan %s прошла, а должна была отказать\n%s", strings.Join(args, " "), out)
	}
	if !strings.Contains(err.Error(), want) {
		e.t.Fatalf("ochan %s: ожидался отказ с %q, получено: %v", strings.Join(args, " "), want, err)
	}
}

func resetAliasCache() {
	aliasRegistryOnce = sync.Once{}
	aliasRegistry, aliasLoadErrs = nil, nil
}

var requestIDPattern = regexp.MustCompile(`Заявка ([0-9a-f]+) создана`)

func TestScriptRequestLifecycle(t *testing.T) {
	e := newScriptEnv(t)
	original := e.read("prod.conf")

	plan := e.must("plan", "tune.ochan")
	for _, want := range []string{"изменить   max_connections: 100 -> 200", "записать   файл — новый файл"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("в плане нет %q:\n%s", want, plan)
		}
	}
	if e.read("prod.conf") != original {
		t.Fatal("plan изменил файл")
	}

	created := e.must("request", "tune.ochan")
	match := requestIDPattern.FindStringSubmatch(created)
	if match == nil {
		t.Fatalf("не найден номер заявки:\n%s", created)
	}
	id := match[1]

	card := e.must("requests", id)
	for _, want := range []string{"Сценарий:", `| save(c, "prod.conf")`, "Алиасы:    " + e.library, "Песочница: нет"} {
		if !strings.Contains(card, want) {
			t.Fatalf("в карточке нет %q:\n%s", want, card)
		}
	}

	e.fails("не согласована", "apply", "--request", id)
	e.must("approve", id)

	// Поменялся файл, из которого сценарий строит конфиг: записано было
	// бы не то, что согласовали.
	e.write(e.path("prod.conf"), "max_connections = 100\nwork_mem = 64MB\n")
	e.fails("план сценария изменился после согласования", "apply", "--request", id)
	e.write(e.path("prod.conf"), original)

	// Поменялся алиас библиотеки, который сценарий вызывает.
	e.write(e.library, "alias поднять(c: config, n: int) {\n    put(c, \"max_connections\", \"1\")\n    return c\n}\n")
	e.fails("файл алиасов "+e.library+" изменился", "apply", "--request", id)
	e.write(e.library, "alias поднять(c: config, n: int) {\n    put(c, \"max_connections\", str(n))\n    return c\n}\n")

	if e.read("prod.conf") != original {
		t.Fatal("отказ выполнения всё-таки изменил файл")
	}

	e.must("apply", "--request", id)
	if got := e.read("prod.conf"); !strings.Contains(got, "max_connections = 200") {
		t.Fatalf("сценарий не выполнен: %q", got)
	}
	if got := e.read("report.txt"); got != "max_connections: 200" {
		t.Fatalf("report.txt: %q", got)
	}

	e.fails("не согласована (выполнена)", "apply", "--request", id)
	if history := e.must("history"); !strings.Contains(history, "сценарий tune.ochan") {
		t.Fatalf("выполнение не попало в журнал:\n%s", history)
	}
}

// Песочница, в которой строили план, действует и при выполнении: её
// границы хранятся в заявке, а не берутся из флагов того, кто выполняет.
func TestScriptRequestKeepsSandbox(t *testing.T) {
	e := newScriptEnv(t)

	outside := filepath.Join(t.TempDir(), "secret.conf")
	e.write(outside, "password = x\n")
	e.write(e.path("leak.ochan"), "print(read(\""+filepath.ToSlash(outside)+"\"))\n")

	e.fails("вне разрешённых каталогов", "plan", "--sandbox", "leak.ochan")
	e.fails("вне разрешённых каталогов", "request", "--sandbox", "leak.ochan")

	created := e.must("request", "--sandbox", "--root", e.work, "tune.ochan")
	id := requestIDPattern.FindStringSubmatch(created)[1]
	card := e.must("requests", id)
	real, err := filepath.EvalSymlinks(e.work)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card, "Песочница: каталоги "+real) {
		t.Fatalf("песочница не сохранена в заявке:\n%s", card)
	}

	e.must("approve", id)
	e.must("apply", "--request", id)
	if got := e.read("prod.conf"); !strings.Contains(got, "max_connections = 200") {
		t.Fatalf("сценарий не выполнен: %q", got)
	}
}

// now() и rand() при выполнении заявки возвращают то же, что при
// построении плана: иначе согласованный план не совпал бы с выполнением.
func TestScriptRequestFixesClockAndSeed(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("stamp.ochan"), "write(\"stamp.txt\", format(\"%s %d\", now(), rand(1000000000)))\n")

	for _, sandboxed := range []bool{false, true} {
		args := []string{"request", "stamp.ochan"}
		if sandboxed {
			args = []string{"request", "--sandbox", "--root", e.work, "stamp.ochan"}
		}
		created := e.must(args...)
		id := requestIDPattern.FindStringSubmatch(created)[1]
		if card := e.must("requests", id); !strings.Contains(card, "Время:     ") {
			t.Fatalf("в карточке нет времени плана:\n%s", card)
		}
		e.must("approve", id)
		e.must("apply", "--request", id)
		if got := e.read("stamp.txt"); !strings.Contains(got, "T") {
			t.Fatalf("stamp.txt: %q", got)
		}
		if err := os.Remove(e.path("stamp.txt")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScriptFlagsNeedSandbox(t *testing.T) {
	e := newScriptEnv(t)
	e.fails("действуют только вместе с --sandbox", "plan", "--root", e.work, "tune.ochan")
	e.fails("укажите --script", "plan", "--sandbox", "prod.conf", "prod.conf")
	e.fails("укажите --script", "request", "--allow-env", "HOME", "prod.conf", "prod.conf")
	e.fails("--name задаёт имя сценария, поданного на stdin", "plan", "--name", "x", "tune.ochan")
}

func TestCheckCommand(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("broken.ochan"), "x = 1\nif x > 5 {\n    поднятъ(config(\"prod.conf\"), 1)\n}\n")

	if out := e.must("check", "tune.ochan"); !strings.Contains(out, "ошибок не найдено") {
		t.Fatalf("вывод: %s", out)
	}
	out, err := e.ochan("check", "broken.ochan")
	if err == nil || !strings.Contains(out, `broken.ochan:3:12: неизвестная функция "поднятъ"; возможно, имелось в виду "поднять"`) {
		t.Fatalf("ошибка в непройденной ветке не найдена: %v\n%s", err, out)
	}

	// План проверяет сценарий до выполнения: ничего не выполнялось.
	e.fails("сценарий не прошёл проверку", "plan", "broken.ochan")

	if out := e.must("check"); !strings.Contains(out, "библиотека алиасов: ошибок не найдено") {
		t.Fatalf("вывод: %s", out)
	}
}

// В заявке видно, с какой версией пакета строился план.
func TestScriptRequestShowsPackageVersion(t *testing.T) {
	e := newScriptEnv(t)
	pkg := filepath.Join(e.home, ".octochan", "modules", "pgconf")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(pkg, "lib.ochan"), "alias limit() { return 300 }\n")
	e.write(filepath.Join(pkg, lang.ManifestName), "version: 1.4\n")
	e.write(e.path("pkg.ochan"), "write(\"limit.txt\", str(pgconf.limit()))\n")
	resetAliasCache()

	created := e.must("request", "pkg.ochan")
	id := requestIDPattern.FindStringSubmatch(created)[1]
	if card := e.must("requests", id); !strings.Contains(card, "Пакет:     pgconf 1.4") {
		t.Fatalf("версии пакета нет в карточке:\n%s", card)
	}
}
