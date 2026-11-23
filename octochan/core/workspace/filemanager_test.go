package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearPathEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range PathEnv() {
		t.Setenv(name, "")
	}
}

// Рабочий каталог — ~/.octochan или OCTOCHAN_HOME; каталоги узла выносятся
// окружением и конфигом, окружение сильнее; путь — только полный.
func TestFileManagerPaths(t *testing.T) {
	home := t.TempDir()
	clearPathEnv(t, home)

	fm, err := NewFileManager()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".octochan")
	if fm.BaseDir() != base || fm.ModulesDir() != filepath.Join(base, "modules") || fm.Home().Source != SourceDefault ||
		fm.RegistryCacheDir() != filepath.Join(base, "registry", "cache") {
		t.Fatalf("по умолчанию: %s, модули %s", fm.BaseDir(), fm.ModulesDir())
	}
	// Тот же путь, что по умолчанию, — не перенос.
	if err := fm.Relocate(DirLogs, filepath.Join(base, "logs"), "config.yaml defaults.log_path"); err != nil {
		t.Fatal(err)
	}
	reports := filepath.Join(t.TempDir(), "reports")
	if err := fm.Relocate(DirReports, reports, "config.yaml paths.reports"); err != nil {
		t.Fatal(err)
	}
	if err := fm.Relocate(DirModules, "mods", "config.yaml paths.modules"); err == nil || !strings.Contains(err.Error(), "нужен полный путь") {
		t.Fatalf("относительный путь: %v", err)
	}
	if err := fm.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{fm.ModulesDir(), fm.AliasesDir(), fm.RequestsDir(), reports} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("каталог %s не создан: %v", dir, err)
		}
	}
	sources := map[Dir]string{}
	for _, p := range fm.Relocatable() {
		sources[p.Dir] = p.Source
	}
	if fm.ReportsDir() != reports || sources[DirReports] != "config.yaml paths.reports" || sources[DirLogs] != SourceDefault {
		t.Fatalf("перенос конфигом: %s, источники %v", fm.ReportsDir(), sources)
	}

	// Окружение: рабочий каталог целиком и модули отдельно; конфиг их не
	// перебивает.
	moved, modules := filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "modules")
	t.Setenv(EnvHome, moved)
	t.Setenv(DirModules.Env(), modules)
	fm, err = NewFileManager()
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Relocate(DirModules, filepath.Join(t.TempDir(), "other"), "config.yaml paths.modules"); err != nil {
		t.Fatal(err)
	}
	if fm.BaseDir() != moved || fm.ModulesDir() != modules || fm.LogsDir() != filepath.Join(moved, "logs") ||
		fm.Home().Source != "окружение OCTOCHAN_HOME" || fm.StatePath() != filepath.Join(moved, "state.json") {
		t.Fatalf("перенос окружением: %s, модули %s, %s", fm.BaseDir(), fm.ModulesDir(), fm.Home().Source)
	}
	if dir, err := HomeDir(); err != nil || dir != moved {
		t.Fatalf("HomeDir: %s %v", dir, err)
	}
	if err := fm.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, "modules")); err == nil {
		t.Fatal("в рабочем каталоге создан каталог модулей, хотя он вынесен")
	}

	for name, value := range map[string]string{EnvHome: "state", DirModules.Env(): "./mods"} {
		t.Setenv(EnvHome, moved)
		t.Setenv(DirModules.Env(), "")
		t.Setenv(name, value)
		if _, err := NewFileManager(); err == nil || !strings.Contains(err.Error(), "нужен полный путь") {
			t.Fatalf("%s=%s: %v", name, value, err)
		}
	}
}

// Указатель рабочего каталога рядом с программой: одна строка с путём;
// путь без начала от корня — от папки программы, ~ — домашний каталог.
func TestReadHomeFile(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, ok, err := ReadHomeFile(dir); ok || err != nil {
		t.Fatalf("указателя нет: %v, %v", ok, err)
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, HomeFile), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	abs := filepath.Join(t.TempDir(), "state")
	for text, want := range map[string]string{
		"# установка\n\n.octochan\n":            filepath.Join(dir, ".octochan"),
		string(rune(0xFEFF)) + "data/ochan\r\n": filepath.Join(dir, "data", "ochan"),
		abs + "\n":                              abs,
		"~/ochan-home\n":                        filepath.Join(home, "ochan-home"),
		"..\n":                                  filepath.Dir(dir),
	} {
		write(text)
		got, ok, err := ReadHomeFile(dir)
		if err != nil || !ok || got != want {
			t.Fatalf("указатель %q: %q, %v, %v — ожидался %q", text, got, ok, err, want)
		}
	}
	write("# только комментарий\n")
	if _, _, err := ReadHomeFile(dir); err == nil || !strings.Contains(err.Error(), "в указателе нет пути") {
		t.Fatalf("пустой указатель: %v", err)
	}
}
