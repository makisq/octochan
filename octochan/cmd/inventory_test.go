package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/scenario"
)

const testInventory = `groups:
  db: {max_connections: 200}
hosts:
  db1: {groups: [db], svm_ci: CI001}
  db2: {groups: [db, db_new], svm_ci: CI002, max_connections: 300}
  web1: {group: web, svm_ci: CI100}
`

func (e *scriptEnv) inventory(text string) {
	e.t.Helper()
	e.write(filepath.Join(e.home, ".octochan", "inventory.yaml"), text)
}

// Цели из инвентаря и --limit: сценарий исполнителя называет группу, а
// не перечисляет серверы; заявка помнит --limit, и поменявшийся инвентарь
// ловит сверка плана.
func TestInventoryTargetsAndLimit(t *testing.T) {
	e := newScriptEnv(t)
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-inv", ext)
	t.Cleanup(func() { scenario.Unregister("test-inv") })
	e.write(e.path("tune.yaml"), "service: test-inv\nparameters: {limit: 200}\ntargets: [{group: db}]\n")

	e.fails("инвентаря нет", "plan", "tune.yaml")
	e.inventory(testInventory)

	out := e.must("plan", "tune.yaml")
	if !strings.Contains(out, "CI001") || !strings.Contains(out, "CI002") || strings.Contains(out, "CI100") {
		t.Fatalf("group=db:\n%s", out)
	}
	out = e.must("plan", "tune.yaml", "--limit", "group=db_new")
	if strings.Contains(out, "CI001") || !strings.Contains(out, "CI002") {
		t.Fatalf("--limit group=db_new:\n%s", out)
	}
	e.fails("не оставил ни одной цели", "plan", "tune.yaml", "--limit", "group=web")
	e.fails("у правки конфига целей нет", "plan", "prod.conf", "prod.conf", "--limit", "group=db")

	id := requestIDPattern.FindStringSubmatch(e.must("request", "tune.yaml", "--limit", "group=db_new"))[1]
	if card := e.must("requests", id); !strings.Contains(card, "Цели:      --limit group=db_new") {
		t.Fatalf("карточка без --limit:\n%s", card)
	}
	e.must("approve", id)
	// В группу добавили сервер — план заявки уже не тот.
	e.inventory(testInventory + "  db3: {groups: [db, db_new], svm_ci: CI003}\n")
	e.fails("план изменился после согласования", "apply", "--request", id)
	e.inventory(testInventory)
	e.must("apply", "--request", id)
	if len(ext.applied) != 1 || ext.applied[0].Len() != 1 || ext.applied[0].Changes[0].Target != "CI002" {
		t.Fatalf("выполнено: %+v", ext.applied)
	}
}

// Сценарий на языке алиасов берёт серверы и их переменные из инвентаря,
// а не из словаря в своём тексте.
func TestScriptInventory(t *testing.T) {
	e := newScriptEnv(t)
	e.inventory(testInventory)
	e.write(e.path("rollout.ochan"), `for сервер in inventory("group=db") {
    c = config("prod.conf")
    put(c, "max_connections", str(сервер.max_connections))
    save(c, "out-" + сервер.host + ".conf")
}
print(len(inventory("group=web")))
`)
	out := e.must("plan", "rollout.ochan")
	for _, want := range []string{"out-db1.conf", "max_connections = 200", "out-db2.conf", "max_connections = 300"} {
		if !strings.Contains(out, want) {
			t.Fatalf("план без %q:\n%s", want, out)
		}
	}
	out = e.must("plan", "rollout.ochan", "--limit", "group=db_new")
	if strings.Contains(out, "out-db1.conf") || !strings.Contains(out, "out-db2.conf") {
		t.Fatalf("--limit:\n%s", out)
	}

	// В песочнице — тот же инвентарь: его читает родитель, ребёнку
	// рабочий каталог ochan не виден.
	out = e.must("plan", "rollout.ochan", "--sandbox", "--limit", "group=db_new")
	if strings.Contains(out, "out-db1.conf") || !strings.Contains(out, "out-db2.conf") {
		t.Fatalf("--sandbox --limit:\n%s", out)
	}

	// Заявка помнит --limit и выполняется с ним.
	id := requestIDPattern.FindStringSubmatch(e.must("request", "rollout.ochan", "--limit", "group=db_new"))[1]
	e.must("approve", id)
	e.must("apply", "--request", id)
	if got := e.read("out-db2.conf"); !strings.Contains(got, "max_connections = 300") {
		t.Fatalf("out-db2.conf:\n%s", got)
	}

	if err := os.Remove(filepath.Join(e.home, ".octochan", "inventory.yaml")); err != nil {
		t.Fatal(err)
	}
	e.write(e.path("no.ochan"), "print(inventory())\n")
	e.fails("инвентаря нет", "run", "no.ochan")
}

// Новый сервер — тот, которого ещё не видели; отметить его увиденным —
// решение, а не чтение.
func TestInventoryDiscovery(t *testing.T) {
	e := newScriptEnv(t)
	e.inventory(testInventory)
	out := e.must("inventory")
	if !strings.Contains(out, "+ db1") || !strings.Contains(out, "Хостов: 3, новых: 3 (+)") {
		t.Fatalf("inventory:\n%s", out)
	}
	if out := e.must("inventory", "ack", "group=db"); !strings.Contains(out, "Отмечены увиденными: 2") {
		t.Fatalf("ack:\n%s", out)
	}
	if out := e.must("inventory", "--new"); !strings.Contains(out, "+ web1") || strings.Contains(out, "db1") {
		t.Fatalf("--new:\n%s", out)
	}
	t.Setenv(EnvRole, "llm")
	e.must("inventory")
	e.fails("inventory ack", "inventory", "ack")
}
