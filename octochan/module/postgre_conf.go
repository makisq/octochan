package module

import (
	"context"
	"fmt"
	"time"

	"ochan/rlm"
	"ochan/scenario"
)

// PostgresConfigFilesModule забирает конфигурационные файлы PostgreSQL.
type PostgresConfigFilesModule struct {
	data   *scenario.Scenario
	params Params
}

// NewPostgresConfigFilesModule создаёт модуль сервиса
// postgresql_se_get_config_files.
func NewPostgresConfigFilesModule(data *scenario.Scenario) (rlm.ScenarioModule, error) {
	return &PostgresConfigFilesModule{data: data, params: Params(data.Parameters)}, nil
}

func (m *PostgresConfigFilesModule) Validate() error {
	if err := m.params.RequireNonEmpty("port", "config_list", "itemname"); err != nil {
		return err
	}
	if len(m.params.StringList("config_list")) == 0 {
		return fmt.Errorf("параметр config_list пуст")
	}

	if len(m.data.Targets) == 0 {
		return fmt.Errorf("не указаны целевые серверы (targets)")
	}
	for i, target := range m.data.Targets {
		if target.SVMCI == "" {
			return fmt.Errorf("пустой svm_ci в target #%d", i+1)
		}
	}
	return nil
}

func (m *PostgresConfigFilesModule) GenerateRequests(_ context.Context, _ rlm.ScenarioAPI) ([]*rlm.APIRequest, error) {
	tableID := m.params.String("table_id", defaultTableID)
	configList := m.params.StringList("config_list")
	requests := make([]*rlm.APIRequest, 0, len(m.data.Targets))

	for _, target := range m.data.Targets {
		items, err := rlm.PrepareScenarioItems([]scenario.Target{target}, tableID)
		if err != nil {
			return nil, err
		}

		// Описание хоста собирается для каждой цели отдельно и содержит её
		// svm_ci. Прежняя версия строила hosts один раз до цикла, поэтому
		// во все запросы уезжал один и тот же (и притом безымянный) хост.
		host := map[string]any{
			"svm_ci":      target.SVMCI,
			"port":        m.params.Int("port", 5432),
			"itemname":    m.params.String("itemname", ""),
			"config_list": configList,
		}

		requests = append(requests, &rlm.APIRequest{
			Method: "POST",
			Body: map[string]any{
				"service":  "postgresql_se_get_config_files",
				"start_at": "now",
				"datetime": time.Now().Format(time.RFC3339),
				"params": map[string]any{
					"hosts": []any{host},
				},
				"items": items,
			},
		})
	}

	return requests, nil
}
