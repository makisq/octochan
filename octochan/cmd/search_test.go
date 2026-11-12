package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// search находит файл по имени и показывает его, несколько — списком;
// ищет по тексту и по заявкам; роль с областью видит только её.
func TestSearch(t *testing.T) {
	e := newScriptEnv(t)
	for path, text := range map[string]string{
		"plans/work_mem.yaml":      "service: postgres\nset:\n  work_mem: 32MB\n",
		"plans/old/work_mem.yaml":  "service: postgres\nset:\n  work_mem: 16MB\n",
		"notes.txt":                "проверить work_mem на server1\nи на server2\n",
		".hidden/secret.yaml":      "token: x\n",
		"node_modules/pkg/a.yaml":  "x: 1\n",
		"binary.dat":               "a\x00b",
		"team/scenario_team.ochan": "print(\"team\")\n",
	} {
		full := e.path(filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		e.write(full, text)
	}

	// Один файл — сразу содержимое: так проверяют сценарий.
	out := e.must("search", "notes")
	if !strings.Contains(out, "── notes.txt (") || !strings.Contains(out, "проверить work_mem на server1\n") {
		t.Fatalf("один файл:\n%s", out)
	}
	// Несколько — списком; скрытое, node_modules и подсказка.
	out = e.must("search", "work_mem.yaml")
	for _, want := range []string{filepath.Join("plans", "work_mem.yaml"), filepath.Join("plans", "old", "work_mem.yaml"),
		"Показать файл: search <имя точнее>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("список: нет %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "work_mem: 32MB") {
		t.Fatalf("список показал содержимое:\n%s", out)
	}
	if out := e.must("search", "work_mem.yaml", "--show"); !strings.Contains(out, "work_mem: 32MB") || !strings.Contains(out, "work_mem: 16MB") {
		t.Fatalf("--show:\n%s", out)
	}
	if out := e.must("search", "notes", "--list"); strings.Contains(out, "проверить") || !strings.Contains(out, "notes.txt") {
		t.Fatalf("--list:\n%s", out)
	}
	// Шаблон имени и путь.
	if out := e.must("search", "*.ochan", "--list"); !strings.Contains(out, "scenario_team.ochan") || !strings.Contains(out, "tune.ochan") ||
		strings.Contains(out, ".yaml") {
		t.Fatalf("шаблон:\n%s", out)
	}
	if out := e.must("search", "old/work"); !strings.Contains(out, "work_mem: 16MB") {
		t.Fatalf("часть пути:\n%s", out)
	}
	// Скрытое и node_modules — мимо; --all заходит в скрытое.
	e.fails("не найдено", "search", "secret")
	if out := e.must("search", "secret", "--all"); !strings.Contains(out, "token: x") {
		t.Fatalf("--all:\n%s", out)
	}
	e.fails("не найдено", "search", "a.yaml")
	// По тексту: строки с найденным; двоичный файл не читается.
	out = e.must("search", "--text", "WORK_MEM")
	if !strings.Contains(out, "notes.txt") || !strings.Contains(out, "1: проверить work_mem на server1") ||
		!strings.Contains(out, "3: work_mem: 32MB") || strings.Contains(out, "binary.dat") {
		t.Fatalf("--text:\n%s", out)
	}
	if out := e.must("search", "binary"); !strings.Contains(out, "двоичный файл — не показан") {
		t.Fatalf("двоичный файл:\n%s", out)
	}
	// Без имени — последние изменённые.
	if out := e.must("search"); !strings.Contains(out, "notes.txt") || !strings.Contains(out, "prod.conf") {
		t.Fatalf("без имени:\n%s", out)
	}
	e.fails("выберите одно", "search", "notes", "--show", "--list")

	// Заявки: без имени — ждущие решения, с именем — по сценарию и номеру.
	created := e.must("request", "tune.ochan")
	id := requestIDPattern.FindStringSubmatch(created)[1]
	if out := e.must("search"); !strings.Contains(out, "Заявки, которые ждут решения:") || !strings.Contains(out, id) {
		t.Fatalf("ждущие заявки:\n%s", out)
	}
	if out := e.must("search", id); !strings.Contains(out, "Заявки:") || !strings.Contains(out, "План заявки: requests <номер>") {
		t.Fatalf("заявка по номеру:\n%s", out)
	}
	if out := e.must("search", "tune"); !strings.Contains(out, "── tune.ochan") || !strings.Contains(out, id) {
		t.Fatalf("файл и заявка по сценарию:\n%s", out)
	}

	// Роль с областью: видит только её; рабочий каталог ochan — никогда.
	inside := filepath.Join(e.work, "team")
	e.must("role", "create", "dan", "--allow", "read")
	e.must("role", "scope", "dan", inside)
	t.Setenv(EnvRole, "dan")
	if err := os.Chdir(inside); err != nil {
		t.Fatal(err)
	}
	if out := e.must("search", "scenario"); !strings.Contains(out, "print(\"team\")") {
		t.Fatalf("роль с областью у себя:\n%s", out)
	}
	e.fails("вне области роли dan", "search", "notes", "--in", e.work)
	e.fails("рабочий каталог ochan", "search", "config", "--in", filepath.Join(e.home, ".octochan"))
}
