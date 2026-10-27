package runnerhost

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"octochan/config"
	"octochan/pluginhost"
	"octochan/scenario"
)

// Исполнитель модуля работает через реестр сценариев, как встроенный:
// собираем пример, ставим как доверенный, грузим так же, как ochan.
func TestRunnerModuleOverGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("собирает модуль")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("нет компилятора go для сборки модуля")
	}
	module := filepath.Join(t.TempDir(), "demo-runner"+pluginhost.ModuleExtension)
	if out, err := exec.Command("go", "build", "-o", module, "octochan/examples/runnermodule").CombinedOutput(); err != nil {
		t.Fatalf("сборка модуля: %v\n%s", err, out)
	}
	if err := pluginhost.WriteChecksum(module); err != nil {
		t.Fatal(err)
	}
	mm := pluginhost.NewModuleManager()
	defer mm.Cleanup()
	if err := mm.LoadModule(module); err != nil {
		t.Fatal(err)
	}
	runners := mm.Runners()
	if len(runners) != 1 || runners[0].Module != "demo-runner" || runners[0].Kind != "Demo" {
		t.Fatalf("исполнители: %+v", runners)
	}

	supply := func(executor string) pluginhost.Supply {
		if executor == "demo" {
			return pluginhost.Supply{Credentials: map[string]string{"token": "секрет"}}
		}
		return pluginhost.Supply{}
	}
	if warnings := Register(runners, supply); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	t.Cleanup(func() { Register(nil, nil) })

	runner, err := scenario.Resolve("demo")
	if err != nil {
		t.Fatal(err)
	}
	s := &scenario.Scenario{Service: "demo", Source: "demo.yaml",
		Parameters: map[string]any{"max_connections": 200},
		Targets:    []scenario.Target{{SVMCI: "CI001", CIList: []string{"CI001"}}}}
	ctx := context.Background()

	plan, err := runner.Plan(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Len() != 1 || plan.Changes[0] != (config.Change{Kind: config.ChangeTask, Target: "CI001", Name: "max_connections = 200"}) ||
		plan.Source != "demo.yaml" {
		t.Fatalf("план: %+v", plan)
	}
	// Исполнитель применяет переданный план, а не пересчитывает сценарий.
	result, err := runner.Apply(ctx, s, plan)
	if err != nil || len(result.Refs) != 1 || result.Refs[0] != "demo-CI001: max_connections = 200" {
		t.Fatalf("выполнение: %+v, %v", result, err)
	}

	// Обратное действие — отдельная операция с планом; нечем вернуть —
	// честный отказ с причиной.
	compensator, ok := runner.(scenario.Compensator)
	if !ok {
		t.Fatal("демонстрационный исполнитель объявил обратное действие, а ядро его не видит")
	}
	if _, err := compensator.PlanCompensation(ctx, s, plan, result.Refs); err == nil ||
		!strings.Contains(err.Error(), "прежнее значение неизвестно") {
		t.Fatalf("обратное действие без прежнего значения: %v", err)
	}
	s.Parameters["previous"] = 100
	undo, err := compensator.PlanCompensation(ctx, s, plan, result.Refs)
	if err != nil || undo.Len() != 1 || undo.Changes[0].Detail != "previous = 100" {
		t.Fatalf("план обратного действия: %+v, %v", undo, err)
	}
	done, err := compensator.Compensate(ctx, s, undo, result.Refs)
	if err != nil || len(done.Refs) != 1 || done.Refs[0] != "demo-undo-CI001" {
		t.Fatalf("обратное действие: %+v, %v", done, err)
	}
	if state, err := runner.(scenario.Waiter).Wait(ctx, "demo-CI001"); err != nil || state != "done" {
		t.Fatalf("ожидание: %q, %v", state, err)
	}
	st, err := runner.(scenario.StatusReporter).Status(ctx, "demo-CI001")
	if err != nil || st.State != "done" || len(st.Fields) != 1 || st.Fields[0].Value != "есть" {
		t.Fatalf("статус: %+v, %v", st, err)
	}

	// Выгруженный модуль уходит из реестра.
	mm.Cleanup()
	Register(mm.Runners(), nil)
	if _, err := scenario.Resolve("demo"); err == nil {
		t.Fatal("сервис выгруженного модуля остался в реестре")
	}
}

type core struct{}

func (core) Kind() string { return "Core" }
func (core) Plan(context.Context, *scenario.Scenario) (*config.Plan, error) {
	return &config.Plan{}, nil
}
func (core) Apply(context.Context, *scenario.Scenario, *config.Plan) (*scenario.Result, error) {
	return &scenario.Result{Refs: []string{"t1"}}, errFailed
}

var errFailed = errors.New("упал после первой задачи")

// Модуль не подменяет встроенного исполнителя.
func TestRegisterKeepsCoreRunner(t *testing.T) {
	scenario.Register("test-core", core{})
	t.Cleanup(func() { scenario.Unregister("test-core") })
	warnings := Register([]pluginhost.RemoteRunner{{Module: "m", Kind: "X", Services: []string{"test-core"},
		Ops: []string{pluginhost.OpPlan, pluginhost.OpApply}}}, nil)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "уже выполняет исполнитель Core") {
		t.Fatalf("предупреждения: %v", warnings)
	}
	if r, _ := scenario.Resolve("test-core"); r.Kind() != "Core" {
		t.Fatal("модуль подменил встроенного исполнителя")
	}
}

// Ошибка apply идёт в ответе вместе с созданным: следы не теряются.
func TestExposeKeepsRefsOnError(t *testing.T) {
	spec := Expose(core{}, "core")
	if strings.Join(spec.Ops, ",") != "plan,apply,apply-plan" {
		t.Fatalf("операции: %v", spec.Ops)
	}
	// Без плана исполнитель не применяет ничего: ядро до 0.2.0 его не
	// передаёт.
	bare, _ := json.Marshal(scenarioRequest{Scenario: &scenario.Scenario{Service: "core"}})
	if _, err := spec.Handle(context.Background(), pluginhost.RunnerRequest{Op: pluginhost.OpApply, Payload: bare}); err == nil ||
		!strings.Contains(err.Error(), "apply без плана") {
		t.Fatalf("apply без плана: %v", err)
	}
	payload, _ := json.Marshal(scenarioRequest{Scenario: &scenario.Scenario{Service: "core"}, Plan: &config.Plan{}})
	out, err := spec.Handle(context.Background(), pluginhost.RunnerRequest{Op: pluginhost.OpApply, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	var reply applyReply
	if err := json.Unmarshal(out, &reply); err != nil || reply.Error != "упал после первой задачи" ||
		len(reply.Refs) != 1 {
		t.Fatalf("ответ: %s, %v", out, err)
	}
	if _, err := spec.Handle(context.Background(), pluginhost.RunnerRequest{Op: pluginhost.OpWait}); err == nil {
		t.Fatal("исполнитель без ожидания ответил на wait")
	}
	if _, err := spec.Handle(context.Background(), pluginhost.RunnerRequest{Op: pluginhost.OpCompensate, Payload: payload}); err == nil {
		t.Fatal("исполнитель без обратного действия ответил на compensate")
	}
}

// Модуль, собранный до 0.2.0, плана не обещает: ядро им не выполняет.
func TestOldModuleIsNotPlanBound(t *testing.T) {
	runner := Runner(pluginhost.RemoteRunner{Module: "old", Kind: "Old", Services: []string{"x"},
		Ops: []string{pluginhost.OpPlan, pluginhost.OpApply}}, nil)
	_, err := runner.Apply(context.Background(), &scenario.Scenario{Service: "x"}, &config.Plan{})
	if !errors.Is(err, ErrNotPlanBound) || !strings.Contains(err.Error(), "исполнитель Old (модуль old)") {
		t.Fatalf("старый модуль: %v", err)
	}
	if _, ok := runner.(scenario.Compensator); ok {
		t.Fatal("обратное действие у исполнителя, который его не объявил")
	}
}

// Отказ до начала выполнения доходит до ядра отказом, а не сбоем: заявка
// остаётся согласованной.
func TestNotStartedCrossesTheWire(t *testing.T) {
	for _, tc := range []struct {
		err        error
		notStarted bool
	}{
		{scenario.NotStarted(errors.New("система недоступна")), true},
		{errors.New("упало на второй задаче"), false},
	} {
		data, err := applied(&scenario.Result{}, tc.err)
		if err != nil {
			t.Fatal(err)
		}
		var reply applyReply
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatal(err)
		}
		_, got := fromReply(reply)
		if got == nil || got.Error() != tc.err.Error() || scenario.IsNotStarted(got) != tc.notStarted {
			t.Fatalf("%v: получено %v (не начиналось: %v)", tc.err, got, scenario.IsNotStarted(got))
		}
	}
}
