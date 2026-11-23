package fp

import (
	"reflect"
	"strings"
	"testing"
)

const deriveTask = `goal: probe   # цель

classes:
  File     : "file"
  Table    : "table"
  Approval : "approve"
  Probe    : "probe"

relations:
  File --defines--> Approval (database_quota)
  Table --modifies--> Approval (low_nightly)
  Approval --strives_to--> Probe

priority:
  Table < File

generalization low_nightly(table : Table, mode : {fast, slow}) :
  Approval = resize_table(table, mode)

instance h121151 = low_nightly(table := plodu2222_ru, mode := fast)
instance h7 = low_nightly(table := kavu17, mode := slow)

import @retry v1
  provides: Table, resize_table

cell c1:
  set File.size = 8 GB
  set Table.size = 4 GB
  set File.owner = root
  set low_nightly.window = night
  set low_nightly.mode = slow
  set h7.window = day
`

func derivedPlan(t *testing.T, src string) *AnswerPlan {
	t.Helper()
	prog, diags := Parse("task.fp", src)
	if len(diags) > 0 {
		t.Fatalf("разбор: %v", diags)
	}
	return PlanAnswer(src, prog)
}

// Перенос по отношению, конфликт по приоритету, наследование по стрелке:
// только у стрелки без своего значения и не на слот.
func TestDerive(t *testing.T) {
	plan := derivedPlan(t, deriveTask)
	want := []Decision{
		{Target: "Approval", Attr: "size", Value: "4 GB", Basis: "priority(Table < File)", Rules: []string{"fp.modifies"}},
		{Target: "Approval", Attr: "owner", Value: "root", Basis: "relation(database_quota)", Rules: []string{"fp.defines"}},
		{Target: "h121151", Attr: "window", Value: "night", Basis: "relation(low_nightly)", Rules: []string{"fp.arrow"}},
	}
	if !reflect.DeepEqual(plan.Decisions, want) {
		t.Fatalf("решения:\n%+v\nожидались:\n%+v", plan.Decisions, want)
	}
	if len(plan.Open) != 0 || plan.Cell != "c2" {
		t.Fatalf("открытые %+v, ячейка %s", plan.Open, plan.Cell)
	}

	// Без приоритета конфликт — вопрос оператору, а не решение.
	prog, _ := Parse("task.fp", strings.Replace(deriveTask, "priority:\n  Table < File\n", "", 1))
	d := Derive(prog)
	if len(d.Open) != 1 || d.Open[0].Attr != "size" || !reflect.DeepEqual(d.Open[0].Values, []string{"8 GB", "4 GB"}) {
		t.Fatalf("конфликт без приоритета: %+v", d.Open)
	}
	for _, dec := range d.Decisions {
		if dec.Attr == "size" {
			t.Fatalf("конфликт без приоритета решён: %+v", dec)
		}
	}
}

// Перенос — цепочкой: выведенное — посылка для следующего шага; посылка
// на середине не переписывается; путь по кругу кончается; другой путь с
// другим значением — вопрос оператору, и от вопроса дальше не переносится.
func TestDeriveChains(t *testing.T) {
	derive := func(relations, cells string) Derivation {
		t.Helper()
		src := "goal: probe\n\nclasses:\n  File : \"file\"\n  Approval : \"approve\"\n  Probe : \"probe\"\n" +
			"  Table : \"table\"\n  Storage : \"disk\"\n\nrelations:\n" + relations + "\ncell c1:\n" + cells
		r := CheckText("task.fp", src, Options{Lexicon: BaseLexicon()})
		if !r.InLanguage() {
			t.Fatalf("задача не в языке: %v", r.Syntax())
		}
		return Derive(r.Program)
	}
	check := func(name string, got Derivation, decisions []Decision, open []OpenQuestion) {
		t.Helper()
		if !reflect.DeepEqual(got.Decisions, decisions) || !reflect.DeepEqual(got.Open, open) {
			t.Fatalf("%s:\nрешения %+v\nвопросы %+v\nожидались %+v\n%+v", name, got.Decisions, got.Open, decisions, open)
		}
	}
	line := "  File --defines--> Approval (database_quota)\n  Approval --modifies--> Probe (low_nightly)\n"

	defines, chain := []string{"fp.defines"}, []string{"fp.defines", "fp.modifies"}
	check("File → Approval → Probe", derive(line, "  set File.size = 8 GB\n"), []Decision{
		{Target: "Approval", Attr: "size", Value: "8 GB", Basis: "relation(database_quota)", Rules: defines},
		{Target: "Probe", Attr: "size", Value: "8 GB", Basis: "relation(low_nightly)", Rules: chain},
	}, nil)
	check("посылка на середине", derive(line, "  set File.size = 8 GB\n  set Approval.size = 2 GB\n"), []Decision{
		{Target: "Probe", Attr: "size", Value: "2 GB", Basis: "relation(low_nightly)", Rules: []string{"fp.modifies"}},
	}, nil)
	check("по кругу", derive(line+"  Probe --limits--> Approval (connection_limit)\n", "  set File.size = 8 GB\n"), []Decision{
		{Target: "Approval", Attr: "size", Value: "8 GB", Basis: "relation(database_quota)", Rules: defines},
		{Target: "Probe", Attr: "size", Value: "8 GB", Basis: "relation(low_nightly)", Rules: chain},
	}, nil)
	// Table → Probe даёт 4 GB на первом шаге, File → Approval → Probe — 8 GB
	// на втором: приоритета между Approval и Table нет — Probe вопрос
	// оператору, и до Storage ничего не доходит.
	check("два пути", derive(line+"  Table --limits--> Probe (log_size)\n  Probe --defines--> Storage (nightly_backup)\n",
		"  set File.size = 8 GB\n  set Table.size = 4 GB\n"),
		[]Decision{{Target: "Approval", Attr: "size", Value: "8 GB", Basis: "relation(database_quota)", Rules: defines}},
		[]OpenQuestion{{Targets: []string{"Probe"}, Attr: "size", Values: []string{"8 GB", "4 GB"}}})
}

// План: копия задачи — каноническая; порядок решений выбирают среди
// допустимых, остальное следует из написанного.
func TestAnswerPlan(t *testing.T) {
	plan := derivedPlan(t, deriveTask)
	if strings.Contains(plan.Copy, "#") || strings.Contains(plan.Copy, "File     :") || !strings.HasSuffix(plan.Copy, "c1:\n  set File.size = 8 GB\n  set Table.size = 4 GB\n  set File.owner = root\n  set low_nightly.window = night\n  set low_nightly.mode = slow\n  set h7.window = day\n") {
		t.Fatalf("копия:\n%s", plan.Copy)
	}

	text := plan.Copy
	steps := [][]string{{"\n"}, {"cell c2:\n"}}
	for _, want := range steps {
		adm, ok := plan.Next(text)
		if !ok || !reflect.DeepEqual(adm, want) {
			t.Fatalf("после %q: %q %v, ожидалось %q", text[len(plan.Copy):], adm, ok, want)
		}
		text += adm[0]
	}
	adm, _ := plan.Next(text)
	if len(adm) != 3 {
		t.Fatalf("решения ячейки: %q", adm)
	}
	// Модель выбрала решение о стрелке первым — отчёт идёт в этом порядке.
	text += "  set h121151.window = night\n"
	if adm, _ := plan.Next(text); len(adm) != 2 {
		t.Fatalf("после одного решения: %q", adm)
	}
	text += "  set Approval.size = 4 GB\n  set Approval.owner = root\n"
	for {
		adm, ok := plan.Next(text)
		if !ok {
			t.Fatalf("план не ведёт ответ:\n%s", text)
		}
		if len(adm) == 0 {
			break
		}
		if len(adm) != 1 {
			t.Fatalf("после решений выбора нет: %q", adm)
		}
		text += adm[0]
	}
	wantTail := "\nreport:\n  q: window\n    choice: night\n    basis: relation(low_nightly)\n" +
		"  q: size\n    choice: 4 GB\n    basis: priority(Table < File)\n  q: owner\n    choice: root\n    basis: relation(database_quota)\n"
	if !strings.HasSuffix(text, wantTail) {
		t.Fatalf("ответ:\n%s", text)
	}
	if r := CheckModelText("answer", text, Options{Builtins: []string{"write"}}); len(r.Syntax()) > 0 {
		t.Fatalf("ответ не в языке: %v", r.Syntax())
	}

	// Ответ ушёл от плана — логика это место не ведёт.
	if _, ok := plan.Next(plan.Copy + "\ncell c2:\n  set Probe.size = 1\n"); ok {
		t.Fatal("чужое решение принято планом")
	}
	// Решений нет — ответ кончается копией.
	empty := derivedPlan(t, "goal: probe\n\nclasses:\n  Probe : \"probe\"\n")
	if adm, ok := empty.Next(empty.Copy); !ok || len(adm) != 0 || empty.Text() != empty.Copy {
		t.Fatalf("без решений: %q %v", adm, ok)
	}
}

const inexactTask = `goal: probe

classes:
  File     : "file"
  Approval : "approve"
  Restart  : "restart"
  Probe    : "probe"

relations:
  File --defines--> Approval (database_quota)
  Approval --strives_to--> Probe

cell c1:
  set File.window = night
  set File.size = 900
  set Approval.window = {night, weekend}
  set Approval.size = 100..500
  set Restart.window = {night, weekend}
  set Restart.mode = {fast, slow}
`

// Неточная предпосылка (раздел 5.2): точная посылка через отношение
// сужает её внутри множества, вне — вопрос оператору; по истории сужает
// модель, ochan проверяет только допустимость.
func TestInexactPremises(t *testing.T) {
	prog, diags := Parse("task.fp", inexactTask)
	if len(diags) > 0 {
		t.Fatalf("разбор: %v", diags)
	}
	sets := prog.Cells[0].Sets
	if v := sets[2].Value; !reflect.DeepEqual(v.Variants, []string{"night", "weekend"}) || v.Text != "{night, weekend}" {
		t.Fatalf("варианты: %+v", v)
	}
	if v := sets[3].Value; v.Range == nil || v.Range.Lo != 100 || v.Range.Hi != 500 || !v.Admits("200") || v.Admits("900") {
		t.Fatalf("диапазон: %+v", v)
	}

	plan := PlanAnswer(inexactTask, prog)
	want := []Decision{{Target: "Approval", Attr: "window", Value: "night", Alternatives: []string{"night", "weekend"},
		Basis: "relation(database_quota)", Rules: []string{"fp.defines"}}}
	if !reflect.DeepEqual(plan.Decisions, want) {
		t.Fatalf("сужение: %+v", plan.Decisions)
	}
	if len(plan.Open) != 1 || plan.Open[0].Attr != "size" {
		t.Fatalf("900 вне 100..500 — вопрос оператору: %+v", plan.Open)
	}

	// Сужение по истории — только неточного, внутри и без второго значения
	// того же вопроса отчёта.
	if _, err := plan.Narrowings([]string{"set Restart.mode = slow", "set Restart.window = night"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"set Restart.mode = noon", "set File.window = day", "set Approval.window = weekend",
		"set Restart.window = weekend"} {
		if _, err := plan.Narrowings([]string{bad}); err == nil {
			t.Errorf("сужение %q принято", bad)
		}
	}
	narrowed, _ := plan.Narrowings([]string{"set Restart.mode = slow"})
	answer := plan.Copy + "\ncell c2:\n  set Restart.mode = slow\n  set Approval.window = night\n\nreport:\n" +
		"  q: mode\n    choice: slow\n    alternatives: {fast, slow}\n    basis: history(mode)\n" +
		"  q: window\n    choice: night\n    alternatives: {night, weekend}\n    basis: relation(database_quota)\n"
	if err := plan.Verify(answer, nil, narrowed); err != nil {
		t.Fatal(err)
	}
	if r := CheckModelText("answer", answer, Options{}); len(r.Syntax()) > 0 || r.Errors() > 0 {
		t.Fatalf("ответ с сужением: %v", r.Diagnostics)
	}

	// Отчёт: выбор вне неточного — противоречие; history без неточного — нет основания.
	for text, code := range map[string]string{
		strings.Replace(answer, "choice: slow", "choice: noon", 1):                  "⊥conflict",
		strings.Replace(answer, "basis: history(mode)", "basis: history(owner)", 1): "⊥undef",
	} {
		r := CheckModelText("answer", text, Options{})
		found := false
		for _, d := range r.Diagnostics {
			found = found || string(d.Code) == code
		}
		if !found {
			t.Errorf("ожидался %s: %v", code, r.Diagnostics)
		}
	}
}

func TestCanonical(t *testing.T) {
	src := "# заголовок\ngoal: x   # цель\n\n\n\nclasses:\n  A    : \"a # не комментарий\"\n\n"
	if got := Canonical(src); got != "goal: x\n\nclasses:\n  A : \"a # не комментарий\"\n" {
		t.Fatalf("%q", got)
	}
}
