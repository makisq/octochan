package cmd

import (
	"strings"
	"testing"

	"ochan/scenario"
)

// Сценарий вызывает сценарий: apply @ (…) и run внутри сценария. План
// вложенного — в плане внешнего, общая строка — «из ответа на …»; заявку
// согласуют целиком, правка вложенного плейбука после согласования её
// останавливает.
func TestNestedScenarios(t *testing.T) {
	e := newScriptEnv(t)
	node := &nodeExecutor{}
	scenario.Register("at-login", node)
	scenario.Register("at-tune", node)
	t.Cleanup(func() {
		scenario.Unregister("at-login")
		scenario.Unregister("at-tune")
	})
	e.write(e.path("login.yaml"), "service: at-login\ntargets:\n  - ip: 10.0.0.9\nparameters:\n  user: ops\n")
	tune := "service: at-tune\ntargets:\n  - ip: 10.0.0.9\nparameters:\n  work_mem: 32MB\n  string_i: ?\n"
	e.write(e.path("tune.yaml"), tune)
	e.write(e.path("node.ochan"), `apply @ ("tune.yaml" | "login.yaml")`+"\n")

	plan := e.must("plan", "node.ochan")
	for _, want := range []string{"ochan apply tune.yaml — вложенный сценарий", "│ План: tune.yaml (at-tune, исполнитель Node)",
		"│ внимание: общая строка string_i ← из ответа на login.yaml: значение появится при выполнении",
		"│ План: login.yaml (at-login, исполнитель Node)", "POST at-tune"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("в плане нет %q:\n%s", want, plan)
		}
	}
	if len(node.tuned) != 0 {
		t.Fatal("план выполнил вложенный сценарий")
	}

	created := e.must("request", "node.ochan")
	id := requestIDPattern.FindStringSubmatch(created)[1]
	if card := e.must("requests", id); !strings.Contains(card, "│ План: tune.yaml") {
		t.Fatalf("в карточке заявки нет вложенного плана:\n%s", card)
	}
	e.must("approve", id)

	// Плейбук поправили после согласования: выполнилось бы не то.
	e.write(e.path("tune.yaml"), strings.Replace(tune, "32MB", "64MB", 1))
	e.fails("вложенный сценарий ochan apply tune.yaml: его файл, аргументы или план не те", "apply", "--request", id)
	if len(node.tuned) != 0 {
		t.Fatal("отказ всё-таки выполнил вложенный сценарий")
	}
	e.write(e.path("tune.yaml"), tune)

	out := e.must("apply", "--request", id)
	if len(node.tuned) != 1 || strings.Contains(out, "внимание: выполнение разошлось") {
		t.Fatalf("заявка: настроек %d\n%s", len(node.tuned), out)
	}
	if !strings.Contains(e.read("tune.at.yaml"), `string_i: "k-42-secret"`) {
		t.Fatalf("копия плейбука с общей строкой:\n%s", e.read("tune.at.yaml"))
	}

	// run сценария из сценария: его запись — во вложенном плане.
	e.write(e.path("inner.ochan"), `write("inner.txt", "из вложенного")`+"\n")
	e.write(e.path("outer.ochan"), `ochan("run inner.ochan")`+"\n")
	plan = e.must("plan", "outer.ochan")
	if !strings.Contains(plan, "ochan run inner.ochan — вложенный сценарий") || !strings.Contains(plan, "│ План: inner.ochan") ||
		!strings.Contains(plan, "│     записать   файл — новый файл") {
		t.Fatalf("план вложенного run:\n%s", plan)
	}
	created = e.must("request", "outer.ochan")
	id = requestIDPattern.FindStringSubmatch(created)[1]
	e.must("approve", id)
	e.write(e.path("inner.ochan"), `write("inner.txt", "другое")`+"\n")
	e.fails("план сценария изменился после согласования", "apply", "--request", id)
	e.write(e.path("inner.ochan"), `write("inner.txt", "из вложенного")`+"\n")
	e.must("apply", "--request", id)
	if got := e.read("inner.txt"); got != "из вложенного" {
		t.Fatalf("inner.txt: %q", got)
	}

	// Цикл и чужая заявка — отказ.
	e.write(e.path("a.ochan"), `ochan("run b.ochan")`+"\n")
	e.write(e.path("b.ochan"), `ochan("run a.ochan")`+"\n")
	e.fails("сценарий запускает сам себя: a.ochan → b.ochan → a.ochan", "plan", "a.ochan")
	e.fails("сценарий запускает сам себя", "run", "a.ochan")
	e.write(e.path("other.ochan"), `ochan("apply --request 0f88eebd40")`+"\n")
	e.fails("из сценария не выполняется", "run", "other.ochan")
	e.write(e.path("box.ochan"), `ochan("run --sandbox inner.ochan")`+"\n")
	e.fails("из сценария не выполняется", "plan", "box.ochan")

	// plan из сценария только читает: выполняется и в плане.
	e.write(e.path("peek.ochan"), `print(ochan("plan login.yaml"))`+"\n")
	if out := e.must("plan", "peek.ochan"); !strings.Contains(out, "POST at-login") {
		t.Fatalf("plan из сценария:\n%s", out)
	}
}
