package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildOchan собирает ochan с версией version в dir.
func buildOchan(t *testing.T, dir, version string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "ochan"+programExt())
	if data, err := exec.Command("go", "build", "-o", out, "-ldflags", "-X ochan/cmd.Version="+version, "ochan").CombinedOutput(); err != nil {
		t.Fatalf("сборка ochan %s: %v\n%s", version, err, data)
	}
	return out
}

// Обновление ставит новую программу на место старой, прежнюю — с копией
// config.yaml — сжимает в архив, рабочий каталог не трогает; откат
// возвращает программу и конфиг, а текущие тоже архивирует; setup новой
// версии находит установленную и обновляет её.
func TestUpdateAndRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("собирает ochan дважды")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("нет компилятора go")
	}
	root := t.TempDir()
	installed := filepath.Join(root, "work")
	old := buildOchan(t, installed, "1.0.0")
	fresh := buildOchan(t, filepath.Join(root, "download"), "2.0.0")

	home := filepath.Join(installed, ".octochan")
	for _, dir := range []string{filepath.Join(home, "tokens"), filepath.Join(home, "modules")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(installed, "ochan.home"), []byte(".octochan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(config, []byte("defaults:\n  http_timeout: 45s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, "tokens", "demo")
	if err := os.WriteFile(token, []byte("секрет\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{"HOME=" + filepath.Join(root, "user"), "PATH=" + installed + string(os.PathListSeparator) + pathWithoutOchan()}
	run := func(program string, stdin string, args ...string) string {
		t.Helper()
		c := exec.Command(program, args...)
		c.Env, c.Dir = env, root
		c.Stdin = strings.NewReader(stdin)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", program, args, err, out)
		}
		return string(out)
	}
	version := func() string { return strings.TrimSpace(run(old, "", "--version")) }

	out := run(old, "", "update", filepath.Dir(fresh))
	for _, want := range []string{"ochan обновлён: 1.0.0 → 2.0.0", "прежняя версия — архив", "ochan update --rollback"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе update нет %q:\n%s", want, out)
		}
	}
	if v := version(); v != "ochan 2.0.0" {
		t.Fatalf("после обновления: %s", v)
	}
	archives := versionArchives(installed)
	if len(archives) != 1 || !strings.Contains(filepath.Base(archives[0]), "ochan-1.0.0-") {
		t.Fatalf("архивы: %v", archives)
	}
	files, err := readArchive(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["ochan"+programExt()]; !ok || !strings.Contains(string(files[configMember]), "http_timeout: 45s") {
		t.Fatalf("в архиве: %v", keys(files))
	}
	for name := range files {
		if strings.Contains(name, "token") {
			t.Fatalf("в архив попал токен: %s", name)
		}
	}
	if data, _ := os.ReadFile(token); string(data) != "секрет\n" || !strings.Contains(readFile(t, config), "http_timeout: 45s") {
		t.Fatal("обновление тронуло рабочий каталог")
	}

	// Новая версия дописала конфиг — откат вернёт тот, что был у прежней,
	// а нынешний уйдёт в архив 2.0.0.
	if err := os.WriteFile(config, []byte("defaults:\n  http_timeout: 45s\nshell:\n  logo: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = run(old, "", "update", "--rollback")
	if !strings.Contains(out, "ochan возвращён: 2.0.0 → 1.0.0") || !strings.Contains(out, "config.yaml — как был у версии 1.0.0") {
		t.Fatalf("откат:\n%s", out)
	}
	if v := version(); v != "ochan 1.0.0" {
		t.Fatalf("после отката: %s", v)
	}
	if text := readFile(t, config); strings.Contains(text, "logo: false") || !strings.Contains(text, "http_timeout: 45s") {
		t.Fatalf("конфиг после отката:\n%s", text)
	}
	archives = versionArchives(installed)
	if len(archives) != 2 || !strings.Contains(filepath.Base(archives[0]), "ochan-2.0.0-") {
		t.Fatalf("архивы после отката: %v", archives)
	}
	if files, _ := readArchive(archives[0]); !strings.Contains(string(files[configMember]), "logo: false") {
		t.Fatal("конфиг версии 2.0.0 не сохранён в её архиве")
	}
	if out := run(old, "", "update", "--list"); !strings.Contains(out, "ochan-2.0.0-") || !strings.Contains(out, "ochan-1.0.0-") {
		t.Fatalf("update --list:\n%s", out)
	}

	// Новая версия запущена из папки загрузки: setup находит установленную
	// (в PATH) и предлагает обновить.
	out = run(fresh, "да\n", "setup")
	if !strings.Contains(out, "Найден установленный ochan 1.0.0: "+old) || !strings.Contains(out, "ochan обновлён: 1.0.0 → 2.0.0") {
		t.Fatalf("setup поверх установленного:\n%s", out)
	}
	if v := version(); v != "ochan 2.0.0" {
		t.Fatalf("после setup: %s", v)
	}
	if out := run(fresh, "", "setup", "--update="+installed); !strings.Contains(out, "Установлена эта же сборка ochan 2.0.0") {
		t.Fatalf("setup --update той же сборкой:\n%s", out)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
