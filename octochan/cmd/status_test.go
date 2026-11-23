package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ochan/core/config"
	"ochan/core/scenario"
)

// fakeExecutor — исполнитель с видом kind; statuses — что он ответит.
type fakeExecutor struct {
	kind     string
	statuses map[string]*scenario.Status
}

func (f fakeExecutor) Kind() string { return f.kind }
func (f fakeExecutor) Plan(context.Context, *scenario.Scenario) (*config.Plan, error) {
	return nil, errors.New("не нужен")
}
func (f fakeExecutor) Apply(context.Context, *scenario.Scenario, *config.Plan) (*scenario.Result, error) {
	return nil, errors.New("не нужен")
}

func (f fakeExecutor) Status(_ context.Context, ref string) (*scenario.Status, error) {
	if status, ok := f.statuses[ref]; ok {
		return status, nil
	}
	return nil, errors.New("нет такой ссылки")
}

// silentExecutor статуса показывать не умеет.
type silentExecutor struct{ fakeExecutor }

func (silentExecutor) Status() {}

// Команда status — общая: исполнитель выбирается флагом, а единственный —
// и без флага.
func TestStatusChoosesExecutorByFlag(t *testing.T) {
	e := newScriptEnv(t)

	e.fails("ни один подключённый исполнитель", "status", "x")

	scenario.Register("test-status-ssh", fakeExecutor{kind: "SSH", statuses: map[string]*scenario.Status{
		"t1": {Ref: "t1", State: "done", Fields: []scenario.StatusField{{Name: "Прогресс", Value: "100%"}},
			Events: []scenario.StatusEvent{{Time: "12:00", Level: "info", Message: "перезапущен"}}},
	}})
	addExecutorFlags()
	out := e.must("status", "t1")
	for _, want := range []string{"Статус t1 (ssh)", "Состояние: done", "Прогресс: 100%", "[12:00] info - перезапущен"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}

	scenario.Register("test-status-linux", fakeExecutor{kind: "Linux", statuses: map[string]*scenario.Status{
		"job-17": {Ref: "job-17", State: "running"},
	}})
	scenario.Register("test-status-mute", silentExecutor{fakeExecutor{kind: "Mute"}})
	addExecutorFlags()

	e.fails("укажите --executor или --linux", "status", "t1")
	if out := e.must("status", "--ssh", "t1"); !strings.Contains(out, "(ssh)") {
		t.Fatalf("--ssh:\n%s", out)
	}
	if out := e.must("status", "--executor", "linux", "job-17"); !strings.Contains(out, "Состояние: running") {
		t.Fatalf("--executor linux:\n%s", out)
	}
	if out := e.must("status", "--linux", "job-17"); !strings.Contains(out, "(linux)") {
		t.Fatalf("--linux:\n%s", out)
	}
	e.fails("указан дважды", "status", "--ssh", "--executor", "linux", "t1")
	e.fails("не подключён или не умеет показывать статус", "status", "--executor", "mute", "t1")
	e.fails("статус не получен полностью: nope", "status", "--ssh", "t1", "nope")
	if statusCmd.Flags().Lookup("mute") != nil {
		t.Fatal("флаг есть у исполнителя, который статуса не показывает")
	}
}
