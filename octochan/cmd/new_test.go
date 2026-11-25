package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ochan new module создаёт модуль, который собирается и ставится; занятый
// каталог не трогает; неверное имя — отказ.
func TestNewModule(t *testing.T) {
	if testing.Short() {
		t.Skip("собирает модуль")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("нет компилятора go")
	}
	src, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// Кеш модулей — обычный, до того как тест подменит HOME: иначе go
	// скачивал бы всё во временный каталог, а файлы кеша только для
	// чтения — и удалить каталог теста в конце нельзя.
	modcache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	e := newScriptEnv(t)
	dir := e.path("mymod")
	out := e.must("new", "module", "mymod", "--dir", dir, "--src", src)
	if !strings.Contains(out, "Заготовка модуля mymod") || strings.Contains(out, "путь угадан") {
		t.Fatalf("new module:\n%s", out)
	}
	gomod := readFile(t, filepath.Join(dir, "go.mod"))
	if !strings.Contains(gomod, "module mymod") || !strings.Contains(gomod, "replace ochan => "+filepath.ToSlash(src)) {
		t.Fatalf("go.mod:\n%s", gomod)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-o", "mymod.hcplugin", "."}} {
		c := exec.Command("go", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GOMODCACHE="+strings.TrimSpace(string(modcache)))
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
	out = e.must("inspect", filepath.Join(dir, "mymod.hcplugin"))
	if !strings.Contains(out, "mymod hello") || !strings.Contains(out, "класс read") || !strings.Contains(out, "modules.mymod.greeting") {
		t.Fatalf("inspect заготовки:\n%s", out)
	}
	e.fails("уже есть и не пуст", "new", "module", "mymod", "--dir", dir)
	e.fails("буквы, цифры", "new", "module", "../x")
	if _, err := os.Stat(e.path("x")); err == nil {
		t.Fatal("неверное имя создало каталог")
	}
}

// Заготовка не указывает на старые исходники: в них нет
// pluginhost.Settings и Token, и make упал бы с undefined. Вне дерева
// исходников путь берётся из сборки (SourceDir), а не угадывается.
func TestNewModuleSource(t *testing.T) {
	src, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	e := newScriptEnv(t) // и переходит в свой каталог: исходников вокруг нет
	old := e.path("old-octochan")
	if err := os.MkdirAll(filepath.Join(old, "modules", "pluginhost"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(old, "go.mod"), "module ochan\n\ngo 1.26.0\n")
	e.write(filepath.Join(old, "modules", "pluginhost", "modulemanager.go"), "package pluginhost\n")
	e.fails("старше этого ochan", "new", "module", "m1", "--dir", e.path("m1"), "--src", old)
	if _, err := os.Stat(e.path("m1")); err == nil {
		t.Fatal("отказ создал каталог")
	}

	prev := SourceDir
	t.Cleanup(func() { SourceDir = prev })
	SourceDir = src
	out := e.must("new", "module", "m2", "--dir", e.path("m2"))
	if strings.Contains(out, "путь угадан") {
		t.Fatalf("путь угадан, хотя сборка знает исходники:\n%s", out)
	}
	if gomod := readFile(t, filepath.Join(e.path("m2"), "go.mod")); !strings.Contains(gomod, "replace ochan => "+filepath.ToSlash(src)) {
		t.Fatalf("go.mod:\n%s", gomod)
	}

	// Исходники неизвестны и угаданный путь пуст — заготовка пишется, но
	// с предупреждением, что make её не соберёт.
	SourceDir = ""
	out = e.must("new", "module", "m3", "--dir", e.path("m3"))
	if !strings.Contains(out, "путь угадан") || !strings.Contains(out, "make не соберёт") {
		t.Fatalf("new module без исходников:\n%s", out)
	}
}
