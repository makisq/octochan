package lang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/config"
)

// planned выполняет исходник в режиме плана в каталоге dir.
func planned(t *testing.T, dir, src string) (string, []Effect, error) {
	t.Helper()

	file, err := Parse("<тест>", src)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	interp := NewInterpreter(context.Background(), NewRegistry(), &out)
	interp.SetDir(dir)
	interp.SetPlanMode()
	err = interp.RunFile(file)
	return out.String(), interp.Effects(), err
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// В режиме плана ничего не записывается, но сценарий дальше видит
// записанное: иначе план описывал бы не тот сценарий, что выполнится.
func TestPlanModeWritesNothingButSeesItsOwnWrites(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.conf"), "port = 5432\n")

	out, effects, err := planned(t, dir, `
write("note.txt", "первая")
print(exists("note.txt"), read("note.txt"))
c = config("a.conf")
put(c, "port", "6432")
save(c, "a.conf")
print(param("a.conf", "port"))
`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "true первая\n6432\n"; out != want {
		t.Fatalf("вывод %q, ожидалось %q", out, want)
	}

	if _, err := os.Stat(filepath.Join(dir, "note.txt")); !os.IsNotExist(err) {
		t.Fatal("режим плана создал файл")
	}
	if got := readFile(t, filepath.Join(dir, "a.conf")); got != "port = 5432\n" {
		t.Fatalf("режим плана изменил файл: %q", got)
	}

	if len(effects) != 2 || effects[0].Kind != EffectWrite || effects[1].Kind != EffectSave {
		t.Fatalf("неверные действия: %+v", effects)
	}
	if effects[1].Path != filepath.Join(dir, "a.conf") || !effects[1].Existed {
		t.Fatalf("неверное действие save: %+v", effects[1])
	}
}

// Тот же сценарий без режима плана делает ровно то, что план описал:
// отпечатки действий совпадают.
func TestPlanMatchesRealRun(t *testing.T) {
	src := `
c = config("a.conf")
put(c, "port", "6432")
save(c, "a.conf")
write("log.txt", "порт: " + param("a.conf", "port"))
`
	planDir, runDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{planDir, runDir} {
		writeFile(t, filepath.Join(dir, "a.conf"), "port = 5432\n")
	}

	_, planEffects, err := planned(t, planDir, src)
	if err != nil {
		t.Fatal(err)
	}

	file, err := Parse("<тест>", src)
	if err != nil {
		t.Fatal(err)
	}
	interp := NewInterpreter(context.Background(), NewRegistry(), &strings.Builder{})
	interp.SetDir(runDir)
	if err := interp.RunFile(file); err != nil {
		t.Fatal(err)
	}
	runEffects := interp.Effects()

	if len(planEffects) != len(runEffects) {
		t.Fatalf("план %d действий, выполнение %d", len(planEffects), len(runEffects))
	}
	for i := range planEffects {
		plan := strings.Replace(planEffects[i].Fingerprint(), planDir, "<dir>", 1)
		real := strings.Replace(runEffects[i].Fingerprint(), runDir, "<dir>", 1)
		if plan != real {
			t.Fatalf("действие %d: план %s, выполнение %s", i, plan, real)
		}
	}
	if got := readFile(t, filepath.Join(runDir, "log.txt")); got != "порт: 6432" {
		t.Fatalf("выполнение записало %q", got)
	}
}

func TestPlanDescribesConfigChanges(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.conf"), "# боевой\nport = 5432\nwork_mem = 4MB\n")

	_, effects, err := planned(t, dir, `
c = config("a.conf")
put(c, "port", "6432")
put(c, "max_connections", "200")
save(c, "a.conf")
save(config("x = 1\n"), "new.conf")
write("same.txt", "")
write("same.txt", "")
`)
	if err != nil {
		t.Fatal(err)
	}

	plan := BuildPlan("тест.ochan", effects)
	text := plan.Render(60)

	for _, want := range []string{
		"изменить   port: 5432 -> 6432",
		"добавить   max_connections = 200",
		"save перепишет " + filepath.Join(dir, "a.conf") + " целиком: комментарии",
		"добавить   x = 1",
		"записать   файл — новый файл, 0 байт",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("в плане нет %q:\n%s", want, text)
		}
	}
	// Вторая запись того же содержимого ничего не меняет и в план не входит.
	if strings.Count(text, "записать") != 1 {
		t.Fatalf("пустая перезапись попала в план:\n%s", text)
	}
}

// save пишет файл целиком: параметр, которого нет в новом конфиге,
// из файла исчезнет. patch так не делает, поэтому план обязан это показать.
func TestPlanShowsRemovedParameters(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.conf"), "port = 5432\nwork_mem = 4MB\n")

	_, effects, err := planned(t, dir, `save(config("port = 5432\n"), "a.conf")`)
	if err != nil {
		t.Fatal(err)
	}

	plan := BuildPlan("тест", effects)
	if plan.Len() != 1 || plan.Changes[0].Kind != config.ChangeRemove || plan.Changes[0].Name != "work_mem" {
		t.Fatalf("ожидалось удаление work_mem, план:\n%s", plan.Render(60))
	}
}

func TestPlanRejectsWriteIntoMissingDirectory(t *testing.T) {
	_, _, err := planned(t, t.TempDir(), `write("нет/файл.txt", "x")`)
	mustFail(t, err, "каталог для нет/файл.txt не существует")
}

// Порядок действий parallel for — порядок элементов, а не завершения:
// иначе два плана одного сценария нельзя было бы сравнить.
func TestPlanParallelEffectsAreOrdered(t *testing.T) {
	dir := t.TempDir()
	src := "parallel for n in [1, 2, 3, 4, 5, 6, 7, 8] { write(\"f\" + n + \".txt\", str(n)) }"

	var first []string
	for attempt := 0; attempt < 20; attempt++ {
		_, effects, err := planned(t, dir, src)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, e := range effects {
			paths = append(paths, filepath.Base(e.Path))
		}
		if attempt == 0 {
			first = paths
			if strings.Join(paths, ",") != "f1.txt,f2.txt,f3.txt,f4.txt,f5.txt,f6.txt,f7.txt,f8.txt" {
				t.Fatalf("порядок действий: %v", paths)
			}
			continue
		}
		if strings.Join(paths, ",") != strings.Join(first, ",") {
			t.Fatalf("порядок действий плавает: %v и %v", first, paths)
		}
	}
}

func TestPlanWarnsAboutLiveSources(t *testing.T) {
	config.RegisterSourceScheme("ochanplan", func(_ context.Context, _ string, _ config.SourceOptions) (*config.LoadedConfig, error) {
		return &config.LoadedConfig{Name: "ochanplan://db1", Data: config.ConfigMap{"": {"port": "5432"}}}, nil
	})

	_, effects, err := planned(t, t.TempDir(), `
a = config("ochanplan://u:secret@db1")
b = config("ochanplan://u:secret@db1")
`)
	if err != nil {
		t.Fatal(err)
	}

	plan := BuildPlan("тест", effects)
	if len(plan.Warnings) != 1 || plan.Warnings[0] != "сценарий читает живой источник ochanplan://db1" {
		t.Fatalf("предупреждения плана: %v", plan.Warnings)
	}
	if plan.Len() != 0 {
		t.Fatalf("чтение попало в изменения:\n%s", plan.Render(60))
	}
}

// Режим плана помнит, какие алиасы вызывались и что записали: по этому
// арбитр проверяет жёсткие правила промпта.
func TestPlanTracesInvocations(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.conf"), "port = 5432\n")

	file, err := Parse("<тест>", `
alias store(c, path) { return save(c, path) }
alias touch(path) { return write(path, "x") }
c = config("a.conf")
store(c, "a.conf")
touch("note.txt")
parallel for n in ["1", "2", "3"] { touch("f" + n + ".txt") }
`)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := registry.AddFile("", file); err != nil {
		t.Fatal(err)
	}
	interp := NewInterpreter(context.Background(), registry, &strings.Builder{})
	interp.SetDir(dir)
	interp.SetPlanMode()
	if err := interp.RunFile(file); err != nil {
		t.Fatal(err)
	}

	calls := interp.Invocations()
	var got []string
	ids := map[int]bool{}
	for _, call := range calls {
		got = append(got, call.Qualified()+"("+strings.Join(call.Args, ",")+")")
		if ids[call.ID] {
			t.Fatalf("ID вызова повторяется: %d", call.ID)
		}
		ids[call.ID] = true
	}
	want := "store(a.conf,a.conf);touch(note.txt);touch(f1.txt);touch(f2.txt);touch(f3.txt)"
	if strings.Join(got, ";") != want {
		t.Fatalf("вызовы:\n  %s\nожидалось\n  %s", strings.Join(got, ";"), want)
	}

	effects := interp.Effects()
	if len(effects) != 5 {
		t.Fatalf("действий %d, ожидалось 5", len(effects))
	}
	for i, e := range effects {
		if len(e.Calls) != 1 || e.Calls[0] != calls[i].ID {
			t.Fatalf("действие %d (%s) приписано вызовам %v, ожидался %d", i, e.Path, e.Calls, calls[i].ID)
		}
	}
}
