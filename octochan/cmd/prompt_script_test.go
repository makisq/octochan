package cmd

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"octochan/lang"
	"octochan/workspace"
)

// stdin подаёт текст на stdin следующей команды, как конвейер оболочки.
func (e *scriptEnv) stdin(text string) {
	e.t.Helper()
	path := filepath.Join(e.t.TempDir(), "stdin")
	e.write(path, text)
	f, err := os.Open(path)
	if err != nil {
		e.t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	stdinOnce, stdinBytes, stdinErr = sync.Once{}, nil, nil
	e.t.Cleanup(func() {
		os.Stdin = old
		_ = f.Close()
		stdinOnce, stdinBytes, stdinErr = sync.Once{}, nil, nil
	})
}

// skill ставит пакет pgconf: его алиасы — операции скилла @pgconf.
func (e *scriptEnv) skill(version string) {
	e.t.Helper()
	pkg := filepath.Join(e.home, ".octochan", "modules", "pgconf")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.write(filepath.Join(pkg, "lib.ochan"), "alias store(c: config, path: string) {\n    return save(c, path)\n}\n"+
		"alias verify(c: config) {\n    return c\n}\n")
	e.write(filepath.Join(pkg, lang.ManifestName), "version: "+version+"\n")
	resetAliasCache()
}

const promptHead = `goal: tuned

classes:
  State  : "tuned"
  Config : "config"

relations:
  Config --strives_to--> State

import @pgconf v1
  provides: Config, store, verify
  requires_class: Config

`

const promptReport = `report:
  q: limit
    choice: high
    alternatives: {low, high}
    basis: not_in_prompt
  q: prod_conf.max_connections
    choice: 200
    basis: not_in_prompt
`

// promptCell — значение, которое задаёт оператор: тогда это не решение
// модели, и отчёта о нём не нужно.
const promptCell = "cell c1:\n  set Config.max_connections = 200\n"

// Жёсткие правила промпта проверяются по плану: план, который их
// нарушает, отвергается, а соблюдённые правила называются.
func TestPlanChecksPromptRules(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf})\nhard require verify(scope = prod_conf)\n")
	e.write(e.path("report.fp"), promptReport)
	e.write(e.path("good.ochan"), `c = config("prod.conf")
put(c, "max_connections", "200")
pgconf.verify(c)
pgconf.store(c, "prod.conf")
`)
	e.write(e.path("bad.ochan"), `c = config("prod.conf")
put(c, "max_connections", "200")
pgconf.store(c, "prod.conf")
save(config("x = 1\n"), "other.conf")
`)

	out := e.must("plan", "good.ochan", "--prompt", "task.fp", "--report", "report.fp")
	for _, want := range []string{
		"Отчёт модели:\n  limit: high из {low, high} — not_in_prompt",
		"соблюдено  hard forbid save(except = {prod_conf})",
		"соблюдено  hard require verify(scope = {prod_conf})",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}

	out, err := e.ochan("plan", "bad.ochan", "--prompt", "task.fp")
	if err == nil || !strings.Contains(err.Error(), "план нарушает жёсткие правила промпта") {
		t.Fatalf("нарушение правил не отвергнуто: %v\n%s", err, out)
	}
	for _, want := range []string{
		"НАРУШЕНО   hard forbid save(except = {prod_conf})",
		"save в " + e.path("other.conf") + " (other_conf)",
		"НАРУШЕНО   hard require verify(scope = {prod_conf})",
		"verify не выполняется для: prod_conf",
		"подсказка: save допустимо только для исключений except = {prod_conf}",
		"подсказка: вызовите verify для каждой цели области: prod_conf",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}

	e.fails("план нарушает жёсткие правила промпта", "request", "bad.ochan", "--prompt", "task.fp")
	if out := e.must("requests"); !strings.Contains(out, "Заявок нет") {
		t.Fatalf("заявка на нарушающий план создана:\n%s", out)
	}
}

// Сценарий по промпту берёт операции только из подключённых промптом
// скиллов, а отказ называет, что подключить.
func TestScriptUsesOnlyPromptSkills(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	noImport := strings.Replace(promptHead, "import @pgconf v1\n  provides: Config, store, verify\n  requires_class: Config\n", "", 1)
	e.write(e.path("bare.fp"), noImport)
	e.write(e.path("uses.ochan"), "c = config(\"prod.conf\")\npgconf.verify(c)\n")

	out, err := e.ochan("plan", "uses.ochan", "--prompt", "bare.fp")
	if err == nil || !strings.Contains(err.Error(), "сценарий использует pgconf.verify, а промпт не подключает @pgconf") ||
		!strings.Contains(err.Error(), "подсказка: добавьте в промпт import @pgconf v1.2.0") {
		t.Fatalf("сценарий вышел за знание промпта: %v\n%s", err, out)
	}
	e.fails("промпт не подключает @pgconf", "request", "uses.ochan", "--prompt", "bare.fp")

	e.write(e.path("task.fp"), promptHead)
	e.must("plan", "uses.ochan", "--prompt", "task.fp")

	// Скилл подключён, но операции нет в provides:.
	narrow := strings.Replace(promptHead, "provides: Config, store, verify", "provides: Config, store", 1)
	e.write(e.path("narrow.fp"), narrow)
	e.fails("подсказка: добавьте verify в provides: import @pgconf", "plan", "uses.ochan", "--prompt", "narrow.fp")
}

func TestPromptIsCheckedBeforePlan(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf})\n")
	e.write(e.path("bad_report.fp"), "report:\n  q: limit\n    choice: huge\n    alternatives: {low, high}\n    basis: not_in_prompt\n")
	e.write(e.path("undef.fp"), promptHead+"hard forbid drop(except = {prod_conf})\n")

	// Ошибка в отчёте называется по строке отчёта, а не склейки.
	e.fails("report.fp:3:13: ⊥conflict: выбор huge не входит в альтернативы", "plan", "tune.ochan",
		"--prompt", "task.fp", "--report", "bad_report.fp")
	e.fails("действие drop неизвестно", "plan", "tune.ochan", "--prompt", "undef.fp")
	// Отказ промпта — с подсказкой: похожая операция скилла.
	e.write(e.path("typo.fp"), promptHead+"hard require verfy(scope = prod_conf)\n")
	e.fails("подсказка: возможно, имелось в виду verify", "plan", "tune.ochan", "--prompt", "typo.fp")
	e.fails("отчёт проверяется по промпту", "plan", "tune.ochan", "--report", "bad_report.fp")
	e.fails("укажите --script", "plan", "--prompt", "task.fp", "prod.conf", "prod.conf")
	e.fails("через --bundle", "plan", "--script", "--prompt", "-", "-")
}

// Промпт и отчёт попадают в заявку, видны согласующему и проверяются
// снова перед выполнением.
func TestScriptRequestCarriesPrompt(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf})\n")
	e.write(e.path("report.fp"), promptReport)
	doc := e.path("заявка.docx")

	created := e.must("request", "tune.ochan", "--prompt", "task.fp", "--report", "report.fp", "--doc", doc)
	id := requestIDPattern.FindStringSubmatch(created)[1]

	card := e.must("requests", id)
	for _, want := range []string{
		"Промпт task.fp:\n  | goal: tuned",
		"Жёсткие правила промпта (план их соблюдает):\n  hard forbid save(except = {prod_conf})",
		"Отчёт модели:\n  limit: high из {low, high} — not_in_prompt",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("в карточке нет %q:\n%s", want, card)
		}
	}
	text := docxText(t, doc)
	for _, want := range []string{"Формальный промпт", "Отчёт модели", "limit: high из {low, high}", "соблюдено в плане"} {
		if !strings.Contains(text, want) {
			t.Fatalf("в документе нет %q", want)
		}
	}

	e.must("approve", id)

	// Скилл, на который ссылается промпт, сменил старшую версию: промпт
	// написан не под него, и выполнять заявку нельзя.
	e.skill("2.0.0")
	e.fails("промпт заявки "+id, "apply", "--request", id)
	e.skill("1.2.0")

	out := e.must("apply", "--request", id)
	if !strings.Contains(out, "соблюдено  hard forbid save(except = {prod_conf})") {
		t.Fatalf("правила не проверены перед выполнением:\n%s", out)
	}
	if got := e.read("prod.conf"); !strings.Contains(got, "max_connections = 200") {
		t.Fatalf("сценарий не выполнен: %q", got)
	}
}

// ochan-mcp передаёт сценарий, промпт и отчёт одним JSON на stdin.
func TestBundleOnStdin(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	data, err := json.Marshal(map[string]string{
		"script": `save(config("x = 1\n"), "other.conf")`,
		"prompt": promptHead + "hard forbid save(except = {prod_conf})\n",
		"report": promptReport,
	})
	if err != nil {
		t.Fatal(err)
	}

	e.stdin(string(data))
	out, err := e.ochan("plan", "--script", "--bundle", "--name", "модель.ochan", "-")
	if err == nil || !strings.Contains(out, "save в "+e.path("other.conf")) || !strings.Contains(out, "План: модель.ochan") {
		t.Fatalf("правило не проверено по сценарию из --bundle: %v\n%s", err, out)
	}

	e.stdin(`{"script": "print(1)", "notes": "x"}`)
	e.fails(`unknown field "notes"`, "plan", "--script", "--bundle", "-")
	e.stdin(`{"prompt": "goal: x"}`)
	e.fails("--bundle: нет сценария", "plan", "--script", "--bundle", "-")
	e.fails("вместо файла укажите «-»", "plan", "--script", "--bundle", "tune.ochan")
}

var xmlTag = regexp.MustCompile(`<[^>]+>`)

// docxText — текст документа без разметки.
func docxText(t *testing.T, path string) string {
	t.Helper()
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, f := range archive.File {
		if f.Name != "word/document.xml" {
			continue
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return xmlTag.ReplaceAllString(string(data), " ")
	}
	t.Fatalf("в %s нет word/document.xml", path)
	return ""
}

var (
	rollbackHint = regexp.MustCompile(`Откат: ochan rollback ([0-9a-f]+)`)
	revertID     = regexp.MustCompile(`откат записан как ([0-9a-f]+)`)
)

// Сценарий, выполненный по заявке, откатывается: записанные файлы
// возвращаются к прежнему содержимому, созданные удаляются.
func TestScriptRollback(t *testing.T) {
	e := newScriptEnv(t)
	original := e.read("prod.conf")

	id := requestIDPattern.FindStringSubmatch(e.must("request", "tune.ochan"))[1]
	e.must("approve", id)
	applied := e.must("apply", "--request", id)
	match := rollbackHint.FindStringSubmatch(applied)
	if match == nil {
		t.Fatalf("выполнение не назвало, как его откатить:\n%s", applied)
	}
	entry := match[1]

	shown := e.must("history", entry)
	for _, want := range []string{"Операция: сценарий", "Откат:    ochan rollback " + entry,
		e.path("prod.conf") + " — был, байт: ", e.path("report.txt") + " — не было, откат удалит"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("в записи нет %q:\n%s", want, shown)
		}
	}

	// Файл поправили после сценария: откат стёр бы правку и отказывает
	// целиком — ни один файл не тронут.
	e.write(e.path("report.txt"), "правка руками")
	e.fails("изменились файлы: "+e.path("report.txt"), "rollback", entry)
	if !strings.Contains(e.read("prod.conf"), "max_connections = 200") {
		t.Fatal("отказавший откат тронул файл")
	}
	e.write(e.path("report.txt"), "max_connections: 200")

	out := e.must("rollback", entry)
	if e.read("prod.conf") != original {
		t.Fatalf("prod.conf не восстановлен: %q", e.read("prod.conf"))
	}
	if _, err := os.Stat(e.path("report.txt")); !os.IsNotExist(err) {
		t.Fatal("созданный сценарием файл не удалён")
	}
	if !strings.Contains(out, "удалён "+e.path("report.txt")+": его создала операция") {
		t.Fatalf("вывод отката:\n%s", out)
	}
	e.fails("уже отменена", "rollback", entry)

	// Откат — тоже операция, и её можно отменить.
	undo := revertID.FindStringSubmatch(out)[1]
	e.must("rollback", undo)
	if !strings.Contains(e.read("prod.conf"), "max_connections = 200") || e.read("report.txt") != "max_connections: 200" {
		t.Fatal("отмена отката не вернула результат сценария")
	}
}

func TestFileStatesKeepFirstBefore(t *testing.T) {
	states := fileStates([]lang.Effect{
		{Kind: lang.EffectWrite, Path: "/a", Existed: true, Before: []byte("0"), After: []byte("1")},
		{Kind: lang.EffectLive, Path: "postgres://db"},
		{Kind: lang.EffectWrite, Path: "/b", After: []byte("x")},
		{Kind: lang.EffectSave, Path: "/a", Existed: true, Before: []byte("1"), After: []byte("2")},
	})
	if len(states) != 2 {
		t.Fatalf("состояния: %+v", states)
	}
	a, b := states[0], states[1]
	if a.Path != "/a" || !a.Existed || string(a.Content) != "0" || a.After != workspace.StateHash(true, []byte("2")) {
		t.Fatalf("файл, записанный дважды: %+v", a)
	}
	if b.Path != "/b" || b.Existed || b.Content != nil {
		t.Fatalf("созданный файл: %+v", b)
	}
}

// Печать в файл — запись, как write: она в плане заявки, под жёсткими
// правилами промпта и откатывается.
func TestPrintToFileIsAWrite(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("log.ochan"), `log = file("run.log")
for n in [1, 2, 3] { print(log, "шаг", n) }
print("готово")
`)
	e.write(e.path("task.fp"), promptHead+"hard forbid write(except = {report_txt})\n")

	plan := e.must("plan", "log.ochan")
	if !strings.Contains(plan, "записать   файл — новый файл, 27 байт") || strings.Count(plan, "run.log") != 1 {
		t.Fatalf("печать в файл не в плане одним изменением:\n%s", plan)
	}
	e.fails("план нарушает жёсткие правила промпта", "plan", "log.ochan", "--prompt", "task.fp")

	id := requestIDPattern.FindStringSubmatch(e.must("request", "log.ochan"))[1]
	e.must("approve", id)
	entry := rollbackHint.FindStringSubmatch(e.must("apply", "--request", id))[1]
	if got := e.read("run.log"); got != "шаг 1\nшаг 2\nшаг 3\n" {
		t.Fatalf("run.log: %q", got)
	}

	e.must("rollback", entry)
	if _, err := os.Stat(e.path("run.log")); !os.IsNotExist(err) {
		t.Fatal("откат не удалил файл, созданный печатью")
	}
}

// Обобщение промпта ограничивает значения, которые модель подставляет в
// операцию: вызов за доменом слота — план не проходит.
func TestPlanChecksGeneralizationSlots(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead+"generalization store_target(target : {staging, test}) :\n  Config = store(_, target)\n")
	e.write(e.path("good.ochan"), "c = config(\"prod.conf\")\npgconf.store(c, \"staging\")\n")
	e.write(e.path("bad.ochan"), "c = config(\"prod.conf\")\npgconf.store(c, \"production\")\n")

	if out := e.must("plan", "good.ochan", "--prompt", "task.fp"); !strings.Contains(out,
		"соблюдено  generalization store_target(target : {staging, test}) = store — вызовов 1") {
		t.Fatalf("законный план:\n%s", out)
	}
	out, err := e.ochan("plan", "bad.ochan", "--prompt", "task.fp")
	if err == nil || !strings.Contains(out, "НАРУШЕНО   generalization store_target(target : {staging, test}) = store") ||
		!strings.Contains(out, "pgconf.store(prod.conf, production): target = production вне домена {staging, test}") {
		t.Fatalf("вызов за доменом прошёл: %v\n%s", err, out)
	}
	e.fails("план нарушает жёсткие правила промпта", "request", "bad.ochan", "--prompt", "task.fp")
}

// Отчёт модели сверяется с планом: решение модели, о котором отчёт
// молчит или говорит другое, план не пропускает. Значение из предпосылок —
// не решение модели, значение ячейки промпта — решение оператора.
func TestReportBoundToPlan(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead)
	e.write(e.path("source.conf"), "max_connections = 450\n")
	// Здесь отказы — предмет проверки, а не перебор модели.
	setRings(t, "budget.plan", 0)
	write := func(value string) {
		e.write(e.path("s.ochan"), "c = config(\"prod.conf\")\nput(c, \"max_connections\", "+value+")\npgconf.store(c, \"prod.conf\")\n")
	}
	report := func(entries ...string) {
		e.write(e.path("r.fp"), "report:\n"+strings.Join(entries, ""))
	}
	entry := func(q, choice string) string {
		return "  q: " + q + "\n    choice: " + choice + "\n    basis: not_in_prompt\n"
	}
	plan := func() (string, error) { return e.ochan("plan", "s.ochan", "--prompt", "task.fp", "--report", "r.fp") }
	expect := func(want string) {
		t.Helper()
		out, err := plan()
		if err == nil || !strings.Contains(out, want) {
			t.Fatalf("ожидался отказ с %q: %v\n%s", want, err, out)
		}
	}
	pass := func() string {
		t.Helper()
		out, err := plan()
		if err != nil {
			t.Fatalf("план не прошёл: %v\n%s", err, out)
		}
		return out
	}

	report(entry("limit", "high"))
	write(`"300"`)
	expect("НЕТ В ОТЧЁТЕ prod_conf.max_connections = 300 — литерал сценария")
	// Спрятать решение в вычисление не выходит.
	write(`str(100 * 3)`)
	expect("НЕТ В ОТЧЁТЕ prod_conf.max_connections = 300 — зависит от чисел сценария")

	report(entry("prod_conf.max_connections", "250"))
	expect("РАСХОДИТСЯ  отчёт: prod_conf.max_connections = 250, а план пишет prod_conf.max_connections = 300")

	report(entry("prod_conf.max_connections", "300"))
	if out := pass(); !strings.Contains(out, "в отчёте    prod_conf.max_connections = 300 (зависит от чисел сценария)") {
		t.Fatalf("решение в отчёте:\n%s", out)
	}
	report(entry("prod_conf.max_connections", "300"), entry("prod_conf.work_mem", "16 MB"))
	expect("отчёт говорит о prod_conf.work_mem, а план его не записывает")

	// Значение из предпосылки — не решение модели.
	report(entry("limit", "high"))
	write(`get(config("source.conf"), "max_connections")`)
	pass()

	// Значение задаёт оператор: отчёта не нужно, но и другое значение нельзя.
	e.write(e.path("task.fp"), promptHead+promptCell)
	write(`"200"`)
	pass()
	write(`"300"`)
	expect("РАСХОДИТСЯ  prod_conf.max_connections = 300, а промпт задаёт 200")

	// В песочнице — тот же второй план, отдельным процессом. Свой промпт —
	// свой счёт отказов.
	e.write(e.path("sandbox.fp"), promptHead+"# в песочнице\n")
	report(entry("limit", "high"))
	write(`str(100 * 3)`)
	if out, err := e.ochan("plan", "s.ochan", "--prompt", "sandbox.fp", "--report", "r.fp", "--sandbox", "--root", e.work); err == nil ||
		!strings.Contains(out, "НЕТ В ОТЧЁТЕ prod_conf.max_connections = 300 — зависит от чисел сценария") {
		t.Fatalf("песочница: %v\n%s", err, out)
	}
}
