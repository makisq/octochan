package fp

import (
	"os"
	"strings"
	"testing"
)

// head — минимальная правильная программа, к которой тесты дописывают
// проверяемый фрагмент.
const head = `goal: northwest

classes:
  Goal       : "northwest"
  Obstacle   : "obstacle"
  Trajectory : "path"
  Fuel       : "fuel"
  Movement   : "move"

relations:
  Obstacle --forces--> Trajectory (detour)
`

func check(t *testing.T, src string, opts Options) *Result {
	t.Helper()
	return CheckText("тест.fp", src, opts)
}

func dump(diags []Diagnostic) string {
	lines := make([]string, len(diags))
	for i, d := range diags {
		lines[i] = "  " + d.String()
	}
	return strings.Join(lines, "\n")
}

// expect требует находку с этим кодом и фрагментом текста.
func expect(t *testing.T, r *Result, code Code, fragment string) {
	t.Helper()
	for _, d := range r.Diagnostics {
		if d.Code == code && strings.Contains(d.Msg, fragment) {
			return
		}
	}
	t.Fatalf("нет находки %s с %q, получено:\n%s", code, fragment, dump(r.Diagnostics))
}

func expectClean(t *testing.T, r *Result) {
	t.Helper()
	if r.Errors() > 0 {
		t.Fatalf("ожидалось без отказов, получено:\n%s", dump(r.Diagnostics))
	}
}

func readExample(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Пример из описания синтаксиса без правок: проверка находит ровно то,
// чего в нём не хватает, с местом каждой находки.
func TestSpecExampleFindings(t *testing.T) {
	r := check(t, readExample(t, "spec.fp"), Options{})

	want := []string{
		"23:16: ⊥undef: операция path_around (обобщение detour) не дана ни одним скиллом",
		"25:10: ⊥undef: в частном случае detour_1 не заполнены слоты: obstacle",
		"25:28: ⊥syn: у обобщения detour нет слота o; слоты: obstacle, side",
		"27:11: ⊥undef: fuel_consumption_model: такого отношения нет ни в промпте, ни в скиллах",
		"33:1: ⊥undef: действие disconnect неизвестно ни скиллам, ни отношениям",
		"34:1: ⊥undef: действие verify_config неизвестно ни скиллам, ни отношениям",
		"41:6: замечание: Trajectory.detour_side задан промптом",
	}
	if len(r.Diagnostics) != len(want) {
		t.Fatalf("находок %d, ожидалось %d:\n%s", len(r.Diagnostics), len(want), dump(r.Diagnostics))
	}
	for i, w := range want {
		if got := r.Diagnostics[i].String(); !strings.HasPrefix(got, w) {
			t.Errorf("находка %d:\n  получено  %s\n  ожидалось %s…", i+1, got, w)
		}
	}
	if r.Goal != "Goal" {
		t.Errorf("цель γ = %q, ожидался класс Goal", r.Goal)
	}
}

func TestCorrectedExampleIsClean(t *testing.T) {
	r := check(t, readExample(t, "navigation.fp"), Options{})
	expectClean(t, r)

	for _, name := range []string{"detour", "detour_1", "path_around", "fuel_consumption_model", "stop"} {
		if !contains(r.Knowledge, name) {
			t.Errorf("в K нет %s: %v", name, r.Knowledge)
		}
	}
	if got := strings.Join(r.Dependents["c2"], ","); got != "c3" {
		t.Errorf("D(c2) = %q, ожидалось c3", got)
	}
	if len(r.Dependents["c1"]) != 0 {
		t.Errorf("от c1 никто не зависит, получено %v", r.Dependents["c1"])
	}
}

// ---------------------------------------------------------------------------
// ⊥syn: текст вне языка
// ---------------------------------------------------------------------------

func TestOutOfLanguageInputIsRejected(t *testing.T) {
	cases := map[string]struct{ src, fragment string }{
		"приветствие":           {head + "hello there\n", "такой последовательности нет в языке"},
		"вопрос":                {head + "how are you?\n", "вопрос — не высказывание языка"},
		"приращение":            {head + "increase speed by 5\n", "приращение, а не значение"},
		"приращение в ячейке":   {head + "cell c1:\n  increase Fuel.reserve\n", "ячейки декларативны"},
		"маркер !":              {head + "!n_disconnect(except: {w})\n", "пишите ??n_действие"},
		"восклицание":           {head + "привет! как дела\n", "восклицание — не высказывание языка"},
		"сигнал без вида":       {head + "??x_stop()\n", "после ?? пишется n_действие"},
		"свободный текст":       {head + "instance d = detour(obstacle := \"камень на дороге\")\n", "свободный текст"},
		"текст в исключении":    {head + "hard forbid stop(except = \"ночью\")\n", "на том же строгом языке (Ж2)"},
		"чужой параметр":        {head + "hard forbid stop(when = night)\n", "параметры except и scope"},
		"неизвестное отношение": {head + "relations:\n  Fuel --eats--> Obstacle\n", "не входит в словарь R"},
		"класс со строчной":     {"goal: x\nclasses:\n  goal : \"x\"\n", "ожидался класс"},
		"табуляция":             {head + "cell c1:\n\tset Fuel.reserve = 10\n", "табуляция в отступе"},
		"отступ вне раздела":    {head + "requires: detour\n  set Fuel.reserve = 10\n", "отступом пишется только тело раздела"},
		"нет цели":              {"classes:\n  Goal : \"x\"\n", "начинается с заголовка goal:"},
		"цель не первой":        {"classes:\n  Goal : \"x\"\ngoal: x\n", "пишется первым"},
		"пустой раздел":         {head + "priority:\n", "раздел priority: пуст"},
		"плохой диапазон кода":  {head + "cell c1:\n  binds_to: a.c:9-3\n", "диапазон строк 9-3"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := check(t, tc.src, Options{})
			expect(t, r, ErrSyn, tc.fragment)
		})
	}
}

// Разбор не останавливается на первой ошибке, а проверки знания после
// синтаксической ошибки не выполняются (алгоритм 1, шаг 1).
func TestSyntaxErrorsAreAllReportedAndStopChecks(t *testing.T) {
	r := check(t, head+"hello\nrequires: nowhere\nhow?\n", Options{})
	if r.Errors() != 2 {
		t.Fatalf("ожидались две ⊥syn, получено:\n%s", dump(r.Diagnostics))
	}
	for _, d := range r.Diagnostics {
		if d.Code != ErrSyn {
			t.Fatalf("после ⊥syn проверка знания не выполняется, получено:\n%s", dump(r.Diagnostics))
		}
	}
}

func TestSignalRules(t *testing.T) {
	prog, diags := Parse("тест", head+"??n_disconnect(except: {maintenance_window, night})\n??a_verify_config(scope: web_group)\n"+
		"hard forbid restart()\nhard require backup(scope = {db1, db2})\n")
	if len(diags) != 0 {
		t.Fatalf("разбор:\n%s", dump(diags))
	}
	got := []string{}
	for _, r := range prog.Rules {
		var args []string
		for _, a := range r.Args {
			args = append(args, a.Name+"="+strings.Join(a.Values, "|"))
		}
		got = append(got, string(r.Kind)+" "+r.Action+" "+strings.Join(args, ","))
	}
	want := "forbid disconnect except=maintenance_window|night;require verify_config scope=web_group;" +
		"forbid restart ;require backup scope=db1|db2"
	if strings.Join(got, ";") != want {
		t.Fatalf("правила:\n  %s\nожидалось\n  %s", strings.Join(got, ";"), want)
	}
}

// ---------------------------------------------------------------------------
// Однозначность
// ---------------------------------------------------------------------------

func TestWordMeaningsLimit(t *testing.T) {
	src := "goal: x\nclasses:\n  Goal : \"x\"\n  Fuel : \"petrol\"\n  Obstacle : \"petrol\"\n"
	expectClean(t, check(t, src, Options{}))
	expect(t, check(t, src, Options{MaxMeanings: 1}), ErrAmbig, "допустимо не больше 1")
}

func TestGoalWordMustBeUnambiguous(t *testing.T) {
	expect(t, check(t, "goal: x\nclasses:\n  Goal : \"x\"\n  Place : \"x\"\n", Options{}), ErrAmbig, "Goal или Place")
	expect(t, check(t, "goal: y\nclasses:\n  Goal : \"x\"\n", Options{}), ErrSyn, "слова \"y\" нет в словаре")
}

// Одноимённость (П4) разрешается местом: слот с типом Obstacle выбирает
// у слова «petrol» смысл «преграда», хотя у него их два.
func TestHomonymResolvedBySlotClass(t *testing.T) {
	base := `goal: x
classes:
  Goal       : "x"
  Fuel       : "petrol"
  Obstacle   : "petrol", "rock"
  Trajectory : "path"
generalization detour(obstacle : Obstacle) :
  Trajectory = path_around(obstacle)
import @driving
  provides: path_around
`
	expectClean(t, check(t, base+"instance d = detour(obstacle := petrol)\n", Options{}))
	expectClean(t, check(t, base+"instance d = detour(obstacle := o_17)\n", Options{}))

	src := strings.Replace(base, `Obstacle   : "petrol", "rock"`, `Obstacle   : "rock"`, 1)
	expect(t, check(t, src+"instance d = detour(obstacle := petrol)\n", Options{}), ErrConflict,
		"слово \"petrol\" означает Fuel, а здесь допустим Obstacle")
}

// ---------------------------------------------------------------------------
// Граф и обобщения
// ---------------------------------------------------------------------------

func TestGraphChecks(t *testing.T) {
	expect(t, check(t, head+"relations:\n  Fuel --limits--> Speed\n", Options{}), ErrUndef, "класс Speed не объявлен")
	expect(t, check(t, head+"relations:\n  Fuel --limits--> Movement (detour)\n", Options{}), ErrConflict, "имя detour уже занято")
	expect(t, check(t, head+"relations:\n  Obstacle --forces--> Trajectory (again)\n", Options{}), Note, "правка избыточна")
}

func TestPriorityMustBePartialOrder(t *testing.T) {
	expectClean(t, check(t, head+"priority:\n  Fuel < Goal\n  Obstacle < Fuel < Movement\n", Options{}))
	expect(t, check(t, head+"priority:\n  Fuel < Goal\n  Goal < Movement < Fuel\n", Options{}), ErrConflict, "цикл Fuel < Goal < Movement < Fuel")
	expect(t, check(t, head+"priority:\n  Fuel < Fuel\n", Options{}), ErrConflict, "важнее самого себя")
	expect(t, check(t, head+"priority:\n  Fuel < Speed\n", Options{}), ErrUndef, "класс Speed не объявлен")
}

func TestGeneralizationAndInstance(t *testing.T) {
	gen := head + "import @driving\n  provides: path_around\n"

	// Схема отношения: результат — цель отношения, среди слотов — источник.
	expect(t, check(t, gen+"generalization detour(o : Obstacle) :\n  Fuel = path_around(o)\n", Options{}),
		ErrConflict, "его результат Fuel, а не Trajectory")
	expect(t, check(t, gen+"generalization detour(f : Fuel) :\n  Trajectory = path_around(f)\n", Options{}),
		ErrConflict, "ни один слот не имеет тип Obstacle")

	g := gen + "generalization around(o : Obstacle, side : {left, right}) :\n  Trajectory = path_around(o, side)\n"
	expectClean(t, check(t, g+"instance a = around(o := o_1, side := left)\n", Options{}))
	expect(t, check(t, g+"instance a = around(o := o_1, side := up)\n", Options{}), ErrSyn, "вне домена слота side: {left, right}")
	expect(t, check(t, g+"instance a = around(side := left)\n", Options{}), ErrUndef, "не заполнены слоты: o")
	expect(t, check(t, g+"instance a = around(o := x, o := y, side := left)\n", Options{}), ErrConflict, "заполнен дважды")
	expect(t, check(t, g+"instance a = nowhere(o := x)\n", Options{}), ErrUndef, "обобщения nowhere нет")
	expect(t, check(t, gen+"generalization around(o : Obstacle) :\n  Trajectory = path_around(x)\n", Options{}),
		ErrUndef, "аргумент x — не слот")
}

// ---------------------------------------------------------------------------
// Req(P) ⊆ K и скиллы
// ---------------------------------------------------------------------------

func TestKnowledgeClosure(t *testing.T) {
	r := check(t, head+"requires: detour, fuel_model, weather\n", Options{})
	expect(t, r, ErrUndef, "fuel_model: такого отношения нет")
	expect(t, r, ErrUndef, "weather: такого отношения нет")
	if r.Errors() != 2 {
		t.Fatalf("detour есть в K и отказа не даёт:\n%s", dump(r.Diagnostics))
	}

	expectClean(t, check(t, head+"requires: fuel_model\nimport @fuel\n  provides: fuel_model\n", Options{}))
}

func TestSkillsAgainstLibrary(t *testing.T) {
	library := func(skill string) ([]string, bool) {
		if skill == "driving" {
			return []string{"path_around", "stop"}, true
		}
		return nil, false
	}
	opts := Options{Library: library}

	// Без перечня операций скилл даёт всё, что в нём есть.
	expectClean(t, check(t, head+"import @driving\nrequires: path_around, stop\n", opts))

	// С перечнем — только перечисленное.
	expect(t, check(t, head+"import @driving\n  provides: path_around\nrequires: stop\n", opts), ErrUndef, "stop")
	expect(t, check(t, head+"import @driving\n  provides: fly\n", opts), ErrUndef, "в скилле @driving нет операции fly")
	expect(t, check(t, head+"import @nginx\n", opts), ErrUndef, "скилла @nginx нет в библиотеке")
	expect(t, check(t, head+"import @driving v2.3\n", opts), Note, "версия v2.3 скилла @driving не проверена")

	// Арбитр A2: классы скилла должны быть допустимы для задачи.
	expect(t, check(t, head+"import @driving\n  requires_class: Speed\n", opts), ErrUndef,
		"требует класс Speed, которого нет в промпте")
	expect(t, check(t, head+"import @driving\n  provides: Server\n", opts), ErrUndef, "даёт класс Server")
}

// ---------------------------------------------------------------------------
// Жёсткие правила
// ---------------------------------------------------------------------------

func TestHardRules(t *testing.T) {
	skill := head + "import @ops\n  provides: disconnect, reload\n"

	expectClean(t, check(t, skill+"??n_disconnect(except: {maintenance_window})\nhard require reload(scope = web_group)\n", Options{}))

	// Ж3: «никогда X» и «всегда X» — противоречие до исполнения.
	expect(t, check(t, skill+"??n_disconnect()\n??a_disconnect(scope: web)\n", Options{}), ErrConflict, "несовместно")
	expect(t, check(t, skill+"hard forbid reload()\nhard forbid reload()\n", Options{}), Note, "правка избыточна")

	// Ж1: действие, которого не знает ни один скилл, по плану не проверить.
	expect(t, check(t, skill+"hard forbid restart()\n", Options{}), ErrUndef, "арбитру нечем проверить правило по плану (Ж1)")
	expect(t, check(t, skill+"hard forbid reload(except = a, except = b)\n", Options{}), ErrConflict, "параметр except правила reload повторяется")
}

// ---------------------------------------------------------------------------
// Ячейки
// ---------------------------------------------------------------------------

func TestCellDependenciesByReadAndWrite(t *testing.T) {
	src := head + `
cell c1:
  set Fuel.reserve = 10 %
cell c2:
  set detour.side = right
cell c3:
  set detour.side.margin = 2 m
cell c4:
  set Trajectory.limit = detour.side.margin
`
	r := check(t, src, Options{})
	expectClean(t, r)

	want := map[string]string{"c1": "", "c2": "c3,c4", "c3": "c4", "c4": ""}
	for cell, deps := range want {
		if got := strings.Join(r.Dependents[cell], ","); got != deps {
			t.Errorf("D(%s) = %q, ожидалось %q", cell, got, deps)
		}
	}
}

func TestCellConsistencyLevels(t *testing.T) {
	forward := head + "cell c1:\n  set Fuel.reserve = Trajectory.length\ncell c2:\n  set Trajectory.length = 5 km\n"
	expect(t, check(t, forward, Options{}), ErrUndef, "задаётся позже, в ячейке c2")
	expect(t, check(t, forward, Options{Consistency: Reconcile}), Note, "задаётся позже, в ячейке c2")
	expectClean(t, check(t, forward, Options{Consistency: Reconcile}))

	// Удалили ячейку, которая задавала значение: зависимая ссылается в никуда.
	removed := head + "cell c3:\n  set Fuel.reserve = Trajectory.length\n"
	expect(t, check(t, removed, Options{}), ErrUndef, "не задаёт ни одна ячейка")

	expect(t, check(t, head+"cell c1:\n  set Speed.limit = 5\n", Options{}), ErrUndef, "класс Speed не объявлен")
	expect(t, check(t, head+"cell c1:\n  set nothing.x = 5\n", Options{}), ErrUndef, "nothing не объявлено")
	expect(t, check(t, head+"cell c1:\n  set Fuel.x = 1\n  set Fuel.x = 2\n", Options{}), ErrConflict, "задаёт Fuel.x дважды")
	expect(t, check(t, head+"cell c1:\n  set Fuel.x = 1\ncell c2:\n  set Fuel.x = 2\n", Options{}), Note, "действует последняя запись")
	expect(t, check(t, head+"cell c1:\n  set Fuel = 1\n", Options{}), ErrSyn, "set задаёт атрибут")
	expect(t, check(t, head+"cell c1:\n  binds_to: a.go:1-2\n  binds_to: b.go:1-2\n", Options{}), ErrConflict, "одна привязка к коду")
}

// ---------------------------------------------------------------------------
// Отчёт
// ---------------------------------------------------------------------------

func TestReportChecks(t *testing.T) {
	base := head + "priority:\n  Fuel < Goal < Movement\ncell c1:\n  set detour.side = left\n"
	entry := func(lines ...string) string {
		return base + "report:\n  q: fuel_policy\n    " + strings.Join(lines, "\n    ") + "\n"
	}

	expectClean(t, check(t, entry("choice: stop", "alternatives: {stop, go}", "basis: priority(Fuel < Goal)"), Options{}))
	// Приоритет по цепочке тоже основание.
	expectClean(t, check(t, entry("choice: stop", "basis: priority(Fuel < Movement)"), Options{}))

	expect(t, check(t, entry("choice: fly", "alternatives: {stop, go}", "basis: not_in_prompt"), Options{}), ErrConflict, "не входит в альтернативы")
	expect(t, check(t, entry("choice: stop", "basis: priority(Goal < Fuel)"), Options{}), ErrUndef, "такого приоритета в промпте нет")
	expect(t, check(t, entry("choice: stop", "basis: relation(none)"), Options{}), ErrUndef, "отношения none в промпте нет")
	expect(t, check(t, entry("choice: stop", "basis: skill(@nginx)"), Options{}), ErrUndef, "скилл не подключён")
	expect(t, check(t, entry("choice: stop", "basis: rule(restart)"), Options{}), ErrUndef, "такого правила нет")
	expect(t, check(t, entry("choice: stop"), Options{}), ErrSyn, "нет basis")
	expect(t, check(t, entry("choice: stop", "basis: hunch"), Options{}), ErrSyn, "основание — not_in_prompt")
	expect(t, check(t, entry("choice: stop", "basis: not_in_prompt"), Options{Quiet: []string{"fuel_policy"}}), Note, "Q_off")

	// Модель не переопределяет то, что задал промпт (П5).
	fixed := base + "report:\n  q: side\n    choice: right\n    basis: not_in_prompt\n"
	expect(t, check(t, fixed, Options{}), ErrConflict, "промпт задаёт detour.side = left, а отчёт выбрал right")

	dup := entry("choice: stop", "basis: not_in_prompt") + "  q: fuel_policy\n    choice: go\n    basis: not_in_prompt\n"
	expect(t, check(t, dup, Options{}), ErrConflict, "уже есть в отчёте")
}

func TestDiagnosticsAreSorted(t *testing.T) {
	r := check(t, head+"requires: zz\nrequires: aa\nhard forbid mm()\n", Options{})
	for i := 1; i < len(r.Diagnostics); i++ {
		a, b := r.Diagnostics[i-1].Pos, r.Diagnostics[i].Pos
		if a.Line > b.Line || (a.Line == b.Line && a.Col > b.Col) {
			t.Fatalf("находки не упорядочены:\n%s", dump(r.Diagnostics))
		}
	}
}

// Разбор получает текст от модели: на любом входе он отвечает отказом,
// а не падает.
func FuzzCheckText(f *testing.F) {
	for _, name := range []string{"spec.fp", "navigation.fp"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(data))
	}
	f.Add(head + "??n_x(except: {a}\ncell c:\n  set A.b.c = 1 km/\nreport:\n  choice: x\n  q:\n")
	f.Fuzz(func(t *testing.T, src string) {
		r := CheckText("fuzz.fp", src, Options{MaxMeanings: 2, Library: func(string) ([]string, bool) { return nil, true }})
		for _, d := range r.Diagnostics {
			if d.Pos.Line < 1 || d.Pos.Col < 1 {
				t.Fatalf("находка без места: %s", d)
			}
		}
	})
}

// Примеры протокола — это то, по чему модель учится языку. Каждый из них
// обязан разбираться: пример с ошибкой научил бы ошибке.
func TestProtocolExamplesParse(t *testing.T) {
	var blocks []string
	inBlock, skip := false, false
	var current strings.Builder
	for _, line := range strings.Split(Protocol(), "\n") {
		switch {
		case strings.HasPrefix(line, "```") && !inBlock:
			inBlock, skip = true, strings.TrimPrefix(line, "```") != ""
			current.Reset()
		case strings.HasPrefix(line, "```"):
			inBlock = false
			if !skip {
				blocks = append(blocks, current.String())
			}
		case inBlock:
			current.WriteString(line + "\n")
		}
	}
	if len(blocks) < 7 {
		t.Fatalf("в протоколе %d примеров, ожидалось не меньше 7", len(blocks))
	}

	program := strings.Join(blocks, "\n")
	if _, diags := Parse("протокол", program); len(diags) > 0 {
		t.Fatalf("примеры протокола не разбираются:\n%s\n%s", dump(diags), program)
	}
}
