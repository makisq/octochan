package cmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"ochan/config"
	"ochan/scenario"
)

// nodeExecutor — узел с API: ответ на вход несёт string_i (Result.Shared),
// настройка без верной string_i получает 500.
type nodeExecutor struct{ tuned []string }

func (*nodeExecutor) Kind() string { return "Node" }

func (*nodeExecutor) Plan(_ context.Context, s *scenario.Scenario) (*config.Plan, error) {
	plan := &config.Plan{Command: "apply"}
	plan.Add(config.Change{Kind: config.ChangeTask, Target: "node_n", Name: "POST " + s.Service})
	return plan, nil
}

func (x *nodeExecutor) Apply(_ context.Context, s *scenario.Scenario, _ *config.Plan) (*scenario.Result, error) {
	switch s.Service {
	case "at-login":
		return &scenario.Result{Summary: "вход выполнен", Refs: []string{"login-1"},
			Shared: map[string]string{"string_i": "k-42-secret"}}, nil
	case "at-login-raw":
		// Исполнитель ничего не выделяет: только тело ответа системы.
		return &scenario.Result{Summary: "вход выполнен", Refs: []string{"login-2"},
			Responses: []string{`{"session": {"id": 9}, "string_i": "k-42-secret"}`}}, nil
	case "at-tune":
		if s.Parameters["string_i"] != "k-42-secret" {
			return nil, scenario.NotStarted(errors.New("HTTP 500: нет верного string_i"))
		}
		x.tuned = append(x.tuned, s.Source)
		return &scenario.Result{Summary: "настроено", Refs: []string{"tune-1"}}, nil
	}
	return nil, errors.New("HTTP 404")
}

// apply @ ("tune.yaml" | "login.yaml") из оболочки: tune ждёт string_i,
// которую исполнитель отдал в ответе на login, — отдельно от вывода, так
// что в выводе её нет. Внутри сценария apply по-прежнему запрещён.
func TestApplyAt(t *testing.T) {
	e := newScriptEnv(t)
	node := &nodeExecutor{}
	scenario.Register("at-login", node)
	scenario.Register("at-tune", node)
	t.Cleanup(func() {
		scenario.Unregister("at-login")
		scenario.Unregister("at-tune")
	})
	e.write("login.yaml", "service: at-login\ntargets:\n  - ip: 10.0.0.9\nparameters:\n  user: ops\n")
	e.write("tune.yaml", "service: at-tune\ntargets:\n  - ip: 10.0.0.9\nparameters:\n  work_mem: 32MB\n  string_i: ?\n")

	t.Cleanup(shellReset)
	t.Cleanup(func() { setContext(rootCmd, context.Background()) })
	var err error
	out := captureStdout(t, func() {
		rootCmd.SetOut(os.Stdout)
		err = runLineShell(context.Background(), strings.NewReader(`apply @ ("tune.yaml" | "login.yaml")`+"\n"), false)
	})
	setContext(rootCmd, context.Background())
	if err != nil {
		t.Fatalf("оболочка: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Создано:") || !strings.Contains(out, "tune-1") || !strings.Contains(out, "login-1") {
		t.Fatalf("вывод:\n%s", out)
	}
	if strings.Contains(out, "k-42-secret") {
		t.Fatalf("общая строка исполнителя попала в вывод:\n%s", out)
	}
	if len(node.tuned) != 1 || !strings.Contains(e.read("tune.at.yaml"), `string_i: "k-42-secret"`) {
		t.Fatalf("настройка: %v\n%s", node.tuned, e.read("tune.at.yaml"))
	}
	if !strings.Contains(e.read("tune.yaml"), "string_i: ?") {
		t.Fatal("плейбук tune.yaml изменён")
	}

	// Только тело ответа: строку находит сам @.
	scenario.Register("at-login-raw", node)
	t.Cleanup(func() { scenario.Unregister("at-login-raw") })
	e.write("login-raw.yaml", "service: at-login-raw\ntargets:\n  - ip: 10.0.0.9\n")
	out = captureStdout(t, func() {
		rootCmd.SetOut(os.Stdout)
		err = runLineShell(context.Background(), strings.NewReader(`apply @ ("tune.yaml" | "login-raw.yaml")`+"\n"), false)
	})
	setContext(rootCmd, context.Background())
	if err != nil || len(node.tuned) != 2 || strings.Contains(out, "k-42-secret") {
		t.Fatalf("строка из тела ответа: %v, настроек %d\n%s", err, len(node.tuned), out)
	}
	// Из командной строки — ochan eval, как строка оболочки.
	e.must("eval", `apply @ ("tune.yaml" | "login.yaml")`)
	if len(node.tuned) != 3 {
		t.Fatalf("eval: настроек %d", len(node.tuned))
	}

	e.write("inner.ochan", `apply @ ("login.yaml")`+"\n")
	e.fails("из сценария не выполняется", "run", "inner.ochan")
}
