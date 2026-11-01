package rlm

import (
	"context"
	"errors"
	"testing"

	"ochan/scenario"
)

// stubAPI — минимальная реализация ScenarioAPI для проверки обёртки.
type stubAPI struct {
	posts int
	gets  int
	waits int
}

func (s *stubAPI) TasksURL() string { return "https://rlm.test/api/tasks.json" }
func (s *stubAPI) IsDryRun() bool   { return false }

func (s *stubAPI) Get(context.Context, string, any) error {
	s.gets++
	return nil
}

func (s *stubAPI) Post(_ context.Context, _ string, _, out any) error {
	s.posts++
	if target, ok := out.(*map[string]any); ok {
		*target = map[string]any{"id": float64(1)}
	}
	return nil
}

func (s *stubAPI) WaitForTask(context.Context, string, WaitOptions) (map[string]any, error) {
	s.waits++
	return map[string]any{"status": "completed"}, nil
}

func TestDryRunBlocksChanges(t *testing.T) {
	stub := &stubAPI{}
	api := DryRun(stub)

	if !api.IsDryRun() {
		t.Error("обёртка не сообщает о режиме плана")
	}

	// Чтение проходит: план опирается на текущее состояние.
	if err := api.Get(context.Background(), "https://rlm.test/api/tasks/1/", nil); err != nil {
		t.Errorf("чтение запрещено: %v", err)
	}
	if stub.gets != 1 {
		t.Errorf("чтение не дошло до клиента: %d вызовов", stub.gets)
	}

	err := api.Post(context.Background(), "https://rlm.test/api/tasks.json", map[string]any{}, nil)
	if !errors.Is(err, ErrDryRun) {
		t.Errorf("создание задачи не отклонено: %v", err)
	}
	if stub.posts != 0 {
		t.Errorf("запрос дошёл до клиента, хотя режим плана: %d вызовов", stub.posts)
	}

	// Ждать нечего: задачи не создавалось. Отказ понятнее зависания.
	if _, err := api.WaitForTask(context.Background(), "1", WaitOptions{}); !errors.Is(err, ErrDryRun) {
		t.Errorf("ожидание задачи не отклонено: %v", err)
	}
	if stub.waits != 0 {
		t.Errorf("ожидание дошло до клиента: %d вызовов", stub.waits)
	}
}

func TestDryRunIsIdempotent(t *testing.T) {
	// Повторное оборачивание не должно создавать матрёшку.
	api := DryRun(DryRun(&stubAPI{}))
	if _, ok := api.(dryRunAPI); !ok {
		t.Fatalf("неожиданный тип %T", api)
	}
	if inner := api.(dryRunAPI).inner; inner == nil {
		t.Fatal("внутренний API потерян")
	}
	if _, nested := api.(dryRunAPI).inner.(dryRunAPI); nested {
		t.Error("обёртка вложена в саму себя")
	}
}

func TestRealClientIsNotDryRun(t *testing.T) {
	// У настоящего клиента режима плана нет: его накладывает только обёртка.
	// Раньше это было изменяемое поле, которое можно было забыть сбросить.
	client := &APIClient{}
	if client.IsDryRun() {
		t.Error("клиент сообщает о режиме плана")
	}
}

// blockingModule имитирует модуль, обращающийся к API на этапе подготовки.
type blockingModule struct {
	data   *scenario.Scenario
	posted bool
}

func (m *blockingModule) Validate() error { return nil }

func (m *blockingModule) GenerateRequests(ctx context.Context, api ScenarioAPI) ([]*APIRequest, error) {
	var response map[string]any
	if err := api.Post(ctx, api.TasksURL(), map[string]any{"service": "разведка"}, &response); err != nil {
		return nil, err
	}
	m.posted = true
	return []*APIRequest{{Method: "POST", Body: map[string]any{"service": "основной"}}}, nil
}

func TestPlanScenarioCannotCreateTasks(t *testing.T) {
	// Главное свойство команды plan: она физически не может ничего создать.
	const service = "тест_план_защита"

	var module *blockingModule
	RegisterScenarioModule(service, func(data *scenario.Scenario) (ScenarioModule, error) {
		module = &blockingModule{data: data}
		return module, nil
	})

	stub := &stubAPI{}
	scenarioJSON := []byte(`{"service":"` + service + `","targets":[{"svm_ci":"CI-1"}]}`)

	plan, err := PlanScenario(context.Background(), stub, mustParse(t, scenarioJSON))
	if err != nil {
		t.Fatalf("PlanScenario вернул ошибку вместо неполного плана: %v", err)
	}

	if stub.posts != 0 {
		t.Errorf("план создал %d задач", stub.posts)
	}
	if module.posted {
		t.Error("модуль считает, что его запрос прошёл")
	}

	// План неполный — об этом должно быть сказано прямо.
	if len(plan.Warnings) == 0 {
		t.Error("неполнота плана не отмечена предупреждением")
	}
	if plan.Len() != 1 {
		t.Errorf("в плане %d изменений, ожидалась одна цель", plan.Len())
	}
	if plan.Changes[0].Target != "CI-1" {
		t.Errorf("цель плана: %q", plan.Changes[0].Target)
	}
}

func TestPlanScenarioFullPlanWithoutAPI(t *testing.T) {
	// Модуль, которому API не нужен, даёт полный план и не трогает сеть.
	const service = "тест_план_полный"

	RegisterScenarioModule(service, func(data *scenario.Scenario) (ScenarioModule, error) {
		return &offlineModule{data: data}, nil
	})

	stub := &stubAPI{}
	scenarioJSON := []byte(`{"service":"` + service + `","targets":[{"svm_ci":"CI-1"},{"svm_ci":"CI-2"}]}`)

	plan, err := PlanScenario(context.Background(), stub, mustParse(t, scenarioJSON))
	if err != nil {
		t.Fatalf("PlanScenario: %v", err)
	}

	if len(plan.Warnings) != 0 {
		t.Errorf("полный план помечен предупреждениями: %v", plan.Warnings)
	}
	if plan.Len() != 2 {
		t.Fatalf("в плане %d изменений, ожидалось 2", plan.Len())
	}
	if stub.posts != 0 || stub.gets != 0 {
		t.Errorf("план обратился к API: %d POST, %d GET", stub.posts, stub.gets)
	}

	if plan.Changes[0].Target != "CI-1" || plan.Changes[1].Target != "CI-2" {
		t.Errorf("цели плана: %q, %q", plan.Changes[0].Target, plan.Changes[1].Target)
	}
}

type offlineModule struct{ data *scenario.Scenario }

func (m *offlineModule) Validate() error { return nil }

func (m *offlineModule) GenerateRequests(context.Context, ScenarioAPI) ([]*APIRequest, error) {
	requests := make([]*APIRequest, 0, len(m.data.Targets))
	for _, target := range m.data.Targets {
		requests = append(requests, &APIRequest{
			Method: "POST",
			Body: map[string]any{
				"service": "оффлайн",
				"items":   []map[string]string{{"invsvm_ci_svm": target.SVMCI}},
				"params":  map[string]any{"restart": "instance"},
			},
		})
	}
	return requests, nil
}

func TestPlanMarksDestructiveService(t *testing.T) {
	// Сервисы, прерывающие обслуживание, должны быть видны в плане сразу.
	const service = "тест_restart_сервис"

	RegisterScenarioModule(service, func(data *scenario.Scenario) (ScenarioModule, error) {
		return &offlineModule{data: data}, nil
	})

	plan, err := PlanScenario(context.Background(), &stubAPI{},
		mustParse(t, []byte(`{"service":"`+service+`","targets":[{"svm_ci":"CI-1"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Destructive {
		t.Error("сервис с restart в имени не помечен как прерывающий обслуживание")
	}
}

// mustParse разбирает сценарий или прекращает тест.
func mustParse(t *testing.T, data []byte) *scenario.Scenario {
	t.Helper()
	s, err := scenario.Parse("<тест>", data)
	if err != nil {
		t.Fatalf("разбор сценария: %v", err)
	}
	return s
}
