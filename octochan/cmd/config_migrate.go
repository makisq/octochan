package cmd

import (
	"fmt"
	"os"
	"strings"

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
	notes, err := dedupeConfigFile(path)
	if err != nil {
		return nil, err
	}
	set := map[string]any{}
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
		return notes, nil
	}
	if err := setConfigValues(path, set); err != nil {
		return nil, fmt.Errorf("перенос настроек в %s: %w", path, err)
	}
	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	return notes, nil
}

// Дубликат обязательной настройки модуля. ochan 0.3.0 писал обязательную
// настройку без умолчания дважды: заданной строкой сразу под заголовком
// модуля (setConfigValues) и закомментированной в блоке с пояснением
// (insertModuleBlock):
//
//	  pgstat:
//	    cluster: main
//	    # имя кластера (обязательно)
//	    # cluster: ""
//
// Теперь блок пишет её одной строкой, а старый конфиг правится при запуске:
// заданная строка встаёт на место закомментированной, под своё пояснение.
// Значение пользователя не меняется; прежняя версия читает файл так же.

// dedupeConfigFile убирает такие дубликаты в config.yaml и перечитывает
// его. Возвращает, что исправлено.
func dedupeConfigFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	text, fixed := dedupeModuleSettings(string(data))
	if len(fixed) == 0 {
		return nil, nil
	}
	if _, err := parseYAML(text); err != nil {
		// Правка испортила бы файл — оставляем как есть.
		return nil, nil
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		return nil, fmt.Errorf("исправление %s: %w", path, err)
	}
	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	notes := make([]string, 0, len(fixed))
	for _, key := range fixed {
		notes = append(notes, i18n.Sprintf("config.yaml: %s была записана дважды (ochan 0.3.0) — закомментированная копия убрана, значение осталось", key))
	}
	return notes, nil
}

// dedupeModuleSettings — текст config.yaml без закомментированных копий
// заданных обязательных настроек модулей и ключи, которые исправлены.
// Трогает только след 0.3.0: в заданном блоке модуля (modules: и модуль
// без #) строка «# ключ: ""» под пояснением «(обязательно)», а тот же ключ
// задан в этом блоке. Свои закомментированные строки пользователя — с
// другим значением или без такого пояснения — остаются.
func dedupeModuleSettings(text string) (string, []string) {
	lines := strings.Split(text, "\n")
	header := -1
	for i, line := range lines {
		if line == modulesKey+":" {
			header = i
			break
		}
	}
	if header < 0 {
		return text, nil
	}
	end := len(lines)
	for i := header + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" && !strings.HasPrefix(lines[i], " ") {
			end = i
			break
		}
	}
	required := map[string]bool{}
	for _, variant := range templateVariantsAll(requiredSuffix) {
		required[strings.TrimSpace(variant)] = true
	}
	isRequired := func(line string) bool {
		_, content, ok := templateCommentLine(line)
		if !ok {
			return false
		}
		for suffix := range required {
			if strings.HasSuffix(content, suffix) {
				return true
			}
		}
		return false
	}

	drop := map[int]bool{}
	var fixed []string
	for i := header + 1; i < end; i++ {
		line := lines[i]
		if leadingSpaces(line) != 2 || !strings.HasSuffix(line, ":") || strings.Contains(strings.TrimSpace(line), " ") {
			continue
		}
		module := strings.TrimSuffix(strings.TrimSpace(line), ":")
		// Строки блока модуля: всё глубже двух пробелов, пустые — конец.
		blockEnd := i + 1
		for blockEnd < end && strings.TrimSpace(lines[blockEnd]) != "" && leadingSpaces(lines[blockEnd]) > 2 {
			blockEnd++
		}
		active := map[string]int{} // ключ → строка «    ключ: значение»
		for j := i + 1; j < blockEnd; j++ {
			if leadingSpaces(lines[j]) != 4 {
				continue
			}
			content := strings.TrimSpace(lines[j])
			if strings.HasPrefix(content, "#") {
				continue
			}
			if key, _, ok := strings.Cut(content, ":"); ok {
				active[key] = j
			}
		}
		for j := i + 2; j < blockEnd; j++ {
			m := commentedLine.FindStringSubmatch(lines[j])
			if m == nil || len(m[1]) != 4 || m[2] != "" {
				continue
			}
			key, value, ok := strings.Cut(m[3], ":")
			if !ok || strings.TrimSpace(value) != `""` || !isRequired(lines[j-1]) {
				continue
			}
			at, set := active[key]
			if !set || drop[at] {
				continue
			}
			// Заданная строка одна (без вложенных) — она встаёт на место
			// копии, под пояснение; со вложенными — копия просто убирается.
			if at+1 >= blockEnd || leadingSpaces(lines[at+1]) <= 4 {
				lines[j] = lines[at]
				drop[at] = true
			} else {
				drop[j] = true
			}
			fixed = append(fixed, moduleSettingKey(module, key))
		}
		i = blockEnd - 1
	}
	if len(fixed) == 0 {
		return text, nil
	}
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if !drop[i] {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n"), fixed
}

// templateVariantsAll — текст шаблона на всех языках ochan.
func templateVariantsAll(text string) []string {
	out := []string{text}
	for _, lang := range i18n.Languages() {
		if t, ok := i18n.Lookup(lang.Code, text); ok {
			out = append(out, t)
		}
	}
	return out
}

// reportMigrations печатает перенесённое в stderr: это видит и ochan
// update — он запускает новую версию и показывает её вывод.
func reportMigrations(notes []string) {
	for _, note := range notes {
		fmt.Fprintln(os.Stderr, note)
	}
}
