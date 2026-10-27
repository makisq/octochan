package config

import (
	"strings"
	"testing"
)

func TestPlanFromDiffIgnoresRemovals(t *testing.T) {
	// patch переносит значения, а не удаляет их: параметр, которого нет
	// в источнике изменений, в план попадать не должен.
	base := ConfigMap{"": {"max_connections": "100", "только_в_базе": "1"}}
	changes := ConfigMap{"": {"max_connections": "200", "новый": "да"}}

	plan := PlanFromDiff(CompareConfigs(base, changes, "база", "изменения"), "out.conf")

	byName := make(map[string]Change)
	for _, change := range plan.Changes {
		byName[change.Name] = change
	}

	if _, ok := byName["только_в_базе"]; ok {
		t.Error("параметр, отсутствующий в изменениях, попал в план")
	}
	if got := byName["max_connections"]; got.Kind != ChangeSet || got.From != "100" || got.To != "200" {
		t.Errorf("max_connections: %+v", got)
	}
	if got := byName["новый"]; got.Kind != ChangeAdd || got.To != "да" {
		t.Errorf("новый: %+v", got)
	}
}

func TestPlanIgnoresEquivalentValues(t *testing.T) {
	// Значения, одинаковые по смыслу, изменением не являются.
	base := ConfigMap{"": {"shared_buffers": "8GB"}}
	changes := ConfigMap{"": {"shared_buffers": "8192MB"}}

	plan := PlanFromDiff(CompareConfigs(base, changes, "a", "b"), "out.conf")
	if plan.Len() != 0 {
		t.Errorf("8GB и 8192MB попали в план: %+v", plan.Changes)
	}
}

func TestApplyPlanRoundTrip(t *testing.T) {
	base := ConfigMap{"": {"max_connections": "100"}, "server": {"port": "5432"}}
	changes := ConfigMap{"": {"max_connections": "200"}, "server": {"port": "5433"}}

	plan := PlanFromDiff(CompareConfigs(base, changes, "a", "b"), "out.conf")
	result := ApplyPlan(base, plan)

	if got := result[""]["max_connections"]; got != "200" {
		t.Errorf("max_connections = %q", got)
	}
	if got := result["server"]["port"]; got != "5433" {
		t.Errorf("server.port = %q", got)
	}
}

func TestPlanSortIsStable(t *testing.T) {
	plan := &Plan{Changes: []Change{
		{Target: "srv2", Name: "b"},
		{Target: "srv1", Name: "z"},
		{Target: "srv1", Name: "a"},
	}}
	plan.Sort()

	want := []string{"srv1/a", "srv1/z", "srv2/b"}
	for i, change := range plan.Changes {
		if got := change.Target + "/" + change.Name; got != want[i] {
			t.Errorf("позиция %d: %s, ожидалось %s", i, got, want[i])
		}
	}
}

func TestDiffPlans(t *testing.T) {
	approved := &Plan{Changes: []Change{
		{Kind: ChangeTask, Target: "CI1", Name: "tune", Detail: "max_connections=200"},
		{Kind: ChangeTask, Target: "CI2", Name: "tune", Detail: "max_connections=200"},
	}}
	same := &Plan{Command: "другое", Warnings: []string{"не важно"}, Changes: []Change{approved.Changes[1], approved.Changes[0]}}
	if diffs := DiffPlans(approved, same); len(diffs) != 0 {
		t.Fatalf("одинаковые изменения в другом порядке: %v", diffs)
	}
	changed := &Plan{Destructive: true, Changes: []Change{approved.Changes[0],
		{Kind: ChangeTask, Target: "CI3", Name: "tune", Detail: "max_connections=200"}}}
	got := strings.Join(DiffPlans(approved, changed), "\n")
	for _, want := range []string{"пропало: задача CI2: tune (max_connections=200)",
		"появилось: задача CI3: tune", "прерывает обслуживание: было false, стало true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("нет %q в\n%s", want, got)
		}
	}
	// Одно и то же изменение дважды — это два изменения.
	twice := &Plan{Changes: []Change{approved.Changes[0], approved.Changes[0], approved.Changes[1]}}
	if diffs := DiffPlans(approved, twice); len(diffs) != 1 || !strings.HasPrefix(diffs[0], "появилось") {
		t.Fatalf("повтор: %v", diffs)
	}
}
