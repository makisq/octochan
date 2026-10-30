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
//	ochan '!!install' demo-runner.hcplugin
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

// Apply применяет ровно согласованный план: задача на каждое изменение
// плана, а не на то, что получилось бы из сценария сейчас.
func (demo) Apply(_ context.Context, _ *scenario.Scenario, plan *config.Plan) (*scenario.Result, error) {
	result := &scenario.Result{}
	for _, change := range plan.Changes {
		result.Refs = append(result.Refs, "demo-"+change.Target+": "+change.Name)
	}
	result.Summary = fmt.Sprintf("задач: %d", len(result.Refs))
	return result, nil
}

// PlanCompensation — план обратного действия: вернуть параметр, который
// задача поставила. Прежнего значения демонстрационный исполнитель не
// знает, поэтому обратное действие возможно, только если сценарий его
// назвал — параметр previous; иначе — честный отказ.
func (demo) PlanCompensation(_ context.Context, s *scenario.Scenario, applied *config.Plan, _ []string) (*config.Plan, error) {
	previous, ok := s.Parameters["previous"]
	if !ok {
		return nil, fmt.Errorf("обратного действия нет: прежнее значение неизвестно (параметр previous сценария не задан)")
	}
	plan := &config.Plan{Command: "compensate", Source: s.Source}
	for _, change := range applied.Changes {
		plan.Add(config.Change{Kind: config.ChangeTask, Target: change.Target, Name: "вернуть " + change.Name,
			Detail: fmt.Sprintf("previous = %v", previous)})
	}
	return plan, nil
}

// Compensate применяет согласованный план обратного действия.
func (demo) Compensate(_ context.Context, _ *scenario.Scenario, plan *config.Plan, _ []string) (*scenario.Result, error) {
	result := &scenario.Result{}
	for _, change := range plan.Changes {
		result.Refs = append(result.Refs, "demo-undo-"+change.Target)
	}
	result.Summary = fmt.Sprintf("обратных задач: %d", len(result.Refs))
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
func (module) Version() string               { return "0.2.0" }

func (module) Runners() []pluginhost.RunnerSpec {
	return []pluginhost.RunnerSpec{runnerhost.Expose(demo{}, "demo")}
}

func main() { pluginhost.ServeCommandModule(module{}) }
