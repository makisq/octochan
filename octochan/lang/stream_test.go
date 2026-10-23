package lang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// streamRun выполняет исходник в каталоге dir; plan — в режиме плана.
func streamRun(t *testing.T, dir, src string, plan bool) (stdout, stderr string, interp *Interpreter, err error) {
	t.Helper()
	file, err := Parse("<тест>", src)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	interp = NewInterpreter(context.Background(), NewRegistry(), &out)
	interp.SetErr(&errOut)
	interp.SetDir(dir)
	if plan {
		interp.SetPlanMode()
	}
	err = interp.RunFile(file)
	return out.String(), errOut.String(), interp, err
}

// print пишет в поток, переданный первым аргументом: на экран, в поток
// ошибок или в конец файла.
func TestPrintToStreams(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, interp, err := streamRun(t, dir, `
log = file("run.log")
print(log, "первая", 1)
print("на экран")
print(stdout(), "тоже на экран")
print(stderr(), "ошибка")
print(log, "вторая")
write("other.txt", "x")
print(log, "третья")
print(log)
`, false)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "на экран\nтоже на экран\n" || stderr != "ошибка\n" {
		t.Fatalf("stdout %q, stderr %q", stdout, stderr)
	}
	if got := readFile(t, filepath.Join(dir, "run.log")); got != "первая 1\nвторая\nтретья\n\n" {
		t.Fatalf("run.log: %q", got)
	}

	// Печать подряд в один файл — одно действие, даже если между строками
	// записан другой файл.
	effects := interp.Effects()
	if len(effects) != 2 || !effects[0].Append || effects[0].Existed ||
		string(effects[0].After) != "первая 1\nвторая\nтретья\n\n" || effects[1].Append {
		t.Fatalf("действия: %+v", effects)
	}
}

// В режиме плана печать в файл ничего не пишет, но сценарий видит
// напечатанное, а план показывает одно изменение на файл.
func TestPrintToFileInPlanMode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "run.log"), "старое\n")
	stdout, _, interp, err := streamRun(t, dir, `
log = file("run.log")
for n in [1, 2, 3] { print(log, "строка", n) }
print(read("run.log"))
`, true)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "старое\nстрока 1\nстрока 2\nстрока 3\n\n" {
		t.Fatalf("сценарий не видит напечатанного в плане: %q", stdout)
	}
	if got := readFile(t, filepath.Join(dir, "run.log")); got != "старое\n" {
		t.Fatalf("режим плана изменил файл: %q", got)
	}

	plan := BuildPlan("тест", interp.Effects())
	if plan.Len() != 1 || !strings.Contains(plan.Render(60), "дописать 45 байт: было 13 байт, станет 58 байт") {
		t.Fatalf("план:\n%s", plan.Render(60))
	}
}

// Сценарий с печатью в файлы делает ровно то, что описал план: отпечатки
// совпадают, в том числе для parallel for, где строки итераций ложатся
// в порядке элементов.
func TestPrintPlanMatchesRun(t *testing.T) {
	src := `
log = file("run.log")
print(log, "начало")
parallel for n in [1, 2, 3, 4, 5, 6, 7, 8] {
    print(log, "сервер", n)
    print(file("n" + n + ".txt"), n)
}
print(log, "конец")
`
	planDir, runDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{planDir, runDir} {
		writeFile(t, filepath.Join(dir, "run.log"), "было\n")
	}

	_, _, planned, err := streamRun(t, planDir, src, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, ran, err := streamRun(t, runDir, src, false)
	if err != nil {
		t.Fatal(err)
	}

	planPrints, runPrints := planned.Effects(), ran.Effects()
	if len(planPrints) != len(runPrints) {
		t.Fatalf("план %d действий, выполнение %d", len(planPrints), len(runPrints))
	}
	for i := range planPrints {
		plan := strings.Replace(planPrints[i].Fingerprint(), planDir, "<dir>", 1)
		real := strings.Replace(runPrints[i].Fingerprint(), runDir, "<dir>", 1)
		if plan != real {
			t.Fatalf("действие %d: план %s, выполнение %s", i, plan, real)
		}
	}

	want := "было\nначало\n"
	for n := 1; n <= 8; n++ {
		want += "сервер " + string(rune('0'+n)) + "\n"
	}
	want += "конец\n"
	if got := readFile(t, filepath.Join(runDir, "run.log")); got != want {
		t.Fatalf("run.log:\n%s\nожидалось:\n%s", got, want)
	}
}

// write переписывает файл целиком: печать после него — новое действие.
func TestWriteClosesPrintToFile(t *testing.T) {
	dir := t.TempDir()
	_, _, interp, err := streamRun(t, dir, `
log = file("run.log")
print(log, "a")
write("run.log", "x")
print(log, "b")
`, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "run.log")); got != "xb\n" {
		t.Fatalf("run.log: %q", got)
	}
	if effects := interp.Effects(); len(effects) != 3 || string(effects[2].Before) != "x" {
		t.Fatalf("действия: %+v", effects)
	}
}

func TestFileStreamRespectsSandbox(t *testing.T) {
	root := t.TempDir()
	box, err := NewSandbox([]string{root}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := Parse("<тест>", `log = file("`+filepath.ToSlash(filepath.Join(t.TempDir(), "x.log"))+`")`)
	if err != nil {
		t.Fatal(err)
	}
	interp := NewInterpreter(context.Background(), NewRegistry(), &strings.Builder{})
	interp.SetSandbox(box)
	mustFail(t, interp.RunFile(file), "вне разрешённых каталогов")
}

func TestStreamType(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := streamRun(t, dir, `var log: stream = file("a.log")
log = stderr()
print(log, "ok")`, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := streamRun(t, dir, `var log: поток`, false); err == nil ||
		!strings.Contains(err.Error(), "у типа поток нет нулевого значения") {
		t.Fatalf("поток без значения: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.log")); !os.IsNotExist(err) {
		t.Fatal("file() создал файл, ничего не напечатав")
	}

	problems := check(t, "s = stdout()\nx = s * 2\nfor c in stderr() { }")
	text := joinProblems(problems)
	for _, want := range []string{"оператор * неприменим к поток", "по значению типа поток нельзя пройти циклом"} {
		if !strings.Contains(text, want) {
			t.Fatalf("проверка не нашла %q:\n%s", want, text)
		}
	}
}
