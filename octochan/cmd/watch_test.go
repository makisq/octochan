package cmd

import (
	"regexp"
	"strings"
	"testing"

	"octochan/scenario"
)

const watchPolicyFile = `name: new-servers
scenario: tune.yaml
targets: [group=db_new]
params:
  limit: 100..300
expires: 2099-01-01
`

// watch проверяет и планирует сам; выполняет сам только то, что в
// зарегистрированной политике, а всё остальное оформляет заявками.
func TestWatchPolicy(t *testing.T) {
	e := newScriptEnv(t)
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-watch", ext)
	t.Cleanup(func() { scenario.Unregister("test-watch") })
	e.write(e.path("tune.yaml"), "service: test-watch\nparameters: {limit: 200}\ntargets: [{host: \"*\"}]\n")
	e.write(e.path("new-servers.yaml"), watchPolicyFile)
	inv := "hosts:\n  db1: {groups: [db_new], svm_ci: CI001}\n  web1: {group: web, svm_ci: CI100}\n"
	e.inventory(inv)

	e.fails("укажите --policy", "watch", "--source", "local")
	e.fails("политики new-servers нет", "watch", "--source", "local", "--policy", "new-servers")

	out := e.must("policy", "add", e.path("new-servers.yaml"))
	for _, want := range []string{"Политика new-servers зарегистрирована", "Цели:      group=db_new",
		"Параметр:  limit: 100..300", "Отозвать: ochan policy revoke new-servers"} {
		if !strings.Contains(out, want) {
			t.Fatalf("policy add: нет %q:\n%s", want, out)
		}
	}
	e.fails("уже действует", "policy", "add", e.path("new-servers.yaml"))

	// db1 — в политике: согласована ею и выполнена; web1 — заявка.
	out = e.must("watch", "--source", "local", "--policy", e.path("new-servers.yaml"))
	if !strings.Contains(out, "хостов 2, новых 2") ||
		!regexp.MustCompile(`db1: заявка [0-9a-f]+ согласована политикой new-servers и выполнена`).MatchString(out) ||
		!strings.Contains(out, "web1: заявка") ||
		!strings.Contains(out, "ждёт решения — вне политики new-servers: хост не подходит под targets (group=db_new)") {
		t.Fatalf("первый проход:\n%s", out)
	}
	if len(ext.applied) != 1 || ext.applied[0].Changes[0].Target != "CI001" {
		t.Fatalf("выполнено: %+v", ext.applied)
	}
	if out := e.must("watch", "--source", "local", "--policy", "new-servers"); !strings.Contains(out, "новых 0") {
		t.Fatalf("повторный проход:\n%s", out)
	}
	show := e.must("policy", "show", "new-servers")
	if !strings.Contains(show, "Согласовано политикой:") || !strings.Contains(show, "новый хост db1") {
		t.Fatalf("policy show:\n%s", show)
	}

	// Значение параметра вне политики — заявка, а не изменение.
	e.write(e.path("tune.yaml"), "service: test-watch\nparameters: {limit: 900}\ntargets: [{host: \"*\"}]\n")
	e.inventory(inv + "  db2: {groups: [db_new], svm_ci: CI002}\n")
	out = e.must("watch", "--source", "local", "--policy", "new-servers")
	if !strings.Contains(out, "параметр limit = 900 вне допустимого (100..300)") || len(ext.applied) != 1 {
		t.Fatalf("параметр вне политики:\n%s", out)
	}

	// Отзыв действует со следующего прохода.
	e.write(e.path("tune.yaml"), "service: test-watch\nparameters: {limit: 200}\ntargets: [{host: \"*\"}]\n")
	e.must("policy", "revoke", "new-servers", "--reason", "проверка")
	e.inventory(inv + "  db2: {groups: [db_new], svm_ci: CI002}\n  db3: {groups: [db_new], svm_ci: CI003}\n")
	out = e.must("watch", "--source", "local", "--policy", "new-servers")
	if !strings.Contains(out, "Выполнение без человека выключено: политика new-servers отозвана") ||
		!strings.Contains(out, "db3: заявка") || len(ext.applied) != 1 {
		t.Fatalf("после отзыва:\n%s", out)
	}
	if list := e.must("policy"); !strings.Contains(list, "отозвана") || !strings.Contains(list, "проверка") {
		t.Fatalf("policy после отзыва:\n%s", list)
	}

	// Файл политики правили после регистрации — это не регистрация.
	e.write(e.path("new-servers.yaml"), strings.Replace(watchPolicyFile, "100..300", "100..900", 1))
	e.fails("отличается от зарегистрированной политики", "watch", "--source", "local", "--policy", e.path("new-servers.yaml"))
}
