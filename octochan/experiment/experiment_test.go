package main

import (
	"reflect"
	"strings"
	"testing"

	"ochan/fp"
)

// Задуманный план, открытые вопросы и веса задач, подготовленных прежним
// стендом, выводит та же логика ochan: задачи не надо готовить заново.
func TestTasksFollowFromLogic(t *testing.T) {
	tasks, err := loadTasks("")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("задачи: %d, %v", len(tasks), err)
	}
	for _, task := range tasks {
		text := task.Formal[0]
		prog, diags := fp.Parse(task.ID, text)
		if len(diags) > 0 {
			t.Fatalf("%s: %v", task.ID, diags)
		}
		plan := fp.PlanAnswer(text, prog)
		intended := map[string]string{}
		for _, d := range plan.Decisions {
			intended[d.Attr] = d.Value
		}
		if len(intended) != len(task.Intended) || len(intended) > 0 && !reflect.DeepEqual(intended, task.Intended) {
			t.Fatalf("%s: задуманный план %v, в задаче %v", task.ID, intended, task.Intended)
		}
		if got := weightsOf(prog); len(got) != len(task.Weights) || len(got) > 0 && !reflect.DeepEqual(got, task.Weights) {
			t.Fatalf("%s: веса %v, в задаче %v", task.ID, got, task.Weights)
		}
	}
}

// План ответа — пары «вопрос → ответ» отчёта; решения — правки новых
// ячеек, не ячеек задачи.
func TestPlanOfAndDelta(t *testing.T) {
	task := "goal: probe\n\nclasses:\n  File : \"file\"\n\ncell c1:\n  set File.size = 8 GB\n"
	answer := task + "\ncell c2:\n  set Approval.size = 8 GB\n\nreport:\n  q: size\n    choice: 8 GB\n    basis: relation(x)\n"
	report, decisions := planOf(answer, task)
	if !reflect.DeepEqual(report, map[string]string{"size": "8 GB"}) || !reflect.DeepEqual(decisions, map[string]string{"Approval.size": "8 GB"}) {
		t.Fatalf("план %v, решения %v", report, decisions)
	}
	w := map[string]int{"a": 3}
	if d := delta(map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1", "b": "3"}, w); d != 2.0/5 {
		t.Fatalf("δ = %v", d)
	}
	if delta(nil, nil, w) != 0 {
		t.Fatal("δ(∅, ∅) ≠ 0")
	}
	if !strings.Contains(rewrite(task, fp.BaseLexicon(), "x/1"), "set File.size = 8 GB") {
		t.Fatal("перезапись потеряла правку")
	}
}
