package cmd

import (
	"fmt"
	"strings"
	"testing"

	"octochan/workspace"

	"github.com/spf13/viper"
)

// Обучение по инцидентам: одна структура на разных хостах созревает по
// порогу autovacuum, одиночный — шум, запись на естественном языке не
// участвует; выгрузка — созревшие кластеры.
func TestIncidentLearn(t *testing.T) {
	e := newScriptEnv(t)
	t.Cleanup(func() { viper.Set("incidents.learn.threshold", 5) })
	store, err := incidentStore()
	if err != nil {
		t.Fatal(err)
	}
	connections := func(host string, limit int) string {
		return "goal: stable\n\nclasses:\n  Connection : \"connection\"\n  Process : \"process\"\n  State : \"stable\"\n\n" +
			"relations:\n  Connection --limits--> Process (connection_limit)\n  Process --strives_to--> State\n\n" +
			fmt.Sprintf("cell %s:\n  set Connection.maximum = %d\n", host, limit)
	}
	for i := 0; i < 8; i++ {
		if err := store.Create(&workspace.Incident{Level: "low", Summary: connections(fmt.Sprintf("db%d", i), 100+i)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, summary := range []string{
		"goal: available\n\nclasses:\n  Log : \"log\"\n  Storage : \"disk\"\n  State : \"available\"\n\n" +
			"relations:\n  Log --modifies--> Storage (log_size)\n  Storage --strives_to--> State\n\ncell c1:\n  set Log.size = 100 MB\n",
		"PostgreSQL на db7 не принимает соединения",
	} {
		if err := store.Create(&workspace.Incident{Level: "low", Summary: summary}); err != nil {
			t.Fatal(err)
		}
	}

	// Порог: 5 + 0,2 × 9 = 6,8 — восемь похожих его перевешивают.
	out := e.must("incident", "learn", "--export", e.path("ripe.jsonl"))
	for _, want := range []string{"Инцидентов на ℒ: 9; на естественном языке: 1", "Порог: больше 5 + 0.2 × 9.0 = 6.8",
		"созрел   инцидентов 8, вес 8.0 — Connection limits Process", "Шум: одиночных инцидентов 1",
		"Созревших кластеров в " + e.path("ripe.jsonl") + ": 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}
	// В выгрузке — порог похожести, по которому модель узнает структуру,
	// и вес каждого инцидента: свежие — около 1.
	if exported := e.read("ripe.jsonl"); strings.Count(exported, `"id":`) != 8 || !strings.Contains(exported, "set Connection.maximum #") ||
		!strings.Contains(exported, `"similarity":0.8`) || !strings.Contains(exported, `"weight":0.99`) && !strings.Contains(exported, `"weight":1,`) {
		t.Fatalf("выгрузка:\n%s", exported)
	}

	viper.Set("incidents.learn.threshold", 10)
	if out := e.must("incident", "learn"); !strings.Contains(out, "копится  инцидентов 8") {
		t.Fatalf("порог выше — кластер копится:\n%s", out)
	}
}
