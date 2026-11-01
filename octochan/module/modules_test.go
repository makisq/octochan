package module

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ochan/rlm"
	"ochan/scenario"
)

// buildScenario разбирает YAML-сценарий и создаёт модуль для его сервиса.
func buildScenario(t *testing.T, yaml string) rlm.ScenarioModule {
	t.Helper()

	data, err := scenario.Parse("<тест>", []byte(yaml))
	if err != nil {
		t.Fatalf("разбор сценария: %v", err)
	}

	switch data.Service {
	case "psqlse_tuningpgbouncer":
		module, err := NewPgBouncerTuningModule(data)
		if err != nil {
			t.Fatal(err)
		}
		return module
	case "postgresql_se_get_config_files":
		module, err := NewPostgresConfigFilesModule(data)
		if err != nil {
			t.Fatal(err)
		}
		return module
	case "pangolin_restart":
		module, err := NewPangolinRestartModule(data)
		if err != nil {
			t.Fatal(err)
		}
		return module
	case "psql_tuning_params_se":
		module, err := NewPsqlTuningParamsModule(data)
		if err != nil {
			t.Fatal(err)
		}
		return module
	default:
		t.Fatalf("неизвестный сервис %q", data.Service)
		return nil
	}
}

// ---------------------------------------------------------------------------
// pgbouncer
// ---------------------------------------------------------------------------

func TestPgBouncerBuildsRequestPerTarget(t *testing.T) {
	module := buildScenario(t, `
service: psqlse_tuningpgbouncer
parameters:
  role: standalone
  version: "6.5.1"
  max_client_conn: 900
targets:
  - svm_ci: CI-1
  - svm_ci: CI-2
`)
	if err := module.Validate(); err != nil {
		t.Fatalf("валидация: %v", err)
	}

	api := newFakeAPI()
	requests, err := module.GenerateRequests(context.Background(), api)
	if err != nil {
		t.Fatalf("GenerateRequests: %v", err)
	}

	if len(requests) != 2 {
		t.Fatalf("получено %d запросов, ожидалось 2", len(requests))
	}
	// Модуль не должен обращаться к API вовсе.
	if api.postCount() != 0 {
		t.Errorf("модуль отправил %d запросов на этапе подготовки", api.postCount())
	}

	params, err := requestParams(requests[0])
	if err != nil {
		t.Fatal(err)
	}

	// role и version уходят в описание хоста, а не в общие параметры.
	if _, leaked := params["role"]; leaked {
		t.Error("role попал в общие параметры вместо описания хоста")
	}
	if params["max_client_conn"] != 900 {
		t.Errorf("max_client_conn = %#v", params["max_client_conn"])
	}

	hosts, ok := params["hosts"].([]any)
	if !ok || len(hosts) != 1 {
		t.Fatalf("hosts: %#v", params["hosts"])
	}
	host := hosts[0].(map[string]any)
	if host["svm_ci"] != "CI-1" || host["role"] != "standalone" || host["version"] != "6.5.1" {
		t.Errorf("описание хоста: %#v", host)
	}
}

func TestPgBouncerDeepCopyKeepsRequestsIndependent(t *testing.T) {
	// Прежний DeepCopyMap не копировал срезы, и все запросы делили
	// один и тот же список hosts.
	module := buildScenario(t, `
service: psqlse_tuningpgbouncer
parameters:
  role: standalone
  version: "13"
targets:
  - svm_ci: CI-1
  - svm_ci: CI-2
`)

	requests, err := module.GenerateRequests(context.Background(), newFakeAPI())
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	for i, req := range requests {
		params, err := requestParams(req)
		if err != nil {
			t.Fatal(err)
		}
		host := params["hosts"].([]any)[0].(map[string]any)
		ci := host["svm_ci"].(string)

		if seen[ci] {
			t.Fatalf("запрос #%d ссылается на тот же хост %s", i+1, ci)
		}
		seen[ci] = true
	}
}

// ---------------------------------------------------------------------------
// postgresql_se_get_config_files
// ---------------------------------------------------------------------------

func TestPostgresConfigFilesAcceptsListAndString(t *testing.T) {
	// config_list раньше приводился к строке без проверки типа
	// и ронял процесс, если в YAML был написан список.
	for name, scenario := range map[string]string{
		"строка": `
service: postgresql_se_get_config_files
parameters:
  port: 5433
  itemname: pg
  config_list: "postgresql.conf, pg_hba.conf"
targets:
  - svm_ci: CI-1
`,
		"список": `
service: postgresql_se_get_config_files
parameters:
  port: 5433
  itemname: pg
  config_list:
    - postgresql.conf
    - pg_hba.conf
targets:
  - svm_ci: CI-1
`,
	} {
		t.Run(name, func(t *testing.T) {
			module := buildScenario(t, scenario)
			if err := module.Validate(); err != nil {
				t.Fatalf("валидация: %v", err)
			}

			requests, err := module.GenerateRequests(context.Background(), newFakeAPI())
			if err != nil {
				t.Fatalf("GenerateRequests: %v", err)
			}

			params, err := requestParams(requests[0])
			if err != nil {
				t.Fatal(err)
			}
			host := params["hosts"].([]any)[0].(map[string]any)

			list, ok := host["config_list"].([]string)
			if !ok || len(list) != 2 || list[0] != "postgresql.conf" {
				t.Errorf("config_list: %#v", host["config_list"])
			}
		})
	}
}

func TestPostgresConfigFilesHostCarriesTargetCI(t *testing.T) {
	// Прежняя версия собирала hosts один раз до цикла, поэтому во все
	// запросы уходил один и тот же безымянный хост.
	module := buildScenario(t, `
service: postgresql_se_get_config_files
parameters:
  port: 5433
  itemname: pg
  config_list: postgresql.conf
targets:
  - svm_ci: CI-1
  - svm_ci: CI-2
`)

	requests, err := module.GenerateRequests(context.Background(), newFakeAPI())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"CI-1", "CI-2"}
	for i, req := range requests {
		params, err := requestParams(req)
		if err != nil {
			t.Fatal(err)
		}
		host := params["hosts"].([]any)[0].(map[string]any)
		if host["svm_ci"] != want[i] {
			t.Errorf("запрос #%d: хост %v, ожидался %s", i+1, host["svm_ci"], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// pangolin_restart
// ---------------------------------------------------------------------------

func TestPangolinRestartRequiresConfirm(t *testing.T) {
	module := buildScenario(t, `
service: pangolin_restart
parameters:
  restart: instance
  skip_sm_conflicts: false
  confirm: false
targets:
  - svm_ci: CI-1
`)

	err := module.Validate()
	if err == nil {
		t.Fatal("перезапуск без подтверждения прошёл валидацию")
	}
	if !strings.Contains(err.Error(), "confirm") {
		t.Errorf("непонятное сообщение: %v", err)
	}
}

func TestPangolinRestartRejectsItemsAndTargetsTogether(t *testing.T) {
	// Прежняя версия формировала запросы и по items, и по targets,
	// то есть перезапускала одни и те же серверы дважды.
	module := buildScenario(t, `
service: pangolin_restart
parameters:
  restart: instance
  skip_sm_conflicts: false
  confirm: true
targets:
  - svm_ci: CI-1
items:
  - invsvm_ci_svm: CI-1
`)

	err := module.Validate()
	if err == nil {
		t.Fatal("одновременные items и targets приняты")
	}
	if !strings.Contains(err.Error(), "дважды") {
		t.Errorf("непонятное сообщение: %v", err)
	}
}

func TestPangolinRestartBoolFromJSONNumber(t *testing.T) {
	// JSON отдаёт числа как float64; прежний getBoolParam его не знал,
	// и confirm: 1 молча превращался в false.
	module := buildScenario(t, `{
      "service": "pangolin_restart",
      "parameters": {"restart": "instance", "skip_sm_conflicts": 0, "confirm": 1},
      "targets": [{"svm_ci": "CI-1"}]
    }`)

	if err := module.Validate(); err != nil {
		t.Fatalf("confirm: 1 не признан подтверждением: %v", err)
	}

	requests, err := module.GenerateRequests(context.Background(), newFakeAPI())
	if err != nil {
		t.Fatal(err)
	}
	params, err := requestParams(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if params["confirm"] != true {
		t.Errorf("confirm = %#v", params["confirm"])
	}
}

// ---------------------------------------------------------------------------
// psql_tuning_params_se — единственный модуль, который ходит в API
// ---------------------------------------------------------------------------

const tuningScenario = `
service: psql_tuning_params_se
parameters:
  role: standalone
  port: 5433
  svm_ip: 10.0.0.10
  ip_replics: "10.0.0.11, 10.0.0.12"
  hugepages: false
  parameters:
    - name: max_connections
      setting: "800"
targets:
  - svm_ci: CI-1
  - svm_ci: CI-2
`

func TestPsqlTuningUsesDiscoveredTableID(t *testing.T) {
	// table_id, найденный разведкой, раньше передавался в построение
	// основного запроса и там игнорировался: уходил захардкоженный
	// "pangolinunique".
	api := newFakeAPI()
	api.waitFn = func(taskID string) (map[string]any, error) {
		return map[string]any{"status": "completed", "table_id": "psqlsecluster"}, nil
	}

	module := buildScenario(t, tuningScenario)
	requests, err := module.GenerateRequests(context.Background(), api)
	if err != nil {
		t.Fatalf("GenerateRequests: %v", err)
	}

	for i, req := range requests {
		items, err := requestItems(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := items[0]["table_id"]; got != "psqlsecluster" {
			t.Errorf("запрос #%d: table_id = %v, ожидался найденный разведкой", i+1, got)
		}
	}
}

func TestPsqlTuningMapsAddressesByGlobalPosition(t *testing.T) {
	// ip_replics — плоский список, а прежняя версия индексировала его
	// позицией CI внутри своей цели, из-за чего адреса разъезжались.
	api := newFakeAPI()
	module := buildScenario(t, tuningScenario)

	requests, err := module.GenerateRequests(context.Background(), api)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"CI-1": "10.0.0.11", "CI-2": "10.0.0.12"}
	for _, req := range requests {
		params, err := requestParams(req)
		if err != nil {
			t.Fatal(err)
		}
		ci, _ := params["svm_ci"].(string)
		if got := params["ip_replics"]; got != want[ci] {
			t.Errorf("%s: ip_replics = %v, ожидался %s", ci, got, want[ci])
		}
	}
}

func TestPsqlTuningFailsWhenAddressesAreMissing(t *testing.T) {
	// Проверка адресов должна происходить до отправки разведки,
	// иначе задачи уже созданы, а сценарий всё равно падает.
	api := newFakeAPI()
	module := buildScenario(t, `
service: psql_tuning_params_se
parameters:
  role: standalone
  port: 5433
  svm_ip: 10.0.0.10
  ip_replics: "10.0.0.11"
  hugepages: false
targets:
  - svm_ci: CI-1
  - svm_ci: CI-2
`)

	if _, err := module.GenerateRequests(context.Background(), api); err == nil {
		t.Fatal("нехватка адресов не обнаружена")
	}
	if api.postCount() != 0 {
		t.Errorf("отправлено %d запросов разведки до проверки адресов", api.postCount())
	}
}

func TestPsqlTuningFallsBackToNextTableID(t *testing.T) {
	// RLM отвергает первый table_id — модуль обязан попробовать следующий.
	api := newFakeAPI()
	api.postFn = func(_ string, body any) (map[string]any, error) {
		payload := body.(map[string]any)
		items := payload["items"].([]any)
		tableID := items[0].(map[string]any)["table_id"].(string)

		if tableID == "pangolinunique" {
			return nil, apiError(400, "Указан неверный идентификатор таблицы (table_id)")
		}
		return map[string]any{"id": float64(7001)}, nil
	}

	module := buildScenario(t, tuningScenario)
	requests, err := module.GenerateRequests(context.Background(), api)
	if err != nil {
		t.Fatalf("перебор table_id не сработал: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("получено %d запросов", len(requests))
	}

	// На каждый CI должно уйти по два обращения: отвергнутое и принятое.
	if api.postCount() != 4 {
		t.Errorf("отправлено %d запросов разведки, ожидалось 4", api.postCount())
	}
	for _, service := range api.postedServices() {
		if service != "psql_tuning_params_se_sys" {
			t.Errorf("на этапе разведки отправлен сервис %q", service)
		}
	}
}

func TestPsqlTuningStopsOnNonTableIDError(t *testing.T) {
	// Ошибка не про table_id перебирать варианты не должна:
	// это лишние обращения к API при недоступной сети.
	api := newFakeAPI()
	api.postFn = func(string, any) (map[string]any, error) {
		return nil, apiError(500, "internal server error")
	}

	module := buildScenario(t, tuningScenario)
	if _, err := module.GenerateRequests(context.Background(), api); err == nil {
		t.Fatal("ошибка сервера проигнорирована")
	}

	// По одному обращению на CI, без перебора четырёх table_id.
	if api.postCount() != 2 {
		t.Errorf("отправлено %d запросов, ожидалось 2", api.postCount())
	}
}

func TestPsqlTuningReportsCreatedTasksOnPartialFailure(t *testing.T) {
	// При частичном отказе уже созданные задачи разведки продолжают
	// выполняться на стороне RLM, и об этом обязаны сообщить.
	api := newFakeAPI()
	api.postFn = func(_ string, body any) (map[string]any, error) {
		payload := body.(map[string]any)
		items := payload["items"].([]any)
		if items[0].(map[string]any)["invsvm_ci_svm"] == "CI-2" {
			return nil, apiError(500, "internal server error")
		}
		return map[string]any{"id": float64(9001)}, nil
	}

	module := buildScenario(t, tuningScenario)
	_, err := module.GenerateRequests(context.Background(), api)
	if err == nil {
		t.Fatal("частичный отказ не обнаружен")
	}
	if !strings.Contains(err.Error(), "9001") {
		t.Errorf("в сообщении нет уже созданных задач: %v", err)
	}
}

func TestPsqlTuningPreservesErrorChain(t *testing.T) {
	// Причины сворачивались в строку через fmt.Errorf("%s", msg),
	// из-за чего errors.Is переставал работать и команда plan не могла
	// отличить запрет режима плана от настоящего сбоя.
	api := newFakeAPI()
	api.postFn = func(string, any) (map[string]any, error) {
		return nil, rlm.ErrDryRun
	}

	module := buildScenario(t, tuningScenario)
	_, err := module.GenerateRequests(context.Background(), api)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if !errors.Is(err, rlm.ErrDryRun) {
		t.Errorf("цепочка ошибок потеряна: %v", err)
	}
}

func TestPsqlTuningDBParamsTypes(t *testing.T) {
	// Числовые значения уходят числами, значения с единицами — строками.
	api := newFakeAPI()
	module := buildScenario(t, `
service: psql_tuning_params_se
parameters:
  role: standalone
  port: 5433
  svm_ip: 10.0.0.10
  ip_replics: "10.0.0.11"
  hugepages: false
  parameters:
    - name: max_connections
      setting: "800"
    - name: shared_buffers
      setting: "8GB"
targets:
  - svm_ci: CI-1
`)

	requests, err := module.GenerateRequests(context.Background(), api)
	if err != nil {
		t.Fatal(err)
	}
	params, err := requestParams(requests[0])
	if err != nil {
		t.Fatal(err)
	}

	dbParams, ok := params["db_params"].([]any)
	if !ok || len(dbParams) != 2 {
		t.Fatalf("db_params: %#v", params["db_params"])
	}

	first := dbParams[0].(map[string]any)
	if first["value"] != float64(800) {
		t.Errorf("max_connections ушёл как %#v, ожидалось число", first["value"])
	}
	second := dbParams[1].(map[string]any)
	if second["value"] != "8GB" {
		t.Errorf("shared_buffers ушёл как %#v, ожидалась строка", second["value"])
	}
}
