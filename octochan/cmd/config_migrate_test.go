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
