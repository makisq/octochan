package cmd

import (
	"os"
	"strings"
	"testing"

	"ochan/modules/pluginhost"

	"github.com/spf13/viper"
)

// Модуль объявляет настройки — !!install пишет их в config.yaml сам:
// раздел modules.<модуль>, пояснения, умолчания под #, обязательное без
// значения — без # и пустым. Модуль получает значения в Init; повторная
// установка ничего не дублирует и заданного не трогает.
func TestModuleSettings(t *testing.T) {
	module := buildModule(t, "ochan/examples/classmodule", "pgstat")
	runner := buildModule(t, "ochan/examples/runnermodule", "demo-runner")
	e := newScriptEnv(t)
	t.Cleanup(func() {
		pluginhost.GetModuleManager().Cleanup()
		refreshModuleCommands()
		syncRunners()
	})
	path := os.Getenv("OCTOCHAN_CONFIG")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := initConfig(); err != nil {
		t.Fatal(err)
	}

	out := e.must("!!install", module)
	for _, want := range []string{"настройки записаны в " + path + " (раздел modules.pgstat)",
		"modules.pgstat.host = 127.0.0.1 — сервер PostgreSQL", "modules.pgstat.cluster = \"\" — имя кластера в отчётах pgstat (обязательно)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	text := readFile(t, path)
	for _, want := range []string{"# Настройки модулей — их пишет !!install", "modules:\n  # pgstat 0.1.0\n  pgstat:",
		"    # сервер PostgreSQL, статистику которого показывает pgstat\n    # host: 127.0.0.1", "    # port: 5432",
		"    # имя кластера в отчётах pgstat (обязательно)\n    cluster: \"\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("в config.yaml нет %q:\n%s", want, text)
		}
	}
	if got := flatten(t, text); len(got) != 1 || got["modules.pgstat.cluster"] != "" {
		t.Fatalf("заданное в config.yaml: %v", got)
	}
	if out := e.must("pgstat", "where"); !strings.Contains(out, "pgstat: 127.0.0.1:5432, кластер не задан") {
		t.Fatalf("умолчания не дошли до модуля:\n%s", out)
	}
	if out := e.must("modules"); !strings.Contains(out, "настроек: 3 (modules.pgstat)") {
		t.Fatalf("modules:\n%s", out)
	}
	// defaults.warn_missing_settings: false — о незаданном обязательном молчат.
	info := pluginhost.ModuleInfo{Name: "pgstat", Missing: []string{"modules.pgstat.cluster"}}
	if moduleMissing(info) == "" {
		t.Fatal("по умолчанию о незаданном обязательном не предупредили")
	}
	setting(t, warnMissingKey, false)
	if warning := moduleMissing(info); warning != "" {
		t.Fatalf("warn_missing_settings: false, а предупреждение: %s", warning)
	}
	if out := e.must("!!install", module); strings.Contains(out, "не задано обязательное") {
		t.Fatalf("установка предупредила при warn_missing_settings: false:\n%s", out)
	}
	setting(t, warnMissingKey, true)

	// Пользователь задал значения — модуль получает их после перезагрузки.
	if err := setConfigValues(path, map[string]any{"modules.pgstat.host": "db1.example.internal", "modules.pgstat.cluster": "main"}); err != nil {
		t.Fatal(err)
	}
	if err := viper.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	out = e.must("!!install", module)
	if !strings.Contains(out, "настройки (уже в "+path) || strings.Contains(out, "внимание") {
		t.Fatalf("повторная установка:\n%s", out)
	}
	text = readFile(t, path)
	if strings.Count(text, "  pgstat:") != 1 || strings.Count(text, "    host:") != 1 || !strings.Contains(text, "    host: db1.example.internal") {
		t.Fatalf("повторная установка тронула записанное:\n%s", text)
	}
	if out := e.must("pgstat", "where"); !strings.Contains(out, "pgstat: db1.example.internal:5432, кластер main") {
		t.Fatalf("заданное не дошло до модуля:\n%s", out)
	}

	// Второй модуль — в тот же раздел modules; его настройку получает и
	// исполнитель (status показывает регион).
	e.must("!!install", runner)
	text = readFile(t, path)
	if strings.Count(text, "\nmodules:") != 1 ||
		!strings.Contains(text, "  # demo-runner:\n  #   # регион API демонстрационного исполнителя\n  #   region: eu-1") {
		t.Fatalf("второй модуль в разделе modules:\n%s", text)
	}
	if _, err := parseYAML(text); err != nil {
		t.Fatalf("config.yaml не разбирается: %v\n%s", err, text)
	}
	if strings.Contains(text, "# # ") {
		t.Fatalf("двойной # в разделе modules:\n%s", text)
	}

	// Новая версия модуля с новой настройкой: дописана в его блок, с одним
	// #, версия в заголовке блока — новая, заданное не тронуто.
	data := readFile(t, path)
	data = strings.Replace(data, "  # pgstat 0.1.0", "  # pgstat 0.0.9", 1)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	info = pluginhost.ModuleInfo{Name: "pgstat", Version: "0.1.0", Settings: []pluginhost.Setting{
		{Key: "host"}, {Key: "refresh", Default: "30s", Description: "как часто обновлять статистику"}}}
	if added, err := declareModuleSettings(path, info); err != nil || len(added) != 1 || added[0] != "modules.pgstat.refresh" {
		t.Fatalf("новая настройка новой версии: %v %v", added, err)
	}
	text = readFile(t, path)
	for _, want := range []string{"  # pgstat 0.1.0\n  pgstat:", "    # как часто обновлять статистику\n    # refresh: 30s",
		"    host: db1.example.internal"} {
		if !strings.Contains(text, want) {
			t.Fatalf("после новой версии нет %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "# # ") || strings.Contains(text, "0.0.9") {
		t.Fatalf("блок модуля после новой версии:\n%s", text)
	}
	if out := e.must("status", "--executor", "demo", "demo-1"); !strings.Contains(out, "Регион: eu-1") {
		t.Fatalf("настройка модуля не дошла до исполнителя:\n%s", out)
	}
	setting(t, "executors", map[string]any{"demo": map[string]any{"region": "us-2"}})
	if out := e.must("status", "--executor", "demo", "demo-1"); !strings.Contains(out, "Регион: us-2") {
		t.Fatalf("executors.demo должен быть сильнее modules.demo-runner:\n%s", out)
	}
}
