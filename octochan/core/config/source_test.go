package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceScheme(t *testing.T) {
	cases := map[string]string{
		"postgres://user@host:5432/db": "postgres",
		"POSTGRESQL://host/db":         "postgresql",
		"ssh://host/etc/nginx.conf":    "ssh",

		// Обычные пути схемой не являются.
		"prod.conf":               "",
		"./configs/prod.conf":     "",
		"/etc/postgresql/pg.conf": "",
		`C:\configs\prod.conf`:    "",
		"-":                       "",
		"":                        "",
		"postgres://":             "",
		"://host":                 "",
		// Схема обязана начинаться с буквы (RFC 3986).
		"1://host": "",
	}

	for ref, want := range cases {
		if got := SourceScheme(ref); got != want {
			t.Errorf("SourceScheme(%q) = %q, ожидалось %q", ref, got, want)
		}
	}
}

func TestSourceSchemeIgnoresWindowsDriveLetter(t *testing.T) {
	// Одиночная буква перед двоеточием — это диск, а не схема.
	if got := SourceScheme(`C:\Users\dba\prod.conf`); got != "" {
		t.Errorf("путь Windows принят за схему %q", got)
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prod.conf")
	if err := os.WriteFile(path, []byte("max_connections = 100\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadConfigFrom(context.Background(), path, SourceOptions{})
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if got := loaded.Data[""]["max_connections"]; got != "100" {
		t.Errorf("max_connections = %q", got)
	}
	if loaded.Name != path {
		t.Errorf("имя источника %q, ожидалось %q", loaded.Name, path)
	}
}

func TestLoadConfigFromUnknownScheme(t *testing.T) {
	_, err := LoadConfigFrom(context.Background(), "redis://host/1", SourceOptions{})
	if err == nil {
		t.Fatal("незарегистрированная схема загрузилась")
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Errorf("в сообщении нет имени схемы: %v", err)
	}
}

func TestRegisterSourceScheme(t *testing.T) {
	// Реестр схем — то, что позволяет core не знать про конкретные
	// источники: движок сравнения одинаково работает с файлом, СУБД
	// или чем угодно ещё, что зарегистрирует свою схему.
	const scheme = "testsource"

	RegisterSourceScheme(scheme, func(_ context.Context, ref string, opts SourceOptions) (*LoadedConfig, error) {
		if opts.IncludeDefaults {
			return &LoadedConfig{
				Name:  ref,
				Data:  ConfigMap{"": {"a": "1", "b": "по умолчанию"}},
				Notes: []string{"замечание источника"},
			}, nil
		}
		return &LoadedConfig{Name: ref, Data: ConfigMap{"": {"a": "1"}}}, nil
	})

	loaded, err := LoadConfigFrom(context.Background(), scheme+"://где-то", SourceOptions{})
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if len(loaded.Data[""]) != 1 {
		t.Errorf("без IncludeDefaults получено %d параметров", len(loaded.Data[""]))
	}

	loaded, err = LoadConfigFrom(context.Background(), scheme+"://где-то", SourceOptions{IncludeDefaults: true})
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if len(loaded.Data[""]) != 2 {
		t.Errorf("с IncludeDefaults получено %d параметров", len(loaded.Data[""]))
	}
	if len(loaded.Notes) != 1 {
		t.Errorf("замечания источника потерялись: %v", loaded.Notes)
	}
}

func TestLoadConfigFromMissingFile(t *testing.T) {
	_, err := LoadConfigFrom(context.Background(), filepath.Join(t.TempDir(), "нет.conf"), SourceOptions{})
	if err == nil {
		t.Fatal("отсутствующий файл загрузился")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ошибка не оборачивает os.ErrNotExist: %v", err)
	}
}
