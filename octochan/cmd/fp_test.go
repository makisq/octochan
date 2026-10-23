package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/fp"
)

// Скиллы промпта — пакеты алиасов: import сверяется с библиотекой,
// а не принимается на слово.
func TestFPCheckUsesAliasLibrary(t *testing.T) {
	example, err := filepath.Abs("../examples/fp/connections.fp")
	if err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile("../examples/fp/pgconf/pgconf.ochan")
	if err != nil {
		t.Fatal(err)
	}

	e := newScriptEnv(t)
	out, err := e.ochan("fp", "check", example)
	if err == nil || !strings.Contains(out, "⊥undef: скилла @pgconf нет в библиотеке") {
		t.Fatalf("скилл без пакета принят: %v\n%s", err, out)
	}

	if out := e.must("fp", "check", "--standalone", example); !strings.Contains(out, "✅") {
		t.Fatalf("без библиотеки скилл даёт объявленное в provides:\n%s", out)
	}

	pkg := filepath.Join(e.home, ".octochan", "modules", "pgconf")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(pkg, "pgconf.ochan"), string(skill))
	// Реестр алиасов кешируется на процесс: новый пакет виден новому запуску.
	resetAliasCache()

	out = e.must("fp", "check", example)
	for _, want := range []string{"✅", "K:       connection_limit, memory_budget, prod_limit, set_param, store"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}

	// Операции, которой в пакете нет, скилл не даёт.
	e.write(e.path("bad.fp"), "goal: x\nclasses:\n  Goal : \"x\"\nimport @pgconf\n  provides: restart\n")
	e.fails("⊥undef 1", "fp", "check", "bad.fp")
}

func TestFPCheckReportsCodes(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("p.fp"), "goal: x\nclasses:\n  Goal : \"x\"\nrequires: a, b\nhow are you?\n")
	out, err := e.ochan("fp", "check", "p.fp")
	if err == nil || !strings.Contains(err.Error(), "p.fp: отказов 1 (⊥syn 1)") {
		t.Fatalf("ошибка: %v\n%s", err, out)
	}
	if !strings.Contains(out, "p.fp:5:12: ⊥syn: вопрос — не высказывание языка") {
		t.Fatalf("вывод:\n%s", out)
	}

	e.fails("--name задаёт имя промпта, поданного на stdin", "fp", "check", "--name", "x", "p.fp")
}

func TestFPProtocolCommand(t *testing.T) {
	e := newScriptEnv(t)
	if out := e.must("fp", "protocol"); out != fp.Protocol() {
		t.Fatal("ochan fp protocol отдаёт не тот текст, что проверяет пакет fp")
	}
}
