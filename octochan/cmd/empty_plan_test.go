package cmd

import (
	"strings"
	"testing"

	"ochan/core/config"
	"ochan/core/workspace"
)

// Заявку, которая ничего не изменит, ochan не оформляет и не согласует.
// Так было на демо-стенде: модель написала work_mem = "32MB" —
// присваивание переменной, — заявка вышла с пустым планом, и её
// согласовали.
func TestEmptyPlanRequest(t *testing.T) {
	e := newScriptEnv(t)

	// Сценарий без записи — отказ с подсказкой, как править конфиг.
	e.write(e.path("noop.ochan"), "work_mem = \"32MB\"\n")
	e.fails("план пуст: сценарий ничего не меняет", "request", "noop.ochan")
	if _, err := e.ochan("request", "noop.ochan"); err == nil || !strings.Contains(err.Error(), "save(c, файл)") {
		t.Fatalf("в отказе нет подсказки, как править конфиг: %v", err)
	}

	// Правка конфига, в которой нечего менять, — тоже отказ.
	e.write(e.path("same.conf"), e.read("prod.conf"))
	e.fails("план пуст: в конфиге с новыми значениями нет отличий", "request", "prod.conf", "same.conf")

	store, err := requestStore()
	if err != nil {
		t.Fatal(err)
	}
	all, _ := store.List()
	if len(all) != 0 {
		t.Fatalf("пустые заявки сохранены: %d", len(all))
	}

	// Заявка с пустым планом, оформленная раньше, — не согласуется.
	old := &workspace.Request{Source: "noop.ochan", Summary: "сценарий noop.ochan, изменений: 0",
		Plan: &config.Plan{Command: "plan"}, Script: &workspace.ScriptRun{Dir: e.work}}
	stampRequest(old)
	if err := store.Create(old); err != nil {
		t.Fatal(err)
	}
	e.fails("план пуст — выполнение ничего не изменит", "approve", old.ID)

	// Обычная заявка с изменением оформляется как прежде.
	if created, err := e.ochan("request", "tune.ochan"); err != nil || !strings.Contains(created, "создана (ждёт решения)") {
		t.Fatalf("заявка с изменением: %v\n%s", err, created)
	}
}
