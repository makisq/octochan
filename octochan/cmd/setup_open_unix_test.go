//go:build unix

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ochan/workspace"
)

// sharedFolder — папка, открытая для записи группе (0770): общая папка, где
// chmod владельцу установки недоступен или не нужен.
func sharedFolder(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Режим ставится явно: umask его не сузит.
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	return dir
}

// installedPaths — ochan paths установленной программой: вывод и stderr.
func installedPaths(t *testing.T, dir string, env ...string) (stdout, stderr string) {
	t.Helper()
	c := exec.Command(filepath.Join(dir, programName()), "paths")
	c.Env = append(append(os.Environ(), "OCTOCHAN_CONFIG=", "OCTOCHAN_HOME=", workspace.EnvWarned+"="), env...)
	var out, errOut strings.Builder
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		t.Fatalf("установленный ochan в %s не запустился: %v\n%s%s", dir, err, out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

// Папка, открытая для записи группе, — не отказ: установка предупреждает и
// спрашивает, продолжить ли и показывать ли предупреждение дальше.
func TestSetupOpenFolder(t *testing.T) {
	e := setupEnv(t)
	const (
		warning  = "Предупреждение: Рабочий каталог: каталог "
		proceed  = "Продолжить установку? да или нет [да]"
		noMore   = "Больше не показывать это предупреждение? да или нет [нет]"
		atLaunch = "предупреждение: " + workspace.HomeFile + ": каталог "
	)

	// «Продолжить» и «больше не показывать»: ответ записан в config.yaml,
	// установленный ochan работает и не предупреждает.
	quiet := sharedFolder(t, "quiet")
	e.answers("да\nда\n")
	out := e.must("setup", "--here="+quiet)
	for _, want := range []string{warning + quiet + " открыт для записи группе или остальным (-rwxrwx---)",
		"chmod go-w " + quiet, proceed, noMore, "config.yaml: paths.warn_open: false",
		"Открыты для чужой записи:", "предупреждение при запуске выключено (paths.warn_open: false)", "Запуск: "} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Установить? да или нет") {
		t.Fatalf("подтверждение спрошено дважды:\n%s", out)
	}
	if config := readText(t, filepath.Join(quiet, ".octochan", "config.yaml")); !strings.Contains(config, "warn_open: false") {
		t.Fatalf("в config.yaml нет paths.warn_open: false:\n%s", config)
	}
	stdout, stderr := installedPaths(t, quiet)
	if stderr != "" || !strings.Contains(stdout, "Открыты для чужой записи:") {
		t.Fatalf("после «больше не показывать»: stderr %q\n%s", stderr, stdout)
	}
	// Повторная установка туда же уже не спрашивает.
	e.answers("")
	if out = e.must("setup", "--here="+quiet); strings.Contains(out, "Предупреждение:") || strings.Contains(out, proceed) {
		t.Fatalf("предупреждение выключено, а установка спросила снова:\n%s", out)
	}

	// «Продолжить», но показывать дальше: ochan работает и предупреждает при
	// каждом запуске; потомок предупреждение не повторяет.
	loud := sharedFolder(t, "loud")
	e.answers("да\nнет\n")
	out = e.must("setup", "--here="+loud)
	if !strings.Contains(out, proceed) || !strings.Contains(out, noMore) || strings.Contains(out, "paths.warn_open: false —") ||
		!strings.Contains(out, "ochan предупреждает об этом при запуске") {
		t.Fatalf("установка с предупреждением дальше:\n%s", out)
	}
	// Проверка установленной программой предупреждение не повторила.
	if strings.Contains(out, atLaunch) {
		t.Fatalf("установка показала предупреждение дважды:\n%s", out)
	}
	if _, stderr = installedPaths(t, loud); !strings.Contains(stderr, atLaunch+loud) || !strings.Contains(stderr, "paths.warn_open: false") {
		t.Fatalf("при запуске нет предупреждения: %q", stderr)
	}
	if _, stderr = installedPaths(t, loud, workspace.EnvWarned+"=1"); stderr != "" {
		t.Fatalf("потомок повторил предупреждение: %q", stderr)
	}

	// «Не продолжать» — отмена: ничего не записано.
	cancelled := sharedFolder(t, "cancelled")
	e.answers("нет\n")
	e.fails("установка отменена", "setup", "--here="+cancelled)
	if entries, _ := os.ReadDir(cancelled); len(entries) != 0 {
		t.Fatalf("установка отменена, а в папке %d файлов", len(entries))
	}

	// Ответить некому (CI): предупреждение — и установка идёт.
	unattended := sharedFolder(t, "unattended")
	e.answers("")
	if out = e.must("setup", "--here="+unattended); !strings.Contains(out, warning+unattended) || !strings.Contains(out, "Запуск: ") {
		t.Fatalf("установка без ответа:\n%s", out)
	}
	// --yes: предупреждение без вопросов.
	yes := sharedFolder(t, "yes")
	if out = e.must("setup", "--here="+yes, "--yes"); !strings.Contains(out, warning+yes) || strings.Contains(out, proceed) {
		t.Fatalf("установка с --yes:\n%s", out)
	}

	// Вынесенный каталог в открытой папке — то же предупреждение; рабочий
	// каталог по умолчанию не проверяется.
	modules := filepath.Join(sharedFolder(t, "exec"), "modules")
	e.answers("да\nнет\n")
	out = e.must("setup", "--bin", filepath.Join(t.TempDir(), "bin"), "--modules", modules)
	if !strings.Contains(out, "Предупреждение: Модули: каталог "+filepath.Dir(modules)) || strings.Contains(out, warning) {
		t.Fatalf("вынесенные модули в открытой папке:\n%s", out)
	}
}
