package module

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"ochan/rlm"
	"ochan/scenario"
)

// intelService — служебный сценарий разведки, который собирает сведения
// о сервере перед основной настройкой.
const intelService = "psql_tuning_params_se_sys"

// candidateTableIDs перебираются, пока RLM не примет один из них.
var candidateTableIDs = []string{
	"pangolinunique",
	"psqlsecluster",
	"psqlseclusterstandalone",
	"postgresql_instances",
}

// DBParam — один настраиваемый параметр СУБД.
type DBParam struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

// MainParams — параметры основного запроса настройки.
type MainParams struct {
	IpReplics       string    `json:"ip_replics"`
	DBParams        []DBParam `json:"db_params"`
	Port            int       `json:"port"`
	Restart         bool      `json:"restart"`
	Hugepages       bool      `json:"hugepages"`
	Role            string    `json:"role"`
	SkipSMConflicts bool      `json:"skip_sm_conflicts"`
	SvmCI           string    `json:"svm_ci"`
	SvmIP           string    `json:"svm_ip"`
	TaskID          int       `json:"task_id"`
}

// MainItem — элемент инвентаря основного запроса.
type MainItem struct {
	TableID     string `json:"table_id"`
	InvsvmCISvm string `json:"invsvm_ci_svm"`
}

// MainPayload — тело основного запроса.
type MainPayload struct {
	Datetime string     `json:"datetime"`
	Items    []MainItem `json:"items"`
	Params   MainParams `json:"params"`
	Service  string     `json:"service"`
	StartAt  string     `json:"start_at"`
}

// PsqlTuningParamsModule настраивает параметры PostgreSQL.
//
// Сначала на каждый CI запускается задача разведки, затем по её результату
// формируется основной запрос.
type PsqlTuningParamsModule struct {
	data   *scenario.Scenario
	params Params
}

// NewPsqlTuningParamsModule создаёт модуль сервиса psql_tuning_params_se.
func NewPsqlTuningParamsModule(data *scenario.Scenario) (rlm.ScenarioModule, error) {
	return &PsqlTuningParamsModule{data: data, params: Params(data.Parameters)}, nil
}

func (m *PsqlTuningParamsModule) Validate() error {
	if len(m.data.Targets) == 0 {
		return fmt.Errorf("не указаны целевые серверы (targets)")
	}
	for i, target := range m.data.Targets {
		if len(target.GetCIs()) == 0 {
			return fmt.Errorf("пустой svm_ci в target #%d", i+1)
		}
	}

	if err := m.params.RequireNonEmpty("role", "port", "svm_ip", "hugepages"); err != nil {
		return err
	}

	tuning, ok := m.data.Parameters["parameters"]
	if !ok {
		return nil
	}

	list, ok := tuning.([]any)
	if !ok {
		return fmt.Errorf("параметр parameters должен быть списком, получено %T", tuning)
	}
	for i, raw := range list {
		param, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("неверный формат параметра настройки #%d: ожидался объект, получено %T", i+1, raw)
		}
		if itemString(param, "name") == "" || itemString(param, "setting") == "" {
			return fmt.Errorf("неполный параметр настройки #%d: требуются name и setting", i+1)
		}
	}

	return nil
}

// allCIs возвращает плоский список CI в порядке следования целей.
func (m *PsqlTuningParamsModule) allCIs() []string {
	var cis []string
	for _, target := range m.data.Targets {
		cis = append(cis, target.GetCIs()...)
	}
	return cis
}

// ipForIndex сопоставляет CI с адресом из ip_replics по сквозной позиции.
//
// Прежняя версия индексировала ip_replics позицией CI внутри своей цели,
// поэтому при нескольких targets адреса разъезжались, а fallback молча
// подставлял ip_replics[0] — то есть настройка могла уехать не на тот сервер.
func (m *PsqlTuningParamsModule) ipForIndex(index int) (string, error) {
	replics := m.params.StringList("ip_replics")
	if index < len(replics) && replics[index] != "" {
		return replics[index], nil
	}
	if len(replics) == 1 && replics[0] != "" {
		// Один адрес на один сервер — единственный случай, когда
		// подстановка однозначна.
		if len(m.allCIs()) == 1 {
			return replics[0], nil
		}
	}
	return "", fmt.Errorf("в ip_replics нет адреса для CI #%d: указано %d адресов на %d серверов",
		index+1, len(replics), len(m.allCIs()))
}

// intelOutcome — результат разведки по одному CI.
type intelOutcome struct {
	ci      string
	taskID  string
	tableID string
	err     error
}

func (m *PsqlTuningParamsModule) GenerateRequests(ctx context.Context, api rlm.ScenarioAPI) ([]*rlm.APIRequest, error) {
	cis := m.allCIs()
	if len(cis) == 0 {
		return nil, fmt.Errorf("нет CI для запуска разведки")
	}

	// Адреса проверяем до отправки запросов: иначе разведка уже создана,
	// а сценарий всё равно падает.
	for i := range cis {
		if _, err := m.ipForIndex(i); err != nil {
			return nil, err
		}
	}

	outcomes := m.runIntel(ctx, api, cis)

	// Ошибки собираем все сразу и обязательно сообщаем, какие задачи разведки
	// уже успели создаться: они остаются выполняться на стороне RLM.
	var (
		failed  []error
		created []string
	)
	for _, o := range outcomes {
		if o.taskID != "" {
			created = append(created, o.taskID)
		}
		if o.err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", o.ci, o.err))
		}
	}
	if len(failed) > 0 {
		note := ""
		if len(created) > 0 {
			note = fmt.Sprintf(" (уже созданы задачи разведки: %s)", strings.Join(created, ", "))
		}

		// Причины оборачиваются, а не форматируются в строку: вызывающему
		// нужно уметь распознать их через errors.Is. Из-за прежнего
		// fmt.Errorf("%s", msg) цепочка терялась, и команда plan не могла
		// отличить запрет режима плана от настоящего сбоя разведки.
		return nil, fmt.Errorf("разведка не удалась для %d из %d CI%s: %w",
			len(failed), len(cis), note, errors.Join(failed...))
	}

	requests := make([]*rlm.APIRequest, 0, len(outcomes))
	for i, o := range outcomes {
		ip, err := m.ipForIndex(i)
		if err != nil {
			return nil, err
		}

		req, err := m.buildMainRequest(o, ip)
		if err != nil {
			return nil, fmt.Errorf("CI %s: %w", o.ci, err)
		}
		requests = append(requests, req)
	}

	log.Printf("psql_tuning_params_se: подготовлено запросов — %d", len(requests))
	return requests, nil
}

// runIntel запускает и дожидается разведки по всем CI.
//
// Результаты пишутся в слайс по индексу: прежняя версия писала в общие
// map и поле структуры из нескольких горутин без синхронизации, что давало
// "concurrent map writes" уже на двух серверах.
func (m *PsqlTuningParamsModule) runIntel(ctx context.Context, api rlm.ScenarioAPI, cis []string) []intelOutcome {
	outcomes := make([]intelOutcome, len(cis))

	var wg sync.WaitGroup
	for i, ci := range cis {
		wg.Add(1)
		go func(idx int, ci string) {
			defer wg.Done()
			outcomes[idx] = m.intelForCI(ctx, api, ci)
		}(i, ci)
	}
	wg.Wait()

	return outcomes
}

func (m *PsqlTuningParamsModule) intelForCI(ctx context.Context, api rlm.ScenarioAPI, ci string) intelOutcome {
	outcome := intelOutcome{ci: ci}

	taskID, tableID, err := m.startIntel(ctx, api, ci)
	if err != nil {
		outcome.err = err
		return outcome
	}
	outcome.taskID = taskID
	outcome.tableID = tableID

	log.Printf("разведка %s: задача %s (table_id=%s)", ci, taskID, tableID)

	status, err := api.WaitForTask(ctx, taskID, rlm.WaitOptions{
		PollInterval: 5 * time.Second,
		Timeout:      m.intelTimeout(),
		OnStatus: func(s string) {
			log.Printf("разведка %s (задача %s): статус %s", ci, taskID, s)
		},
	})
	if err != nil {
		outcome.err = err
		return outcome
	}

	// table_id из ответа разведки приоритетнее подобранного при отправке:
	// раньше он вычислялся, передавался в tryCreateMainRequest и там молча
	// игнорировался, а основной запрос всегда уходил с "pangolinunique".
	if discovered, ok := status["table_id"].(string); ok && discovered != "" {
		outcome.tableID = discovered
	}

	return outcome
}

func (m *PsqlTuningParamsModule) intelTimeout() time.Duration {
	if d := time.Duration(m.params.Int("intel_timeout_seconds", 0)) * time.Second; d > 0 {
		return d
	}
	return 15 * time.Minute
}

// startIntel подбирает подходящий table_id и создаёт задачу разведки.
func (m *PsqlTuningParamsModule) startIntel(ctx context.Context, api rlm.ScenarioAPI, ci string) (taskID, tableID string, err error) {
	tableIDs := candidateTableIDs
	if explicit := m.params.String("table_id", ""); explicit != "" {
		tableIDs = []string{explicit}
	}

	var lastErr error
	for _, candidate := range tableIDs {
		id, err := m.sendIntel(ctx, api, ci, candidate, false)
		if err == nil {
			return id, candidate, nil
		}
		lastErr = err

		// Недостающий table_row_id — отдельный случай: повторяем с заглушкой.
		if rlm.IsTableRowIDError(err) {
			id, retryErr := m.sendIntel(ctx, api, ci, candidate, true)
			if retryErr == nil {
				return id, candidate, nil
			}
			lastErr = retryErr
		}

		if rlm.IsTableIDError(lastErr) {
			log.Printf("разведка %s: table_id %q не подошёл, пробую следующий", ci, candidate)
			continue
		}

		// Ошибка не про table_id — перебирать дальше бессмысленно.
		return "", "", lastErr
	}

	// Раньше здесь терялась последняя ошибка и возвращалось общее сообщение.
	return "", "", fmt.Errorf("не удалось подобрать table_id (перебрано %d вариантов): %w",
		len(tableIDs), lastErr)
}

func (m *PsqlTuningParamsModule) sendIntel(ctx context.Context, api rlm.ScenarioAPI, ci, tableID string, withRowID bool) (string, error) {
	item := map[string]any{
		"table_id":      tableID,
		"invsvm_ci_svm": ci,
	}
	if withRowID {
		item["table_row_id"] = "None"
	}

	payload := map[string]any{
		"service":  intelService,
		"start_at": "now",
		"items":    []any{item},
	}

	var response map[string]any
	if err := api.Post(ctx, api.TasksURL(), payload, &response); err != nil {
		return "", err
	}
	return rlm.ExtractTaskID(response)
}

func (m *PsqlTuningParamsModule) buildMainRequest(outcome intelOutcome, ip string) (*rlm.APIRequest, error) {
	// RLM ожидает числовой task_id разведки.
	taskID, err := strconv.Atoi(outcome.taskID)
	if err != nil {
		return nil, fmt.Errorf("нечисловой идентификатор задачи разведки %q: %w", outcome.taskID, err)
	}

	payload := MainPayload{
		Service:  "psql_tuning_params_se",
		StartAt:  "now",
		Datetime: time.Now().Format(time.RFC3339),
		Items: []MainItem{{
			TableID:     outcome.tableID,
			InvsvmCISvm: outcome.ci,
		}},
		Params: MainParams{
			IpReplics:       ip,
			DBParams:        m.dbParams(),
			Port:            m.params.Int("port", 5432),
			Restart:         m.params.Bool("restart", false),
			Hugepages:       m.params.Bool("hugepages", false),
			Role:            m.params.String("role", ""),
			SkipSMConflicts: m.params.Bool("skip_sm_conflicts", true),
			SvmCI:           outcome.ci,
			SvmIP:           m.params.String("svm_ip", ""),
			TaskID:          taskID,
		},
	}

	body, err := structToMap(payload)
	if err != nil {
		return nil, err
	}
	return &rlm.APIRequest{Method: "POST", Body: body}, nil
}

// dbParams переводит блок parameters сценария в список DBParam.
func (m *PsqlTuningParamsModule) dbParams() []DBParam {
	raw, ok := m.data.Parameters["parameters"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}

	params := make([]DBParam, 0, len(list))
	for _, item := range list {
		param, ok := item.(map[string]any)
		if !ok {
			continue
		}

		setting := itemString(param, "setting")

		// Числовые значения отдаём числами, всё остальное — строками
		// (например "8GB" или "logical").
		var value any = setting
		if n, err := strconv.Atoi(setting); err == nil {
			value = n
		} else if f, err := strconv.ParseFloat(setting, 64); err == nil {
			value = f
		}

		params = append(params, DBParam{
			Name:  itemString(param, "name"),
			Value: value,
			Unit:  itemString(param, "unit"),
		})
	}
	return params
}
