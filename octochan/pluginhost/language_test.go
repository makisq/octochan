package pluginhost

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// Модуль языка отдаёт словарь по gRPC: собираем пример, ставим как
// доверенный и загружаем так же, как ochan при запуске.
func TestLanguageModuleOverGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("собирает модуль")
	}
	dir := t.TempDir()
	module := filepath.Join(dir, "lang-de"+ModuleExtension)
	build := exec.Command("go", "build", "-o", module, "octochan/examples/langmodule")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка модуля: %v\n%s", err, out)
	}
	if err := WriteChecksum(module); err != nil {
		t.Fatal(err)
	}

	mm := NewModuleManager()
	defer mm.Cleanup()
	if err := mm.LoadModule(module); err != nil {
		t.Fatal(err)
	}

	languages := mm.Languages()
	if len(languages) != 1 {
		t.Fatalf("языки: %+v", languages)
	}
	de := languages[0]
	if de.Code != "de" || de.Name != "Deutsch" || de.Module != "lang-de" {
		t.Fatalf("язык: %+v", de)
	}
	if de.Entries["Согласовать заявку"] != "Antrag genehmigen" {
		t.Fatalf("словарь: %v", de.Entries)
	}
	if commands := mm.GetCommands(); len(commands) != 0 {
		t.Fatalf("у модуля языка не должно быть команд: %d", len(commands))
	}
}

// Модуль команд без языка отвечает пустым списком.
func TestCommandModuleWithoutLanguage(t *testing.T) {
	resp, err := (&CommandGRPCServer{Impl: plainModule{}}).GetTranslations(context.Background(), &Empty{})
	if err != nil || len(resp.GetLanguages()) != 0 {
		t.Fatalf("ответ %v, ошибка %v", resp, err)
	}
}

type plainModule struct{}

func (plainModule) GetCommands() []*cobra.Command { return nil }
func (plainModule) Init(map[string]any) error     { return nil }
func (plainModule) Name() string                  { return "plain" }
func (plainModule) Version() string               { return "1" }
