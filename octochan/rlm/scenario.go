package rlm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"octochan/config"
	"octochan/scenario"
)

// APIRequest — подготовленный модулем запрос к RLM API.
// Заголовки здесь больше не хранятся: авторизацию проставляет APIClient,
// раньше каждый модуль собирал их сам и тащил токен через свои структуры.
type APIRequest struct {
	Method string
	URL    string
	Body   map[string]any
}

// ScenarioAPI — то, что модуль сценария вправе делать с внешней системой.
//
// Модули принимают интерфейс, а не *APIClient, по двум причинам. Во-первых,
// без этого их нельзя проверить, не имея живого RLM: раньше у модулей
// не было ни одного теста именно поэтому. Во-вторых, набор методов задаёт
// границу дозволенного — переключателя режима плана здесь намеренно нет,
// чтобы модуль не мог его отключить.
type ScenarioAPI interface {
	// TasksURL — адрес создания задач.
	TasksURL() string
	// Get выполняет чтение.
	Get(ctx context.Context, url string, out any) error
	// Post создаёт задачу. В режиме плана возвращает ErrDryRun.
	Post(ctx context.Context, url string, body, out any) error
	// WaitForTask ждёт завершения задачи.
	WaitForTask(ctx context.Context, taskID string, opts WaitOptions) (map[string]any, error)
	// IsDryRun сообщает, что изменяющие запросы запрещены.
	IsDryRun() bool
}

// Проверка на этапе компиляции: настоящий клиент обязан подходить под
// интерфейс, иначе о расхождении узнали бы только в рантайме.
var _ ScenarioAPI = (*APIClient)(nil)

// ScenarioModule описывает конкретный сервис RLM.
type ScenarioModule interface {
	Validate() error
	GenerateRequests(ctx context.Context, api ScenarioAPI) ([]*APIRequest, error)
}

// ScenarioModuleCreator создаёт модуль по данным сценария.
type ScenarioModuleCreator func(data *scenario.Scenario) (ScenarioModule, error)

// Реестр модулей защищён мьютексом: регистрация выполняется из init()
// нескольких пакетов, а чтение — из рабочих горутин.
var (
	scenarioModulesMu sync.RWMutex
	scenarioModules   = make(map[string]ScenarioModuleCreator)
)

func lookupScenarioModule(serviceName string) (ScenarioModuleCreator, bool) {
	scenarioModulesMu.RLock()
	defer scenarioModulesMu.RUnlock()
	creator, ok := scenarioModules[serviceName]
	return creator, ok
}

// ExecuteScenario разбирает сценарий, валидирует его и выполняет запросы.
// Возвращает идентификаторы созданных задач в порядке следования запросов.
func ExecuteScenario(ctx context.Context, api ScenarioAPI, data *scenario.Scenario) ([]string, error) {
	if data == nil {
		return nil, errors.New("сценарий не задан")
	}

	module, err := buildModule(data)
	if err != nil {
		return nil, err
	}

	requests, err := module.GenerateRequests(ctx, api)
	if err != nil {
		return nil, fmt.Errorf("ошибка подготовки запросов: %w", err)
	}
	if len(requests) == 0 {
		return nil, errors.New("модуль не сформировал ни одного запроса")
	}

	return submitRequests(ctx, api, requests)
}

// buildModule создаёт и валидирует модуль сценария.
func buildModule(data *scenario.Scenario) (ScenarioModule, error) {
	creator, ok := lookupScenarioModule(data.Service)
	if !ok {
		return nil, fmt.Errorf("модуль для сервиса %q не зарегистрирован; доступны: %v",
			data.Service, scenario.Services())
	}

	module, err := creator(data)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания модуля: %w", err)
	}
	if err := module.Validate(); err != nil {
		return nil, fmt.Errorf("ошибка валидации: %w", err)
	}
	return module, nil
}

// PlanScenario готовит сценарий и описывает, что было бы сделано,
// ничего при этом не делая.
//
// Клиент переводится в режим плана: модуль psql_tuning_params_se обращается
// к API уже на этапе подготовки, и без запрета «показать план» создавало бы
// задачи разведки. Если модуль упирается в этот запрет, план получается
// неполным — и об этом говорится прямо, а не замалчивается.
func PlanScenario(ctx context.Context, api ScenarioAPI, data *scenario.Scenario) (*config.Plan, error) {
	if data == nil {
		return nil, errors.New("сценарий не задан")
	}

	module, err := buildModule(data)
	if err != nil {
		return nil, err
	}

	plan := &config.Plan{
		Command:     "apply",
		Source:      data.Service,
		Destructive: isDestructiveService(data.Service),
	}

	// Обёртка, а не флаг: модуль держит только интерфейс и до настоящего
	// Post дотянуться не может.
	requests, err := module.GenerateRequests(ctx, DryRun(api))
	if err != nil {
		if !errors.Is(err, ErrDryRun) {
			return nil, fmt.Errorf("ошибка подготовки запросов: %w", err)
		}

		// Полный план недоступен: модулю нужен ответ внешней системы.
		// Показываем то, что известно до обращения к ней.
		plan.Warn("модуль %s обращается к API на этапе подготовки, поэтому план "+
			"неполный: показаны цели, но не итоговые параметры", data.Service)
		for _, target := range data.Targets {
			for _, ci := range target.GetCIs() {
				plan.Add(config.Change{
					Kind:   config.ChangeTask,
					Target: ci,
					Name:   data.Service,
					Detail: "состав запроса определится после разведки",
				})
			}
		}
		plan.Sort()
		return plan, nil
	}

	for i, req := range requests {
		plan.Add(config.Change{
			Kind:   config.ChangeTask,
			Target: requestTarget(req, i),
			Name:   data.Service,
			Detail: describeRequestParams(req),
		})
	}

	plan.Sort()
	return plan, nil
}

// isDestructiveService помечает сервисы, прерывающие обслуживание.
func isDestructiveService(service string) bool {
	return strings.Contains(service, "restart")
}

// requestTarget вытаскивает из тела запроса, к какому серверу он относится.
func requestTarget(req *APIRequest, index int) string {
	if items, ok := req.Body["items"]; ok {
		if cis := collectCIs(items); len(cis) > 0 {
			return strings.Join(cis, ", ")
		}
	}
	return fmt.Sprintf("запрос #%d", index+1)
}

// collectCIs находит идентификаторы серверов в items любого вида:
// модули собирают их и как []map[string]string, и как []any.
func collectCIs(items any) []string {
	var found []string

	appendFrom := func(m map[string]any) {
		if ci, ok := m["invsvm_ci_svm"].(string); ok && ci != "" {
			found = append(found, ci)
		}
	}

	switch list := items.(type) {
	case []map[string]string:
		for _, item := range list {
			if ci := item["invsvm_ci_svm"]; ci != "" {
				found = append(found, ci)
			}
		}
	case []map[string]any:
		for _, item := range list {
			appendFrom(item)
		}
	case []any:
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				appendFrom(m)
			}
		}
	}
	return found
}

// describeRequestParams коротко описывает параметры запроса.
func describeRequestParams(req *APIRequest) string {
	params, ok := req.Body["params"].(map[string]any)
	if !ok {
		return ""
	}

	keys := make([]string, 0, len(params))
	for key := range params {
		// hosts и db_params — вложенные структуры, в однострочное
		// описание они не помещаются.
		if key == "hosts" || key == "db_params" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, params[key]))
	}
	return strings.Join(parts, ", ")
}

// submitRequests отправляет запросы с ограничением параллелизма.
//
// Ретраев здесь нет намеренно: запросы не идемпотентны, и прежняя реализация
// с тремя попытками порождала дубли задач на стороне RLM.
func submitRequests(ctx context.Context, api ScenarioAPI, requests []*APIRequest) ([]string, error) {
	maxParallel := currentSettings().MaxParallel
	if maxParallel <= 0 {
		maxParallel = 5
	}

	taskIDs := make([]string, len(requests))
	errs := make([]error, len(requests))

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, maxParallel)

	for i, req := range requests {
		wg.Add(1)
		go func(idx int, r *APIRequest) {
			defer wg.Done()

			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				errs[idx] = ctx.Err()
				return
			}

			url := r.URL
			if url == "" {
				url = api.TasksURL()
			}

			var response map[string]any
			if err := api.Post(ctx, url, r.Body, &response); err != nil {
				errs[idx] = fmt.Errorf("запрос #%d: %w", idx+1, err)
				return
			}

			taskID, err := ExtractTaskID(response)
			if err != nil {
				errs[idx] = fmt.Errorf("запрос #%d: %w", idx+1, err)
				return
			}
			taskIDs[idx] = taskID
		}(i, req)
	}

	wg.Wait()

	// Возвращаем и созданные задачи, и все ошибки: прежняя реализация отдавала
	// только первую ошибку, остальные молча терялись.
	created := make([]string, 0, len(taskIDs))
	for _, id := range taskIDs {
		if id != "" {
			created = append(created, id)
		}
	}

	return created, errors.Join(errs...)
}

// PrepareScenarioItems собирает items для перечисленных целей.
// tableID раньше был захардкожен внутри функции.
func PrepareScenarioItems(targets []scenario.Target, tableID string) ([]map[string]string, error) {
	if tableID == "" {
		return nil, errors.New("не указан table_id")
	}

	items := make([]map[string]string, 0, len(targets))
	for i, target := range targets {
		if target.SVMCI == "" {
			return nil, fmt.Errorf("пустой svm_ci в target #%d", i+1)
		}
		items = append(items, map[string]string{
			"table_id":      tableID,
			"invsvm_ci_svm": target.SVMCI,
		})
	}
	return items, nil
}

// DeepCopyMap возвращает глубокую копию map, включая вложенные срезы.
// Прежняя версия копировала только вложенные map, а срезы оставались общими.
func DeepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return DeepCopyMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = deepCopyValue(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(val))
		for i, item := range val {
			out[i] = DeepCopyMap(item)
		}
		return out
	case []string:
		out := make([]string, len(val))
		copy(out, val)
		return out
	default:
		return v
	}
}
