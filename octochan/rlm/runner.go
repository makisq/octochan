package rlm

import (
	"context"
	"fmt"

	"octochan/config"
	"octochan/scenario"
)

// runner выполняет сценарий через RLM API.
//
// Это обычный scenario.Runner, зарегистрированный под именами своих сервисов.
// Команда apply не знает, что за ним стоит RLM: она спрашивает исполнителя
// у реестра по имени сервиса. Рядом может встать исполнитель, работающий
// по SSH, локально или как угодно ещё.
type runner struct{}

// Kind сообщает, каким способом будет выполнен сценарий.
func (runner) Kind() string { return "RLM" }

func (r runner) Plan(ctx context.Context, s *scenario.Scenario) (*config.Plan, error) {
	api, err := NewAPIClient()
	if err != nil {
		return nil, err
	}
	return PlanScenario(ctx, api, s)
}

// Apply создаёт ровно задачи согласованного плана: ExecuteScenario сверяет
// запросы с ним до первой отправки. Обратного действия у RLM нет —
// scenario.Compensator он не реализует, и ochan compensate честно
// отказывает.
func (r runner) Apply(ctx context.Context, s *scenario.Scenario, plan *config.Plan) (*scenario.Result, error) {
	api, err := NewAPIClient()
	if err != nil {
		return nil, err
	}

	taskIDs, err := ExecuteScenario(ctx, api, s, plan)

	result := &scenario.Result{
		Refs:    taskIDs,
		Summary: fmt.Sprintf("создано задач: %d", len(taskIDs)),
	}
	// Созданные задачи возвращаются даже при ошибке: они уже существуют
	// во внешней системе, и умолчать о них нельзя.
	return result, err
}

// Wait дожидается завершения задачи RLM.
// Реализует необязательный интерфейс scenario.Waiter.
func (r runner) Wait(ctx context.Context, ref string) (string, error) {
	api, err := NewAPIClient()
	if err != nil {
		return "", err
	}

	status, err := api.WaitForTask(ctx, ref, WaitOptions{})
	if err != nil {
		return "", err
	}

	state, _ := status["status"].(string)
	return state, nil
}

// Status показывает состояние задачи RLM и её журнал.
// Реализует необязательный интерфейс scenario.StatusReporter.
func (r runner) Status(ctx context.Context, ref string) (*scenario.Status, error) {
	api, err := NewAPIClient()
	if err != nil {
		return nil, err
	}
	raw, err := api.TaskStatusWithEvents(ctx, ref)
	if raw == nil {
		return nil, err
	}

	status := &scenario.Status{Ref: ref}
	if state, ok := raw["status"]; ok && state != nil {
		status.State = fmt.Sprint(state)
	}
	for _, field := range []struct{ key, name string }{{"created_at", "Создана"}, {"updated_at", "Обновлена"}} {
		if value, ok := raw[field.key]; ok && value != nil {
			status.Fields = append(status.Fields, scenario.StatusField{Name: field.name, Value: fmt.Sprint(value)})
		}
	}
	if progress, ok := raw["progress"].(float64); ok {
		status.Fields = append(status.Fields, scenario.StatusField{Name: "Прогресс", Value: fmt.Sprintf("%.0f%%", progress)})
	}
	if events, ok := raw["events"].([]map[string]any); ok {
		for _, event := range events {
			status.Events = append(status.Events, scenario.StatusEvent{
				Time: fmt.Sprint(event["timestamp"]), Level: fmt.Sprint(event["level"]), Message: fmt.Sprint(event["message"]),
			})
		}
	}
	// Статус без журнала событий — всё равно статус: его показывают, а
	// ошибку журнала называют.
	return status, err
}

// RegisterScenarioModule регистрирует модуль сервиса RLM.
//
// Помимо внутреннего реестра модулей сервис объявляется в общем реестре
// исполнителей, чтобы apply нашёл его по имени без всяких флагов.
func RegisterScenarioModule(serviceName string, creator ScenarioModuleCreator) {
	scenarioModulesMu.Lock()
	scenarioModules[serviceName] = creator
	scenarioModulesMu.Unlock()

	scenario.Register(serviceName, runner{})
}
