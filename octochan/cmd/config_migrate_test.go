package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// Значение переименованной настройки переходит на новый ключ сам, один
// раз; прежняя строка остаётся для версии, к которой откатятся.
func TestMigrateConfig(t *testing.T) {
	e := newScriptEnv(t)
	logs := e.path("logs")
	path := viper.ConfigFileUsed()
	e.write(path, "defaults:\n  log_path: "+logs+"\n  http_timeout: 45s\n")
	if err := viper.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	notes, err := migrateConfig(path)
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "defaults.log_path перенесено в paths.logs") {
		t.Fatalf("перенос: %v, %v", notes, err)
	}
	text := readFile(t, path)
	if !strings.Contains(text, "log_path: "+logs) || !strings.Contains(text, "logs: "+logs) || !strings.Contains(text, "http_timeout: 45s") {
		t.Fatalf("config.yaml после переноса:\n%s", text)
	}
	if viper.GetString("paths.logs") != logs {
		t.Fatalf("paths.logs = %q", viper.GetString("paths.logs"))
	}
	if notes, err := migrateConfig(path); err != nil || len(notes) != 0 {
		t.Fatalf("второй перенос: %v, %v", notes, err)
	}
}

// Конфиг 0.3.0: обязательная настройка модуля записана дважды — заданной
// под заголовком и закомментированной в блоке. При запуске остаётся одна
// строка, на месте копии и с прежним значением; свои закомментированные
// строки пользователя не трогаются.
func TestDedupeModuleSettings(t *testing.T) {
	e := newScriptEnv(t)
	path := viper.ConfigFileUsed()
	old := "defaults:\n  http_timeout: 45s\n\n" +
		"modules:\n" +
		"  # pgstat 1.2.0\n" +
		"  pgstat:\n" +
		"    cluster: main\n" +
		"    # сервер PostgreSQL, статистику которого показывает pgstat\n" +
		"    # host: 127.0.0.1\n" +
		"    # порт PostgreSQL\n" +
		"    # port: 5432\n" +
		"    # имя кластера в отчётах pgstat (обязательно)\n" +
		"    # cluster: \"\"\n" +
		"  other:\n" +
		"    host: db2\n" +
		"    # запасной узел\n" +
		"    # host: \"\"\n"
	e.write(path, old)
	if err := viper.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	notes, err := migrateConfig(path)
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "modules.pgstat.cluster") {
		t.Fatalf("исправление: %v, %v", notes, err)
	}
	want := "defaults:\n  http_timeout: 45s\n\n" +
		"modules:\n" +
		"  # pgstat 1.2.0\n" +
		"  pgstat:\n" +
		"    # сервер PostgreSQL, статистику которого показывает pgstat\n" +
		"    # host: 127.0.0.1\n" +
		"    # порт PostgreSQL\n" +
		"    # port: 5432\n" +
		"    # имя кластера в отчётах pgstat (обязательно)\n" +
		"    cluster: main\n" +
		"  other:\n" +
		"    host: db2\n" +
		"    # запасной узел\n" +
		"    # host: \"\"\n"
	if text := readFile(t, path); text != want {
		t.Fatalf("config.yaml после исправления:\n%s", text)
	}
	if viper.GetString("modules.pgstat.cluster") != "main" {
		t.Fatalf("modules.pgstat.cluster = %q", viper.GetString("modules.pgstat.cluster"))
	}
	if notes, err := migrateConfig(path); err != nil || len(notes) != 0 {
		t.Fatalf("второй запуск: %v, %v", notes, err)
	}
}
