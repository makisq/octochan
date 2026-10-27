package rlm

import (
	"context"
	"strings"
	"testing"

	"octochan/config"
	"octochan/scenario"
)

// recordingAPI запоминает отправленные задачи.
type recordingAPI struct{ posted int }

func (a *recordingAPI) TasksURL() string                       { return "https://rlm/tasks" }
func (a *recordingAPI) Get(context.Context, string, any) error { return nil }
func (a *recordingAPI) IsDryRun() bool                         { return false }
func (a *recordingAPI) WaitForTask(context.Context, string, WaitOptions) (map[string]any, error) {
	return nil, nil
}
func (a *recordingAPI) Post(_ context.Context, _ string, _ any, out any) error {
	a.posted++
	*(out.(*map[string]any)) = map[string]any{"id": "task-1"}
	return nil
}

// planTestModule — модуль сервиса с задачей на каждую цель.
type planTestModule struct{ data *scenario.Scenario }

func (m planTestModule) Validate() error { return nil }
func (m planTestModule) GenerateRequests(context.Context, ScenarioAPI) ([]*APIRequest, error) {
	var out []*APIRequest
	for _, ci := range m.data.AllCIs() {
		out = append(out, &APIRequest{Body: map[string]any{
			"items":  []map[string]string{{"invsvm_ci_svm": ci}},
			"params": map[string]any{"limit": m.data.Parameters["limit"]},
		}})
	}
	return out, nil
}

// RLM выполняет ровно согласованный план: запросы, разошедшиеся с ним,
// не отправляются вовсе.
func TestExecuteScenarioIsBoundToPlan(t *testing.T) {
	RegisterScenarioModule("test_plan_bound", func(d *scenario.Scenario) (ScenarioModule, error) {
		return planTestModule{d}, nil
	})
	s := &scenario.Scenario{Service: "test_plan_bound", Parameters: map[string]any{"limit": 200},
		Targets: []scenario.Target{{SVMCI: "CI1", CIList: []string{"CI1"}}}}

	approved, err := PlanScenario(context.Background(), DryRun(&recordingAPI{}), s)
	if err != nil {
		t.Fatal(err)
	}
	api := &recordingAPI{}
	if ids, err := ExecuteScenario(context.Background(), api, s, approved); err != nil || len(ids) != 1 || api.posted != 1 {
		t.Fatalf("согласованный план: %v, %v, отправлено %d", ids, err, api.posted)
	}

	// Параметр поменялся после согласования — ничего не отправлено.
	changed := s.WithOverrides(map[string]string{"limit": "500"})
	api = &recordingAPI{}
	_, err = ExecuteScenario(context.Background(), api, changed, approved)
	if err == nil || !strings.Contains(err.Error(), "разошлись с согласованным планом") || api.posted != 0 {
		t.Fatalf("расхождение: %v, отправлено %d", err, api.posted)
	}
	if _, err := ExecuteScenario(context.Background(), api, s, nil); err == nil {
		t.Fatal("выполнение без плана")
	}

	// Неполный план (модуль ходил в API при подготовке) сверяется по целям.
	incomplete := &config.Plan{Changes: []config.Change{{Kind: config.ChangeTask, Target: "CI1",
		Name: "test_plan_bound", Detail: incompleteDetail}}}
	api = &recordingAPI{}
	if _, err := ExecuteScenario(context.Background(), api, changed, incomplete); err != nil || api.posted != 1 {
		t.Fatalf("неполный план по целям: %v, отправлено %d", err, api.posted)
	}
}
