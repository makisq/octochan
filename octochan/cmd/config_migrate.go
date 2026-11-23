package cmd

import (
	"fmt"
	"os"

	"ochan/core/i18n"

	"github.com/spf13/viper"
)

// Перенос значений: новая версия ochan, у которой настройка называется
// иначе, переносит заданное значение со старого ключа на новый сама, при
// первом запуске. Прежняя строка остаётся: версия, к которой откатятся
// (ochan update --rollback), читает тот же файл и найдёт её. Пользователь
// ничего не заполняет заново.

// configRename — настройка переименована: значение From переносится в
// To, если To ещё не задан. Since — версия, где появился новый ключ.
type configRename struct {
	From, To, Since string
}

// configRenames — переименования настроек, старые первыми. Новое
// переименование — сюда, а старый ключ — читать, пока нового нет.
var configRenames = []configRename{
	{From: "defaults.log_path", To: "paths.logs", Since: "0.3.0"},
}

// migrateConfig переносит значения переименованных настроек в config.yaml
// и перечитывает его. Возвращает, что перенесено.
func migrateConfig(path string) ([]string, error) {
	set := map[string]any{}
	var notes []string
	for _, r := range configRenames {
		if !viper.InConfig(r.From) || viper.InConfig(r.To) {
			continue
		}
		value := viper.Get(r.From)
		if settingText(value) == "" {
			continue
		}
		set[r.To] = value
		notes = append(notes, i18n.Sprintf("config.yaml: %s перенесено в %s (ochan %s); прежняя строка оставлена — её читает прежняя версия",
			r.From, r.To, r.Since))
	}
	if len(set) == 0 {
		return nil, nil
	}
	if err := setConfigValues(path, set); err != nil {
		return nil, fmt.Errorf("перенос настроек в %s: %w", path, err)
	}
	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	return notes, nil
}

// reportMigrations печатает перенесённое в stderr: это видит и ochan
// update — он запускает новую версию и показывает её вывод.
func reportMigrations(notes []string) {
	for _, note := range notes {
		fmt.Fprintln(os.Stderr, note)
	}
}
