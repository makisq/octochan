package cmd

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ochan/embedded"
	"ochan/fp"

	"github.com/spf13/viper"
)

// Пакет знаний своей модели — обычный пакет реестра: подписанный индекс,
// сумма архива, заявка. Алиасов у него нет; проверка ℒ подключает его
// import'ом с версией, а своя модель должна его видеть — иначе генерации
// нет.
func TestKnowledgePackage(t *testing.T) {
	e := newScriptEnv(t)
	reg := t.TempDir()
	pkg := filepath.Join(e.work, "pg_incidents")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	knowledge := "protocol: 1\ngeneralizations:\n  - key: 75cf2d265cfe\n    shape:\n      - class Log\n" +
		"      - relation Log modifies Storage\n    similarity: 0.8\n    instances: 8\n    outcomes: [1.0, 5.8]\n" +
		"    rules:\n      fp.modifies: [1.0, 5.8]\n"
	e.write(filepath.Join(pkg, fp.KnowledgeName), knowledge)
	e.fails("у пакета знаний нужна версия", "!!install", pkg)
	e.write(filepath.Join(pkg, "package.yaml"), "version: 1.0\ndescription: знания об инцидентах с журналами\n")

	e.must("!!registry", "keygen", filepath.Join(e.work, "rel"))
	pub, _ := os.ReadFile(filepath.Join(e.work, "rel.pub"))
	key := filepath.Join(e.work, "rel.key")
	e.write(filepath.Join(pkg, fp.KnowledgeName), strings.Replace(knowledge, "[1.0, 5.8]\n    rules", "[7.0, 5.8]\n    rules", 1))
	e.fails("успехов от 0 до числа исходов", "!!registry", "publish", pkg, "--registry", reg, "--key", key)
	e.write(filepath.Join(pkg, fp.KnowledgeName), knowledge)
	e.must("!!registry", "publish", pkg, "--registry", reg, "--key", key)

	viper.Set("registry", map[string]any{
		"default": "corp",
		"sources": map[string]any{"corp": map[string]any{"url": "file://" + reg, "signed_by": "rel"}},
		"keys":    map[string]any{"rel": strings.TrimSpace(string(pub))},
	})
	t.Cleanup(func() { viper.Set("registry", nil) })
	id := regexpID(t, e.must("!!rin", "@pg_incidents"))
	e.must("approve", id)
	out := e.must("apply", "--request", id)
	for _, want := range []string{"Установлен @pg_incidents v1.0 из corp: sha256",
		"Пакет знаний pg_incidents v1.0 установлен: обобщений 1 — своя модель берёт их для задачи с import @pg_incidents v1.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("apply: нет %q:\n%s", want, out)
		}
	}
	// Пакет без алиасов тоже числится установленным: !!rin не ставит его
	// заново.
	if out := e.must("!!rin", "@pg_incidents"); !strings.Contains(out, "уже установлен — пропускается") ||
		!strings.Contains(out, "Ставить нечего: всё уже установлено") {
		t.Fatalf("повторная установка:\n%s", out)
	}

	// Задача подключает знания import'ом; версия — часть знания.
	task := "goal: available\n\nclasses:\n  Log : \"log\"\n  Storage : \"disk\"\n  State : \"available\"\n\n" +
		"relations:\n  Log --modifies--> Storage (log_size)\n  Storage --strives_to--> State\n\n" +
		"import @pg_incidents v1\n\ncell c1:\n  set Log.size = 900 MB\n"
	e.write(e.path("task.fp"), task)
	e.must("fp", "check", e.path("task.fp"))
	e.write(e.path("v2.fp"), strings.Replace(task, "v1", "v2", 1))
	if out, err := e.ochan("fp", "check", e.path("v2.fp")); err == nil ||
		!strings.Contains(out, "промпт требует @pg_incidents v2, а в библиотеке версия 1.0") {
		t.Fatalf("другая версия знаний: %v\n%s", err, out)
	}

	// Своя модель должна видеть знания, которые задача подключает.
	prog, _ := fp.Parse("task.fp", task)
	answer := fp.PlanAnswer(task, prog).Text()
	model := &archModel{answer: embedded.Answer{Text: answer, Forced: 9, Packages: []embedded.ModelPackage{{
		Statement: "set Storage.size = 900 MB", Rules: []string{"fp.modifies"}, L: 0.66, U: 1, Verdict: "well_true"}}}}
	blind := httptest.NewServer(embedded.Handler(embedded.Info{Engine: "own", Model: "own-small"}, model))
	defer blind.Close()
	sees := httptest.NewServer(embedded.Handler(embedded.Info{Engine: "own", Model: "own-small",
		Knowledge: []string{"pg_incidents 1.0"}}, model))
	defer sees.Close()
	viper.Set("llm.mode", "embedded")
	t.Cleanup(func() { viper.Set("llm.mode", ""); viper.Set("llm.endpoint", "http://127.0.0.1:8080") })

	viper.Set("llm.endpoint", blind.URL)
	if out := e.must("llm"); !strings.Contains(out, "Знания:  пакетов знаний модель не видит (каталог модулей ochan:") {
		t.Fatalf("llm без знаний:\n%s", out)
	}
	if _, err := e.ochan("llm", "generate", "--prompt", e.path("task.fp")); err == nil ||
		!strings.Contains(err.Error(), "задача подключает знания @pg_incidents 1.0, а модель их не видит — генерации нет") {
		t.Fatalf("модель без знаний: %v", err)
	}

	viper.Set("llm.endpoint", sees.URL)
	if out := e.must("llm"); !strings.Contains(out, "Знания:  @pg_incidents 1.0 — пакеты знаний, которые видит модель") {
		t.Fatalf("llm со знаниями:\n%s", out)
	}
	if out := e.must("llm", "generate", "--prompt", e.path("task.fp")); !strings.Contains(out, "set Storage.size = 900 MB") {
		t.Fatalf("generate со знаниями:\n%s", out)
	}
}
