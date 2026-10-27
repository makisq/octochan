package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"octochan/config"
	"octochan/scenario"
)

// extExecutor — исполнитель внешней системы: задача на цель с параметром
// limit. remote — состояние системы: план зависит от него, как план
// настоящего исполнителя зависит от ответа API.
type extExecutor struct {
	mu      sync.Mutex
	remote  string
	applied []*config.Plan
	undone  []*config.Plan
}

func (x *extExecutor) Kind() string { return "Ext" }

func (x *extExecutor) Plan(_ context.Context, s *scenario.Scenario) (*config.Plan, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	plan := &config.Plan{Command: "apply"}
	for _, ci := range s.AllCIs() {
		plan.Add(config.Change{Kind: config.ChangeTask, Target: ci,
			Name: fmt.Sprintf("limit = %v", s.Parameters["limit"]), Detail: "система " + x.remote})
	}
	return plan, nil
}

func (x *extExecutor) Apply(_ context.Context, _ *scenario.Scenario, plan *config.Plan) (*scenario.Result, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.applied = append(x.applied, plan)
	result := &scenario.Result{Summary: "готово"}
	for _, c := range plan.Changes {
		result.Refs = append(result.Refs, "job-"+c.Target)
	}
	return result, nil
}

func (x *extExecutor) PlanCompensation(_ context.Context, s *scenario.Scenario, applied *config.Plan, refs []string) (*config.Plan, error) {
	previous, ok := s.Parameters["previous"]
	if !ok {
		return nil, fmt.Errorf("задачи %s уже отработали, прежнее значение неизвестно", strings.Join(refs, ", "))
	}
	plan := &config.Plan{Command: "compensate"}
	for _, c := range applied.Changes {
		plan.Add(config.Change{Kind: config.ChangeTask, Target: c.Target, Name: fmt.Sprintf("limit = %v", previous)})
	}
	return plan, nil
}

func (x *extExecutor) Compensate(_ context.Context, _ *scenario.Scenario, plan *config.Plan, refs []string) (*scenario.Result, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.undone = append(x.undone, plan)
	return &scenario.Result{Refs: []string{"undo-" + strings.Join(refs, "+")}, Summary: "возвращено"}, nil
}

// plainExecutor — тот же исполнитель без обратного действия.
type plainExecutor struct{ x *extExecutor }

func (p plainExecutor) Kind() string { return "Plain" }
func (p plainExecutor) Plan(ctx context.Context, s *scenario.Scenario) (*config.Plan, error) {
	return p.x.Plan(ctx, s)
}
func (p plainExecutor) Apply(ctx context.Context, s *scenario.Scenario, plan *config.Plan) (*scenario.Result, error) {
	return p.x.Apply(ctx, s, plan)
}

// Заявка выполняется ровно по согласованному плану: с параметрами, с
// которыми его строили, и только если система с тех пор не изменилась.
func TestApplyRequestUsesApprovedPlan(t *testing.T) {
	e := newScriptEnv(t)
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-ext", ext)
	t.Cleanup(func() { scenario.Unregister("test-ext") })
	e.write(e.path("ext.yaml"), "service: test-ext\nparameters: {limit: 200}\ntargets: [{svm_ci: CI1}]\n")

	// Параметр командной строки — часть согласованного.
	id := requestIDPattern.FindStringSubmatch(e.must("request", "ext.yaml", "limit=300"))[1]
	if out := e.must("requests", id); !strings.Contains(out, "Параметр:  limit=300") {
		t.Fatalf("карточка без параметра:\n%s", out)
	}
	e.must("approve", id)
	e.must("apply", "--request", id)
	if len(ext.applied) != 1 || ext.applied[0].Changes[0].Name != "limit = 300" {
		t.Fatalf("исполнитель получил не согласованный план: %+v", ext.applied)
	}

	// Система изменилась после согласования — выполнения нет.
	id = requestIDPattern.FindStringSubmatch(e.must("request", "ext.yaml"))[1]
	e.must("approve", id)
	ext.remote = "v2"
	e.fails("план изменился после согласования заявки "+id, "apply", "--request", id)
	if len(ext.applied) != 1 {
		t.Fatal("разошедшийся план выполнен")
	}

	// Без заявки apply показывает план и передаёт его исполнителю.
	out := e.must("apply", "ext.yaml")
	if !strings.Contains(out, "limit = 200") || len(ext.applied) != 2 || ext.applied[1].Changes[0].Detail != "система v2" {
		t.Fatalf("apply без заявки:\n%s\n%+v", out, ext.applied)
	}
}

// Обратное действие во внешней системе — вторая согласованная операция;
// нечем вернуть — честный отказ.
func TestCompensate(t *testing.T) {
	e := newScriptEnv(t)
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-ext", ext)
	t.Cleanup(func() { scenario.Unregister("test-ext") })
	e.write(e.path("ext.yaml"), "service: test-ext\nparameters: {limit: 200}\ntargets: [{svm_ci: CI1}]\n")

	id := requestIDPattern.FindStringSubmatch(e.must("request", "ext.yaml"))[1]
	e.fails("обратное действие нужно только выполненной", "compensate", id)
	e.must("approve", id)
	e.must("apply", "--request", id)
	e.fails("обратного действия для заявки "+id+" нет: задачи job-CI1 уже отработали", "compensate", id)

	// С прежним значением обратное действие есть — и оно ждёт решения.
	id = requestIDPattern.FindStringSubmatch(e.must("request", "ext.yaml", "previous=100"))[1]
	e.must("approve", id)
	e.must("apply", "--request", id)
	out := e.must("compensate", id)
	undo := requestIDPattern.FindStringSubmatch(out)[1]
	if !strings.Contains(out, "обратное действие заявки "+id) || !strings.Contains(out, "limit = 100") {
		t.Fatalf("compensate:\n%s", out)
	}
	if len(ext.undone) != 0 {
		t.Fatal("обратное действие выполнено без согласования")
	}
	e.fails("не согласована", "apply", "--request", undo)
	e.must("approve", undo)
	e.must("apply", "--request", undo)
	if len(ext.undone) != 1 || ext.undone[0].Changes[0].Name != "limit = 100" {
		t.Fatalf("обратное действие: %+v", ext.undone)
	}
	if card := e.must("requests", id); !strings.Contains(card, "Обратное действие: заявка "+undo) {
		t.Fatalf("карточка без обратного действия:\n%s", card)
	}
	e.fails("уже есть обратное действие", "compensate", id)
	e.fails("сама обратное действие заявки "+id, "compensate", undo)

	// Исполнитель без обратного действия и правка файлов — отказ с причиной.
	scenario.Register("test-plain", plainExecutor{ext})
	t.Cleanup(func() { scenario.Unregister("test-plain") })
	e.write(e.path("plain.yaml"), "service: test-plain\nparameters: {limit: 200, previous: 100}\ntargets: [{svm_ci: CI2}]\n")
	plain := requestIDPattern.FindStringSubmatch(e.must("request", "plain.yaml"))[1]
	e.must("approve", plain)
	e.must("apply", "--request", plain)
	e.fails("исполнитель Plain не умеет обратного действия", "compensate", plain)
	e.write(e.path("changes.conf"), "max_connections = 200\n")
	file := requestIDPattern.FindStringSubmatch(e.must("request", "prod.conf", "changes.conf"))[1]
	e.must("approve", file)
	e.must("apply", "--request", file)
	e.fails("их откат точный — ochan rollback", "compensate", file)
}
