package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"octochan/lang"
	"octochan/pluginhost"
	"octochan/scenario"

	"github.com/spf13/viper"
)

// Пакет алиасов ставится той же командой, что и модуль: проверяется до
// копирования, сразу доступен сценариям, удаляется по имени.
func TestInstallAliasPackage(t *testing.T) {
	e := newScriptEnv(t)
	src := filepath.Join(t.TempDir(), "pgtune")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(src, "lib.ochan"), "alias raise(c: config) {\n    put(c, \"max_connections\", \"300\")\n    return c\n}\n")
	e.write(filepath.Join(src, lang.ManifestName), "version: 1.0.0\nprior.raise: 8/8\n")
	e.write(filepath.Join(src, "notes.txt"), "не часть пакета")

	out := e.must("!!install", src)
	if !strings.Contains(out, "Пакет алиасов pgtune v1.0.0 установлен: алиасов 1") {
		t.Fatalf("установка пакета:\n%s", out)
	}
	installed := filepath.Join(e.home, ".octochan", "modules", "pgtune")
	if _, err := os.Stat(filepath.Join(installed, "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("в пакет скопирован посторонний файл")
	}
	e.write(e.path("use.ochan"), "c = pgtune.raise(config(\"prod.conf\"))\nsave(c, \"prod.conf\")\n")
	if out := e.must("plan", "use.ochan"); !strings.Contains(out, "max_connections: 100 -> 300") {
		t.Fatalf("алиас установленного пакета не работает:\n%s", out)
	}
	if out := e.must("modules"); !strings.Contains(out, "pgtune") || !strings.Contains(out, "v1.0.0 — алиасов: 1") {
		t.Fatalf("пакета нет в списке:\n%s", out)
	}

	// Сломанный пакет не ставится и установленный не портит.
	e.write(filepath.Join(src, "lib.ochan"), "alias raise(c: config) {\n")
	e.fails("пакет pgtune не установлен", "!!install", src)
	if data, _ := os.ReadFile(filepath.Join(installed, "lib.ochan")); !strings.Contains(string(data), "300") {
		t.Fatal("сломанный пакет перезаписал установленный")
	}

	bad := filepath.Join(t.TempDir(), "my-pkg")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(bad, "a.ochan"), "alias f() { return 1 }\n")
	e.fails("допустимы буквы, цифры и «_»", "!!install", bad)
	e.fails("нет ни алиасов", "!!install", t.TempDir())
	e.fails("ставятся модули", "!!install", e.path("prod.conf"))

	e.must("!!install", "--remove", "pgtune")
	e.fails("pgtune не установлен", "!!install", "--remove", "pgtune")
	e.fails("укажите имя модуля или пакета", "!!install", "--remove", "../x")
	e.fails(`пакет алиасов "pgtune" не подключён`, "plan", "use.ochan")
}

// Модуль-исполнитель ставится !!install и сразу выполняет сценарии своего
// сервиса; токен ему выдаёт auth --executor.
func TestInstallRunnerModule(t *testing.T) {
	module := buildModule(t, "octochan/examples/runnermodule", "demo-runner")
	e := newScriptEnv(t)
	t.Cleanup(func() {
		pluginhost.GetModuleManager().Cleanup()
		syncRunners()
	})

	out := e.must("!!install", module)
	for _, want := range []string{"Модуль demo-runner.hcplugin установлен", "исполнитель Demo: сервисы demo",
		"ochan auth --executor demo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	if out := e.must("modules"); !strings.Contains(out, "исполнитель Demo (demo)") {
		t.Fatalf("исполнителя нет в списке модулей:\n%s", out)
	}

	e.write(e.path("demo.yaml"), "service: demo\nparameters:\n  max_connections: 200\ntargets:\n  - svm_ci: CI001\n")
	if out := e.must("plan", "demo.yaml"); !strings.Contains(out, "max_connections") || !strings.Contains(out, "исполнитель Demo") {
		t.Fatalf("план через исполнителя модуля:\n%s", out)
	}

	e.must("auth", "--executor", "demo", "секрет")
	data, err := os.ReadFile(filepath.Join(e.home, ".octochan", "tokens", "demo"))
	if err != nil || strings.TrimSpace(string(data)) != "секрет" {
		t.Fatalf("токен исполнителя: %q, %v", data, err)
	}
	if out := e.must("status", "--executor", "demo", "demo-CI001"); !strings.Contains(out, "Токен") || !strings.Contains(out, "есть") {
		t.Fatalf("токен не дошёл до исполнителя:\n%s", out)
	}
	e.fails("допустимы буквы, цифры", "auth", "--executor", "../x", "t")

	e.must("!!install", "--remove", "demo-runner"+pluginhost.ModuleExtension)
	if _, err := scenario.Resolve("demo"); err == nil {
		t.Fatal("сервис удалённого модуля остался")
	}
}

// buildModule собирает модуль ochan из пакета. Собирать надо до
// newScriptEnv: он уводит текущий каталог из модуля octochan.
func buildModule(t *testing.T, pkg, name string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("собирает модуль")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("нет компилятора go для сборки модуля")
	}
	module := filepath.Join(t.TempDir(), name+pluginhost.ModuleExtension)
	if out, err := exec.Command("go", "build", "-o", module, pkg).CombinedOutput(); err != nil {
		t.Fatalf("сборка модуля %s: %v\n%s", pkg, err, out)
	}
	return module
}

// RLM — модуль. Без него сервисы RLM называют, что поставить; с ним plan,
// apply и status идут в RLM API с токеном из auth и адресом из конфига
// ochan — прежним defaults.api_url или своим executors.rlm.api_url.
func TestRLMModule(t *testing.T) {
	module := buildModule(t, "octochan/plugins/rlm", "rlm")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token секрет" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"detail": "нет токена"}`)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks.json":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": 101}`)
		case r.URL.Path == "/api/tasks/101/":
			fmt.Fprint(w, `{"status": "success", "created_at": "2026-10-27"}`)
		case r.URL.Path == "/api/tasks/101/events/":
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	e := newScriptEnv(t)
	t.Cleanup(func() {
		pluginhost.GetModuleManager().Cleanup()
		syncRunners()
	})
	e.write(e.path("restart.yaml"), "service: pangolin_restart\nparameters:\n  restart: instance\n"+
		"  skip_sm_conflicts: false\n  confirm: true\ntargets:\n  - svm_ci: CI00000001\n")

	e.fails("теперь приносит модуль rlm", "plan", "restart.yaml")
	e.fails("исполнителей нет", "auth", "секрет")

	e.must("!!install", module)
	setting(t, "defaults.api_url", api.URL+"/api/tasks.json")
	if out := e.must("auth", "секрет"); !strings.Contains(out, "Токен исполнителя rlm сохранён") {
		t.Fatalf("auth без --executor при одном исполнителе:\n%s", out)
	}
	if out := e.must("plan", "restart.yaml"); !strings.Contains(out, "pangolin_restart") || !strings.Contains(out, "исполнитель RLM") {
		t.Fatalf("план через модуль RLM:\n%s", out)
	}
	if out := e.must("apply", "restart.yaml"); !strings.Contains(out, "Создано:\n  - 101") {
		t.Fatalf("выполнение через модуль RLM:\n%s", out)
	}
	if out := e.must("status", "--rlm", "101"); !strings.Contains(out, "Состояние: success") {
		t.Fatalf("статус через модуль RLM:\n%s", out)
	}

	// Свой адрес исполнителя перекрывает общий.
	setting(t, "executors.rlm.api_url", api.URL+"/elsewhere/tasks.json")
	e.fails("код 404", "status", "--rlm", "101")
}

// setting задаёт параметр конфига на время теста.
func setting(t *testing.T, key string, value any) {
	t.Helper()
	old := viper.Get(key)
	viper.Set(key, value)
	t.Cleanup(func() { viper.Set(key, old) })
}
