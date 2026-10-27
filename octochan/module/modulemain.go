// Package module содержит сервисы сценариев RLM: по одному на вид задачи.
//
// Это часть модуля plugins/rlm: ядро ochan пакет не импортирует, а модуль
// объявляет эти сервисы исполнителем RLM. Модули ochan (.hcplugin)
// подключаются через go-plugin, см. pluginhost.ModuleManager.
package module

import "octochan/rlm"

func init() {
	rlm.RegisterScenarioModule("psqlse_tuningpgbouncer", NewPgBouncerTuningModule)
	rlm.RegisterScenarioModule("psql_tuning_params_se", NewPsqlTuningParamsModule)
	rlm.RegisterScenarioModule("postgresql_se_get_config_files", NewPostgresConfigFilesModule)
	rlm.RegisterScenarioModule("pangolin_restart", NewPangolinRestartModule)
}
