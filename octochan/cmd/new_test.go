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
