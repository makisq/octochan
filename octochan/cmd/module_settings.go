package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"ochan/core/i18n"
	"ochan/modules/pluginhost"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Настройки модулей: модуль объявляет их (pluginhost.Configurable), ochan
// пишет их в config.yaml при !!install — раздел modules.<имя модуля>, с
// пояснением и значением по умолчанию, как шаблон, — и при каждой загрузке
// передаёт модулю значения в Init. Пользователю не нужно знать, что модуль
// вообще настраивается: строки появляются в конфиге сами, как параметры
// расширения в postgresql.conf.

// modulesKey — раздел config.yaml с настройками модулей.
const modulesKey = "modules"

func init() {
	pluginhost.SettingValues = moduleSettingValues
}

// moduleSettingKey — ключ настройки модуля в config.yaml.
func moduleSettingKey(module, key string) string { return modulesKey + "." + module + "." + key }

// moduleSettingValues — значения настроек модуля: заданное в config.yaml,
// иначе значение по умолчанию модуля. missing — обязательные без значения.
func moduleSettingValues(module string, declared []pluginhost.Setting) (map[string]string, []string) {
	values := map[string]string{}
	var missing []string
	for _, s := range declared {
		key := moduleSettingKey(module, s.Key)
		if s.Default != "" {
			viper.SetDefault(key, s.Default)
		}
		value := settingText(viper.Get(key))
		if value == "" {
			if s.Required {
				missing = append(missing, key)
			}
			continue
		}
		values[s.Key] = value
	}
	return values, missing
}

// settingText — значение настройки текстом для модуля: строка как есть,
// список и словарь — YAML одной строкой.
func settingText(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case []any, []string, map[string]any:
		text, err := yamlInline(val)
		if err != nil {
			return fmt.Sprint(val)
		}
		return text
	}
	return fmt.Sprint(v)
}

// settingDefault — значение по умолчанию одной строкой YAML: 30s, [a, b],
// "" для пустого.
func settingDefault(text string) string {
	if text == "" {
		return `""`
	}
	var v any
	if err := yaml.Unmarshal([]byte(text), &v); err == nil && v != nil {
		if inline, err := yamlInline(v); err == nil {
			return inline
		}
	}
	inline, _ := yamlInline(text)
	return inline
}

// moduleMissing — предупреждение о модуле, которому не хватает
// обязательных настроек.
func moduleMissing(info pluginhost.ModuleInfo) string {
	if len(info.Missing) == 0 {
		return ""
	}
	return i18n.Sprintf("модуль %s: не задано обязательное — %s в %s", info.Name, strings.Join(info.Missing, ", "),
		viper.ConfigFileUsed())
}

// declareModuleSettings записывает настройки модуля в config.yaml: блок
// modules.<имя> с пояснениями, умолчания закомментированы. Уже записанное
// — заданное пользователем или прежней установкой — не трогается.
// Обязательная настройка без значения по умолчанию ставится без #, пустой:
// её видно, и её надо заполнить. Возвращает ключи, которые добавлены.
func declareModuleSettings(path string, info pluginhost.ModuleInfo) ([]string, error) {
	if len(info.Settings) == 0 {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := writeConfigTemplate(path, nil); err != nil {
			return nil, err
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	text := string(data)
	var entries []configEntry
	var added, required []string
	for _, s := range info.Settings {
		key := moduleSettingKey(info.Name, s.Key)
		if templateHasKey(text, key) {
			continue
		}
		comment := s.Description
		if comment == "" {
			comment = templateText(moduleNoComment)
		}
		if s.Required {
			comment += templateText(requiredSuffix)
			if s.Default == "" {
				required = append(required, key)
			}
		}
		entries = append(entries, configEntry{key: key, comment: comment, example: rawYAML(settingDefault(s.Default))})
		added = append(added, key)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	title := strings.TrimSpace(info.Name + " " + info.Version)
	text = insertModuleBlock(text, info.Name, title, entries)
	if _, err := parseYAML(text); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return nil, err
	}
	if len(required) > 0 {
		set := map[string]any{}
		for _, key := range required {
			set[key] = ""
		}
		if err := setConfigValues(path, set); err != nil {
			return nil, err
		}
	}
	return added, nil
}

// rawYAML — значение, уже записанное YAML: шаблон вставляет его как есть.
type rawYAML string

// insertModuleBlock вставляет блок модуля в раздел modules: — под его
// заголовок (с # или без); раздела нет — в конец файла.
func insertModuleBlock(text, module, title string, entries []configEntry) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	header, active := -1, false
	for i, line := range lines {
		switch line {
		case modulesKey + ":":
			header, active = i, true
		case "# " + modulesKey + ":":
			header = i
		}
		if header >= 0 {
			break
		}
	}
	if header < 0 {
		lines = append(lines, "", "# "+templateText(modulesTitle),
			"# "+modulesKey+":")
		header = len(lines) - 1
	}
	// Конец раздела: первая пустая строка или строка без отступа.
	end := len(lines)
	for i := header + 1; i < len(lines); i++ {
		body, _ := uncommentLine(lines[i], "  ")
		if strings.TrimSpace(lines[i]) == "" || !strings.HasPrefix(body, "  ") {
			end = i
			break
		}
	}
	// Модуль уже есть в разделе — его строки дописываются под ним.
	at, moduleActive := -1, false
	for i := header + 1; i < end; i++ {
		body, commented := uncommentLine(lines[i], "  ")
		if body == "  "+module+":" {
			at, moduleActive = i, !commented
			break
		}
	}
	// mark — строка под заголовком: под заданным — # после отступа (у
	// пояснения он уже свой), под закомментированным — # в начале строки.
	mark := func(indent string, parentActive bool, rest string) string {
		switch {
		case parentActive && strings.HasPrefix(rest, "# "):
			return indent + rest
		case parentActive:
			return indent + "# " + rest
		}
		return "# " + indent + rest
	}
	var block []string
	if at < 0 {
		block = append(block, mark("  ", active, "# "+title))
		block = append(block, mark("  ", active, module+":"))
	}
	// Строки настроек: модуль задан — # после отступа; задан только раздел
	// modules — # после отступа модуля; не задано ничего — # в начале.
	child := func(rest string) string {
		switch {
		case moduleActive:
			return mark("    ", true, rest)
		case active:
			return "  # " + "  " + rest
		}
		return "# " + "    " + rest
	}
	for _, e := range entries {
		leaf := e.key[strings.LastIndex(e.key, ".")+1:]
		block = append(block, child("# "+e.comment))
		block = append(block, child(leaf+": "+string(e.example.(rawYAML))))
	}
	// Новая версия модуля — новая версия и в заголовке блока.
	if at > header {
		if _, content, ok := templateCommentLine(lines[at-1]); ok && strings.HasPrefix(content, module+" ") {
			lines[at-1] = strings.TrimSuffix(lines[at-1], content) + title
		}
	}
	pos := end
	if at >= 0 {
		pos = at + 1
		for pos < end {
			body, _ := uncommentLine(lines[pos], "    ")
			if !strings.HasPrefix(body, "    ") {
				break
			}
			pos++
		}
	}
	lines = append(lines[:pos], append(block, lines[pos:]...)...)
	return strings.Join(lines, "\n") + "\n"
}

// templateHasKey — в конфиге есть строка этой настройки модуля
// (modules.<модуль>.<ключ>): заданная или закомментированная по шаблону.
func templateHasKey(text, key string) bool {
	if _, ok := flattenYAML(text)[key]; ok {
		return true
	}
	parts := strings.Split(key, ".")
	if len(parts) != 3 {
		return false
	}
	inModule := false
	for _, line := range strings.Split(text, "\n") {
		indent, content, ok := templateLine(line)
		if !ok {
			continue
		}
		switch {
		case indent == 2 && content == parts[1]+":":
			inModule = true
		case inModule && indent == 4 && strings.HasPrefix(content, parts[2]+":"):
			return true
		case indent < 4:
			inModule = false
		}
	}
	return false
}

// templateLine — строка конфига без # шаблона: отступ и содержимое. ok —
// это настройка или заголовок, а не пояснение и не пустая строка.
func templateLine(line string) (indent int, content string, ok bool) {
	if strings.TrimSpace(line) == "" {
		return 0, "", false
	}
	if m := commentedLine.FindStringSubmatch(line); m != nil {
		if strings.HasPrefix(m[3], "#") {
			return 0, "", false
		}
		return len(m[1]) + len(m[2]), m[3], true
	}
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return 0, "", false
	}
	return leadingSpaces(line), strings.TrimSpace(line), true
}

// flattenYAML — заданные настройки текста через точку.
func flattenYAML(text string) map[string]any {
	out := map[string]any{}
	tree, err := parseYAML(text)
	if err != nil {
		return out
	}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			if sub, ok := v.(map[string]any); ok {
				walk(key, sub)
				continue
			}
			out[key] = v
		}
	}
	walk("", tree)
	return out
}

// moduleSettingsLines — строки о настройках модуля для !!install и
// inspect: ключ, значение сейчас и пояснение.
func moduleSettingsLines(info pluginhost.ModuleInfo) []string {
	settings := append([]pluginhost.Setting(nil), info.Settings...)
	sort.SliceStable(settings, func(i, j int) bool { return settings[i].Key < settings[j].Key })
	var lines []string
	for _, s := range settings {
		key := moduleSettingKey(info.Name, s.Key)
		value := settingText(viper.Get(key))
		if value == "" {
			value = `""`
		}
		line := fmt.Sprintf("%s = %s", key, value)
		if s.Description != "" {
			line += " — " + s.Description
		}
		if s.Required {
			line += i18n.T(" (обязательно)")
		}
		lines = append(lines, line)
	}
	return lines
}

// templateCommentLine — пояснение в строке конфига: всё до текста и текст.
func templateCommentLine(line string) (prefix, content string, ok bool) {
	m := templateComment.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}
