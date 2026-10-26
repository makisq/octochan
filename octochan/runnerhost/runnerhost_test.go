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

	tokens := func(executor string) map[string]string {
		if executor == "demo" {
			return map[string]string{"token": "секрет"}
		}
		return nil
	}
	if warnings := Register(runners, tokens); len(warnings) != 0 {
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
	result, err := runner.Apply(ctx, s)
	if err != nil || len(result.Refs) != 1 || result.Refs[0] != "demo-CI001" {
		t.Fatalf("выполнение: %+v, %v", result, err)
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
func (core) Apply(context.Context, *scenario.Scenario) (*scenario.Result, error) {
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
	if strings.Join(spec.Ops, ",") != "plan,apply" {
		t.Fatalf("операции: %v", spec.Ops)
	}
	payload, _ := json.Marshal(scenarioRequest{Scenario: &scenario.Scenario{Service: "core"}})
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
}
