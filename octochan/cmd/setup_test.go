package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ochan/workspace"
)

func programName() string {
	if runtime.GOOS == "windows" {
		return "ochan.exe"
	}
	return "ochan"
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// pointerPath — путь из указателя: строка без комментариев.
func pointerPath(t *testing.T, dir string) string {
	t.Helper()
	for _, line := range strings.Split(readText(t, filepath.Join(dir, workspace.HomeFile)), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	t.Fatalf("в указателе %s нет пути", dir)
	return ""
}

// setupEnv — дом теста и каталоги по умолчанию в нём: установка не должна
// тронуть настоящие ~/.local/bin и %LOCALAPPDATA%.
func setupEnv(t *testing.T) *scriptEnv {
	t.Helper()
	e := newScriptEnv(t)
	t.Setenv("OCHAN_TEST_AS_OCHAN", "1")
	t.Setenv("LOCALAPPDATA", filepath.Join(e.home, "AppData", "Local"))
	return e
}

func (e *scriptEnv) answers(text string) {
	rootCmd.SetIn(strings.NewReader(text))
	e.t.Cleanup(func() { rootCmd.SetIn(nil) })
}

// Быстрая установка: программа, рабочий каталог и указатель — в одной
// папке; установленный ochan находит рабочий каталог сам, без окружения.
func TestSetupHere(t *testing.T) {
	e := setupEnv(t)
	here := filepath.Join(t.TempDir(), "work")
	out := e.must("setup", "--here="+here)
	home := filepath.Join(here, ".octochan")
	for _, want := range []string{"Установлено: " + filepath.Join(here, programName()), "Указатель: ", home,
		workspace.SourceHomeFile, "Запуск: "} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	if got := pointerPath(t, here); got != ".octochan" {
		t.Fatalf("указатель быстрой установки — путь от папки программы, а записано %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); err != nil {
		t.Fatalf("установленный ochan не создал конфиг в рабочем каталоге: %v", err)
	}
	// Папку переносят целиком: указатель относительный, ochan снова находит своё.
	moved := filepath.Join(filepath.Dir(here), "moved")
	if err := os.Rename(here, moved); err != nil {
		t.Skipf("папку не переименовать: %v", err)
	}
	c := exec.Command(filepath.Join(moved, programName()), "paths")
	c.Env = append(os.Environ(), "OCTOCHAN_CONFIG=", "OCTOCHAN_HOME=")
	got, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(got), filepath.Join(moved, ".octochan")) {
		t.Fatalf("после переноса папки: %v\n%s", err, got)
	}

	e.fails("не задают --bin", "setup", "--here", "--bin", here)
}

// Ручная установка флагами: каждый путь отдельно; рабочий каталог — в
// указателе, вынесенные каталоги — в config.yaml.
func TestSetupManualFlags(t *testing.T) {
	e := setupEnv(t)
	root := t.TempDir()
	bin, home, modules := filepath.Join(root, "bin"), filepath.Join(root, "state"), filepath.Join(root, "exec", "modules")
	out := e.must("setup", "--bin", bin, "--home", home, "--modules", modules)
	if got := pointerPath(t, bin); got != home {
		t.Fatalf("указатель: %q, ожидался %q", got, home)
	}
	if config := readText(t, filepath.Join(home, "config.yaml")); !strings.Contains(config, "modules: "+modules) &&
		!strings.Contains(config, strings.ReplaceAll(modules, `\`, `\\`)) {
		t.Fatalf("в config.yaml нет paths.modules:\n%s", config)
	}
	for _, want := range []string{"config.yaml: paths.modules: " + modules, "config.yaml paths.modules", "Каталога " + bin + " нет в PATH"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	if info, err := os.Stat(modules); err != nil || !info.IsDir() {
		t.Fatalf("каталог модулей не создан: %v", err)
	}

	// Рабочий каталог по умолчанию — указатель не нужен, прежний убирается.
	out = e.must("setup", "--bin", bin)
	if _, err := os.Stat(filepath.Join(bin, workspace.HomeFile)); err == nil || !strings.Contains(out, "Прежний указатель") {
		t.Fatalf("прежний указатель остался:\n%s", out)
	}
}

// Вопросы читаются построчно — и с терминала, и из конвейера; без ответа —
// каталоги по умолчанию.
func TestSetupDialog(t *testing.T) {
	e := setupEnv(t)
	// 1 — всё в текущей папке.
	e.answers("\nтри\n1\n\nда\n")
	out := e.must("setup")
	if !strings.Contains(out, "1 — быстрая: всё в этой папке") || !strings.Contains(out, "1 — быстрая, 2 — ручная") ||
		!strings.Contains(out, "Значок в меню приложений и на рабочем столе? да или нет [нет]") ||
		pointerPath(t, e.work) != ".octochan" {
		t.Fatalf("быстрая установка в разговоре:\n%s", out)
	}

	// 2 — пути по одному: Enter — путь по умолчанию.
	root := t.TempDir()
	bin, logs := filepath.Join(root, "bin"), filepath.Join(root, "logs")
	e.answers("2\n" + bin + "\n\n\n" + logs + "\n\n\n\nда\n")
	out = e.must("setup")
	home := filepath.Join(e.home, ".octochan")
	if !strings.Contains(out, "Рабочий каталог — роли, заявки, журнал аудита, конфиг ["+home+"]") ||
		!strings.Contains(out, "config.yaml: paths.logs: "+logs) {
		t.Fatalf("ручная установка в разговоре:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(bin, workspace.HomeFile)); err == nil {
		t.Fatal("рабочий каталог по умолчанию, а указатель записан")
	}

	e.answers("2\n" + bin + "\n\n\n\n\n\n\nнет\n")
	e.fails("установка отменена", "setup")

	// Ответить некому: каталоги по умолчанию, как у прежней установки.
	e.answers("")
	out = e.must("setup")
	if !strings.Contains(out, "Ответа нет — установка в каталоги по умолчанию") || !strings.Contains(out, "Установлено: "+filepath.Join(defaultBin(), programName())) {
		t.Fatalf("установка без ответа:\n%s", out)
	}
}

// Установка идёт до рабочего каталога: там, где домашний каталог закрыт,
// ochan setup всё равно работает.
func TestSetupWithoutHome(t *testing.T) {
	e := setupEnv(t)
	blocked := filepath.Join(t.TempDir(), "home-is-a-file")
	e.write(blocked, "x")
	here := filepath.Join(t.TempDir(), "only-here")
	self, _ := os.Executable()
	env := append(os.Environ(), "HOME="+blocked, "USERPROFILE="+blocked, "OCTOCHAN_CONFIG=", "OCTOCHAN_HOME=")

	c := exec.Command(self, "paths")
	c.Env = env
	if out, err := c.CombinedOutput(); err == nil {
		t.Fatalf("дом закрыт, а ochan paths прошла:\n%s", out)
	}
	c = exec.Command(self, "setup", "--here="+here)
	c.Env = env
	out, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(out), filepath.Join(here, ".octochan")) || !strings.Contains(string(out), workspace.SourceHomeFile) {
		t.Fatalf("установка при закрытом доме: %v\n%s", err, out)
	}
	c = exec.Command(self, "setup", "--help")
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil || !strings.Contains(string(out), "--here") {
		t.Fatalf("справка установки: %v\n%s", err, out)
	}
}

// Значок: под Linux — запись меню приложений, картинка и копия на рабочем
// столе; под Windows — ярлык со значком из программы. Убирается так же.
func TestSetupDesktop(t *testing.T) {
	e := setupEnv(t)
	if runtime.GOOS == "windows" {
		// Настоящие меню «Пуск» и рабочий стол тест не трогает.
		dir := t.TempDir()
		self, _ := os.Executable()
		made, err := windowsShortcuts(self, dir, []string{dir})
		if err != nil || len(made) != 1 || made[0] != filepath.Join(dir, "ochan.lnk") {
			t.Fatalf("ярлык: %v, %v", made, err)
		}
		if info, err := os.Stat(made[0]); err != nil || info.Size() == 0 {
			t.Fatalf("ярлык не записан: %v", err)
		}
		e.fails("only — только значок", "setup", "--desktop=maybe")
		return
	}
	data := filepath.Join(e.home, "data")
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PATH", "") // xdg-user-dir машины теста не спрашивается
	desk := filepath.Join(e.home, "Desktop")
	if err := os.MkdirAll(desk, 0o755); err != nil {
		t.Fatal(err)
	}
	here := filepath.Join(t.TempDir(), "папка с пробелом")
	out := e.must("setup", "--here="+here, "--desktop")
	entry := filepath.Join(data, "applications", "ochan.desktop")
	icon := filepath.Join(data, "icons", "hicolor", "256x256", "apps", "ochan.png")
	text := readText(t, entry)
	for _, want := range []string{"Exec=\"" + filepath.Join(here, "ochan") + "\" shell\n", "Icon=" + icon + "\n", "Terminal=true\n",
		"Path=" + here + "\n", "Name=ochan\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("в записи меню нет %q:\n%s", want, text)
		}
	}
	if !strings.Contains(out, "Значок: "+entry) || readText(t, icon) != string(iconPNG) {
		t.Fatalf("значок не поставлен:\n%s", out)
	}
	if info, err := os.Stat(filepath.Join(desk, "ochan.desktop")); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("запись на рабочем столе должна быть исполняемой: %v", err)
	}

	out = e.must("setup", "--desktop=remove")
	for _, path := range []string{entry, icon, filepath.Join(desk, "ochan.desktop")} {
		if _, err := os.Stat(path); err == nil || !strings.Contains(out, "Убран: "+path) {
			t.Fatalf("значок не убран: %s\n%s", path, out)
		}
	}
	e.fails("only — только значок", "setup", "--desktop=maybe")
}
