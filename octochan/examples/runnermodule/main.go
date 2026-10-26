// Модуль-исполнитель для ochan — пример и заготовка.
//
// Модуль приносит исполнителя сценариев: ochan выбирает его по полю
// service сценария, как встроенного, и plan, apply, status работают с ним
// так же. Этот исполнитель ничего не меняет — он показывает, как устроен
// исполнитель: план по параметрам сценария на каждой цели, ссылки на
// созданное, ожидание и статус. Настоящий ходит в свою систему — по SSH,
// HTTP, gRPC — с токеном, который оператор задал ochan auth --executor demo.
//
// Сборка и установка:
//
//	go build -o demo-runner.hcplugin ./examples/runnermodule
//	ochan '!install' demo-runner.hcplugin
//
// Сценарий для него:
//
//	service: demo
//	parameters: {max_connections: 200}
//	targets: [{svm_ci: CI001}]
package main

import (
	"context"
	"fmt"
	"sort"

	"octochan/config"
	"octochan/pluginhost"
	"octochan/runnerhost"
	"octochan/scenario"

	"github.com/spf13/cobra"
)

// demo — исполнитель: scenario.Runner, а ожидание и статус — по желанию.
type demo struct{}

func (demo) Kind() string { return "Demo" }

func (demo) Plan(_ context.Context, s *scenario.Scenario) (*config.Plan, error) {
	plan := &config.Plan{Command: "apply", Source: s.Source}
	keys := make([]string, 0, len(s.Parameters))
	for key := range s.Parameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Прежнего значения исполнитель не знает — это задача, а не правка с
	// «было → стало». Исполнитель, который может прочитать цель, пишет
	// config.ChangeSet с From.
	for _, ci := range s.AllCIs() {
		for _, key := range keys {
			plan.Add(config.Change{Kind: config.ChangeTask, Target: ci, Name: fmt.Sprintf("%s = %v", key, s.Parameters[key])})
		}
	}
	if plan.Len() == 0 {
		plan.Warn("у сценария нет параметров или целей: исполнителю нечего делать")
	}
	return plan, nil
}

func (demo) Apply(_ context.Context, s *scenario.Scenario) (*scenario.Result, error) {
	result := &scenario.Result{}
	for _, ci := range s.AllCIs() {
		result.Refs = append(result.Refs, "demo-"+ci)
	}
	result.Summary = fmt.Sprintf("задач: %d", len(result.Refs))
	return result, nil
}

func (demo) Wait(context.Context, string) (string, error) { return "done", nil }

func (demo) Status(ctx context.Context, ref string) (*scenario.Status, error) {
	// Сам токен не показывается: только есть он или нет.
	token := "нет"
	if runnerhost.Token(ctx) != "" {
		token = "есть"
	}
	return &scenario.Status{Ref: ref, State: "done", Fields: []scenario.StatusField{{Name: "Токен", Value: token}}}, nil
}

// module — модуль ochan: команд и языков у него нет, есть исполнитель.
type module struct{}

func (module) GetCommands() []*cobra.Command { return nil }
func (module) Init(map[string]any) error     { return nil }
func (module) Name() string                  { return "demo-runner" }
func (module) Version() string               { return "0.1.0" }

func (module) Runners() []pluginhost.RunnerSpec {
	return []pluginhost.RunnerSpec{runnerhost.Expose(demo{}, "demo")}
}

func main() { pluginhost.ServeCommandModule(module{}) }
