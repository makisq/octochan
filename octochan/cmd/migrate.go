package cmd

import "octochan/scenario"

// До 0.2.0 эти сервисы выполнял встроенный исполнитель RLM. Теперь RLM —
// модуль (plugins/rlm), и отказ «сервис не зарегистрирован» называет, что
// поставить. Убрать в 0.3.0.
func init() {
	for _, service := range []string{
		"pangolin_restart",
		"postgresql_se_get_config_files",
		"psql_tuning_params_se",
		"psqlse_tuningpgbouncer",
	} {
		scenario.Moved(service, "rlm")
	}
}
