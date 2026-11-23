package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// flatten — заданные настройки файла через точку.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func flatten(t *testing.T, text string) map[string]any {
	t.Helper()
	tree, err := parseYAML(text)
	if err != nil {
		t.Fatalf("шаблон не разбирается: %v\n%s", err, text)
	}
	out := map[string]any{}
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

// Новый конфиг — шаблон с пояснениями: в нём ничего не задано, каждая
// настройка шаблона — настоящая настройка ochan, а снятый # даёт ровно её.
func TestConfigTemplate(t *testing.T) {
	newScriptEnv(t)
	known := map[string]bool{}
	for _, k := range viper.AllKeys() {
		known[k] = true
	}
	all := map[string]any{}
	for _, section := range configSections {
		for _, entry := range section.entries {
			if entry.example == nil && !known[entry.key] {
				t.Errorf("в шаблоне %s, а такой настройки у ochan нет", entry.key)
			}
			if entry.comment == "" {
				t.Errorf("у %s нет пояснения", entry.key)
			}
			all[entry.key] = "x"
		}
	}

	empty, err := renderConfigTemplate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := flatten(t, empty); len(got) != 0 {
		t.Fatalf("в пустом шаблоне что-то задано: %v", got)
	}
	if !strings.HasPrefix(empty, "# config.yaml ochan.") || !strings.Contains(empty, "docs/reference/config.md") {
		t.Fatalf("шапка шаблона:\n%s", empty)
	}

	full, err := renderConfigTemplate(all)
	if err != nil {
		t.Fatal(err)
	}
	got := flatten(t, full)
	for key := range all {
		if got[key] != "x" {
			t.Fatalf("%s не задан в шаблоне со всеми настройками:\n%s", key, full)
		}
	}

	// Заданное и его раздел — без #, остальное раздела — с # после отступа.
	some, err := renderConfigTemplate(map[string]any{"llm.chat.root": []string{"work"}, "unknown.key": 1})
	if err != nil {
		t.Fatal(err)
	}
	got = flatten(t, some)
	if len(got) != 2 || got["unknown.key"] != 1 || !strings.Contains(some, "llm:\n") ||
		!strings.Contains(some, "    root: [work]") || !strings.Contains(some, "  # mode: \"\"") {
		t.Fatalf("шаблон с заданным: %v\n%s", got, some)
	}
}

// Настройка ставится в свою строку шаблона: пояснения и правки руками
// остаются; чего в шаблоне нет — встаёт под свой раздел; конфиг не по
// шаблону правится разбором YAML.
func TestSetConfigValues(t *testing.T) {
	e := newScriptEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeConfigTemplate(path, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	// Правка руками: свой комментарий.
	if err := os.WriteFile(path, append([]byte("# моё: не трогать\n"), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	set := map[string]any{"defaults.token_timer_enabled": true, "llm.chat.provider": "anthropic",
		"defaults.token_lifetime": "1h", "executors.ssh.port": 22}
	if err := setConfigValues(path, set); err != nil {
		t.Fatal(err)
	}
	text := readFile(t, path)
	got := flatten(t, text)
	for key, want := range set {
		if got[key] != want {
			t.Fatalf("%s = %v, ожидалось %v\n%s", key, got[key], want, text)
		}
	}
	for _, want := range []string{"# моё: не трогать", "# config.yaml ochan.", "  # http_timeout: 30s",
		"    provider: anthropic", "# Песочница ОС для --sandbox"} {
		if !strings.Contains(text, want) {
			t.Fatalf("после правки нет %q:\n%s", want, text)
		}
	}
	// Второй раз — то же значение на месте, без второй строки.
	if err := setConfigValues(path, map[string]any{"defaults.token_timer_enabled": false}); err != nil {
		t.Fatal(err)
	}
	if text = readFile(t, path); strings.Count(text, "token_timer_enabled:") != 1 || flatten(t, text)["defaults.token_timer_enabled"] != false {
		t.Fatalf("повторная правка:\n%s", text)
	}

	// Конфиг не по шаблону.
	foreign := filepath.Join(t.TempDir(), "config.yaml")
	e.write(foreign, "# свой конфиг\nlang: eng\n")
	if err := setConfigValues(foreign, map[string]any{"defaults.token_timer_enabled": true}); err != nil {
		t.Fatal(err)
	}
	if text = readFile(t, foreign); !strings.Contains(text, "# свой конфиг") || flatten(t, text)["lang"] != "eng" ||
		flatten(t, text)["defaults.token_timer_enabled"] != true {
		t.Fatalf("конфиг не по шаблону:\n%s", text)
	}
}

// ochan создаёт новый конфиг шаблоном — без выгрузки умолчаний и путей
// рабочего каталога; timer on меняет в нём одну строку.
func TestNewConfigIsTemplate(t *testing.T) {
	e := newScriptEnv(t)
	path := os.Getenv("OCTOCHAN_CONFIG")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := initConfig(); err != nil {
		t.Fatal(err)
	}
	text := readFile(t, path)
	if !strings.HasPrefix(text, "# config.yaml ochan.") || len(flatten(t, text)) != 0 || strings.Contains(text, e.home) {
		t.Fatalf("новый конфиг:\n%s", text)
	}
	e.must("timer", "on")
	t.Cleanup(func() { viper.Set("defaults.token_timer_enabled", false) })
	if got := flatten(t, readFile(t, path)); len(got) != 1 || got["defaults.token_timer_enabled"] != true {
		t.Fatalf("после timer on: %v\n%s", got, readFile(t, path))
	}
}

// shell.goodbye: по умолчанию exit прощается, false — молча; на языке
// ochan.
func TestShellGoodbye(t *testing.T) {
	newScriptEnv(t)
	if got := goodbye(); got != "До встречи!" {
		t.Fatalf("по умолчанию: %q", got)
	}
	setting(t, "shell.goodbye", false)
	if got := goodbye(); got != "" {
		t.Fatalf("shell.goodbye: false, а прощание %q", got)
	}
}

// Сменили lang — сменились пояснения config.yaml, а значения, свои
// комментарии пользователя и текст автора модуля остались; обратно — тот
// же файл байт в байт.
func TestConfigFollowsLanguage(t *testing.T) {
	newScriptEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeConfigTemplate(path, nil); err != nil {
		t.Fatal(err)
	}
	if err := setConfigValues(path, map[string]any{"lang": "eng", "shell.goodbye": false, "llm.chat.provider": "anthropic"}); err != nil {
		t.Fatal(err)
	}
	module := "\n# " + templateText(modulesTitle) + "\nmodules:\n  # demo 1.0\n  demo:\n    # текст автора модуля" +
		templateText(requiredSuffix) + "\n    url: x\n"
	data := readFile(t, path) + module + "# свой комментарий\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	ru := readFile(t, path)

	english, ok := retranslateConfig(ru, "en")
	if !ok {
		t.Fatal("конфиг по шаблону не узнан")
	}
	for _, want := range []string{"# ochan config.yaml.", "# Shell", "  # say goodbye on exit", "  goodbye: false",
		"lang: eng", "    provider: anthropic", "# Module settings — written by !!install", "    # текст автора модуля (required)",
		"# свой комментарий"} {
		if !strings.Contains(english, want) {
			t.Fatalf("после перевода нет %q:\n%s", want, english)
		}
	}
	if strings.Contains(english, "Оболочка") || strings.Contains(english, "(обязательно)") {
		t.Fatalf("остались русские пояснения:\n%s", english)
	}
	if !reflect.DeepEqual(flatten(t, ru), flatten(t, english)) {
		t.Fatalf("перевод изменил настройки:\n%v\n%v", flatten(t, ru), flatten(t, english))
	}
	back, _ := retranslateConfig(english, "ru")
	if back != ru {
		t.Fatalf("обратный перевод — не тот же файл:\n%s", back)
	}
	if _, ok := retranslateConfig("# свой конфиг\nlang: eng\n", "en"); ok {
		t.Fatal("конфиг не по шаблону принят за шаблон")
	}
}
