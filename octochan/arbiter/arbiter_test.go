package arbiter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/fp"
	"octochan/lang"
)

// plan выполняет сценарий в режиме плана и проверяет по нему правила
// промпта.
func plan(t *testing.T, rules, script string) []Outcome {
	t.Helper()

	prog, diags := fp.Parse("правила.fp", "goal: x\nclasses:\n  Goal : \"x\"\n"+rules)
	if len(diags) > 0 {
		t.Fatalf("правила не разбираются: %v", diags)
	}

	dir := t.TempDir()
	for _, name := range []string{"prod.conf", "test.conf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("port = 5432\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file, err := lang.Parse("сценарий.ochan", `
alias store(c, path) { return save(c, path) }
alias verify_config(path) { return validate(path) }
alias wrapper(path) { return store(config(path), path) }
`+script)
	if err != nil {
		t.Fatal(err)
	}
	registry := lang.NewRegistry()
	for _, decl := range file.Aliases {
		if err := registry.Add("", decl); err != nil {
			t.Fatal(err)
		}
	}
	interp := lang.NewInterpreter(context.Background(), registry, &strings.Builder{})
	interp.SetDir(dir)
	interp.SetPlanMode()
	if err := interp.RunFile(file); err != nil {
		t.Fatal(err)
	}
	return Check(prog.Rules, interp.Effects(), interp.Invocations())
}

func expectOK(t *testing.T, outcomes []Outcome) {
	t.Helper()
	for _, o := range outcomes {
		if !o.OK() {
			t.Fatalf("%s нарушено: %v", Describe(o.Rule), o.Violations)
		}
	}
}

func expectViolation(t *testing.T, outcomes []Outcome, fragment string) {
	t.Helper()
	for _, o := range outcomes {
		for _, v := range o.Violations {
			if strings.Contains(v, fragment) {
				return
			}
		}
	}
	t.Fatalf("нет нарушения с %q: %+v", fragment, outcomes)
}

func TestTargetName(t *testing.T) {
	for path, want := range map[string]string{
		"prod.conf":            "prod_conf",
		"/srv/pg/web-1.conf":   "web_1_conf",
		"C:/configs/Prod.CONF": "prod_conf",
		"web_group":            "web_group",
	} {
		if got := TargetName(path); got != want {
			t.Errorf("TargetName(%q) = %q, ожидалось %q", path, got, want)
		}
	}
}

func TestForbidBuiltin(t *testing.T) {
	rule := "hard forbid save(except = {prod_conf})\n"
	expectOK(t, plan(t, rule, `save(config("prod.conf"), "prod.conf")`))
	expectViolation(t, plan(t, rule, `save(config("prod.conf"), "test.conf")`), "save в")
	expectOK(t, plan(t, rule, `write("note.txt", "x")`))

	expectViolation(t, plan(t, "??n_write()\n", `write("note.txt", "x")`), "note_txt")
}

// Исключение оправдывает только то, что вызов записал.
func TestForbidAlias(t *testing.T) {
	rule := "??n_store(except: {prod_conf})\n"
	expectOK(t, plan(t, rule, `store(config("prod.conf"), "prod.conf")`))
	expectViolation(t, plan(t, rule, `store(config("prod.conf"), "test.conf")`), "пишет вне исключений: test_conf")

	// Вложенный вызов — тоже вызов: запрет не обойти обёрткой.
	expectViolation(t, plan(t, rule, `wrapper("test.conf")`), "вызов store(test.conf, test.conf)")

	// Без исключений запрещён любой вызов.
	expectViolation(t, plan(t, "hard forbid verify_config()\n", `verify_config("prod.conf")`), "вызов verify_config(prod.conf)")

	// Запрет внешнего алиаса покрывает записи внутренних.
	expectViolation(t, plan(t, "hard forbid wrapper(except = {prod_conf})\n", `wrapper("test.conf")`), "test_conf")
	expectOK(t, plan(t, "hard forbid wrapper(except = {prod_conf})\n", `wrapper("prod.conf")`))
}

// Ветка, которая в плане не выполнилась, правило не нарушает: план — это
// то, что сценарий сделает.
func TestForbidOnlyWhatRuns(t *testing.T) {
	expectOK(t, plan(t, "hard forbid store()\n", `if false { store(config("prod.conf"), "prod.conf") }`))
}

func TestRequire(t *testing.T) {
	rule := "??a_verify_config(scope: {prod_conf, test_conf})\n"
	expectOK(t, plan(t, rule, `verify_config("prod.conf")
verify_config("test.conf")`))
	expectViolation(t, plan(t, rule, `verify_config("prod.conf")`), "verify_config не выполняется для: test_conf")

	expectViolation(t, plan(t, "hard require verify_config()\n", `print(1)`), "в плане нет verify_config")
	expectOK(t, plan(t, "hard require save(scope = prod_conf)\n", `save(config("prod.conf"), "prod.conf")`))
	expectViolation(t, plan(t, "hard require save(scope = prod_conf)\n", `write("prod.conf", "x")`), "save не выполняется")
}

func TestDescribe(t *testing.T) {
	prog, _ := fp.Parse("x", "goal: x\nclasses:\n  Goal : \"x\"\n??n_disconnect(except: {a, b})\n")
	if got := Describe(prog.Rules[0]); got != "hard forbid disconnect(except = {a, b})" {
		t.Fatalf("Describe: %q", got)
	}
}
