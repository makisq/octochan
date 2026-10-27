package fp

import (
	"strings"
	"testing"
)

// Базовый словарь годен: у каждого слова от одного смысла, их меньше,
// чем в естественном языке, а ключевых слов в нём нет.
func TestBaseLexicon(t *testing.T) {
	lex, err := ParseLexicon("base", baseLexicon)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(lex.Words()); n < 150 {
		t.Fatalf("в базовом словаре %d слов", n)
	}
	for _, w := range lex.Words() {
		e, _ := lex.Lookup(w)
		if e.N() < 1 || (e.NL > 1 && e.N() >= e.NL) || (e.NL == 1 && e.N() != 1) {
			t.Errorf("%s: n = %d, nl = %d", w, e.N(), e.NL)
		}
		if IsKeyword(w) {
			t.Errorf("%s — ключевое слово", w)
		}
	}
	for _, w := range []string{"before", "around", "max"} {
		if !lex.IsFunction(w) {
			t.Errorf("%s — не служебное слово", w)
		}
	}
	if e, _ := lex.Lookup("server"); strings.Join(e.Senses, ",") != "Node,Service" || e.NL != 4 {
		t.Errorf("server: %+v", e)
	}
	// Базовый словарь отдаётся копией: слияние со словарём скилла его
	// не меняет.
	copy := BaseLexicon()
	if err := copy.Merge(drivingLexicon(t)); err != nil {
		t.Fatal(err)
	}
	if _, ok := BaseLexicon().Lookup("rock"); ok {
		t.Fatal("слияние изменило общий базовый словарь")
	}
}

func TestLexiconRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"words:\n  rock: {senses: [Obstacle, Genre, Music, Stone], nl: 4}\n": "смыслов 4, а в естественном языке 4",
		"words:\n  petrol: {senses: [Fuel, Obstacle], nl: 1}\n":              "в естественном языке у слова один смысл",
		"words:\n  rock: {senses: [Obstacle]}\n":                             "не указано nl",
		"words:\n  rock: {senses: [], nl: 9}\n":                              "нет смыслов",
		"words:\n  rock: {senses: [obstacle], nl: 9}\n":                      "класс с заглавной латинской буквы",
		"words:\n  rock: {senses: [Obstacle, Obstacle], nl: 9}\n":            "смысл Obstacle повторяется",
		"words:\n  set: {senses: [Setting], nl: 9}\n":                        "ключевое слово языка",
		"words:\n  hot_rod: {senses: [Car], nl: 2}\n":                        "составные имена",
		"words:\n  rock: {senses: [Obstacle], nl: 9, note: x}\n":             "note",
		"words:\n  rock: {senses: [Obstacle], nl: 9}\nfunction: [rock]\n":    "и служебное, и со смыслом",
		"units: [\"%\"]\n": "единица",
		"words: [rock]\n":  "cannot unmarshal",
	}
	for src, want := range cases {
		if _, err := ParseLexicon("t.yaml", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, ожидалось %q", src, err, want)
		}
	}
	if lex, err := ParseLexicon("empty.yaml", nil); err != nil || len(lex.Words()) != 0 {
		t.Fatalf("пустой словарь: %v", err)
	}
}

// Словари сливаются: смыслы одного слова объединяются, но язык и после
// слияния строже естественного.
func TestLexiconMerge(t *testing.T) {
	parse := func(src string) *Lexicon {
		t.Helper()
		lex, err := ParseLexicon("t.yaml", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return lex
	}
	lex := parse("words:\n  rock: {senses: [Obstacle], nl: 3}\n")
	if err := lex.Merge(parse("words:\n  rock: {senses: [Genre], nl: 3}\n")); err != nil {
		t.Fatal(err)
	}
	if e, _ := lex.Lookup("rock"); strings.Join(e.Senses, ",") != "Obstacle,Genre" {
		t.Fatalf("смыслы после слияния: %v", e.Senses)
	}
	if err := lex.Merge(parse("words:\n  rock: {senses: [Stone], nl: 3}\n")); err == nil ||
		!strings.Contains(err.Error(), "смыслов 3, а в естественном языке 3") {
		t.Fatalf("слияние до n = nl: %v", err)
	}
	if err := lex.Merge(parse("words:\n  rock: {senses: [Genre], nl: 9}\n")); err == nil ||
		!strings.Contains(err.Error(), "nl 3 в t.yaml и 9 в t.yaml") {
		t.Fatalf("разные nl: %v", err)
	}
	if err := lex.Merge(parse("function: [rock]\n")); err == nil || !strings.Contains(err.Error(), "служебное") {
		t.Fatalf("служебное и со смыслом: %v", err)
	}
}

// Ключевые слова заняты грамматикой: ни именем, ни словом словаря.
func TestKeywordsAreReserved(t *testing.T) {
	cases := map[string]string{
		"ячейка":           head + "cell set:\n  set Fuel.reserve = 10 %\n",
		"частный случай":   head + "instance rule = detour(obstacle := o_1)\n",
		"слово словаря":    "goal: northwest\nclasses:\n  Direction : \"northwest\", \"scope\"\n",
		"действие сигнала": head + "??n_report()\n",
		"имя отношения":    head + "relations:\n  Fuel --limits--> Movement (scope)\n",
		"значение правила": head + "hard forbid stop(except = {relation})\n",
		"атрибут":          head + "cell c1:\n  set Fuel.choice = 1\n",
		"операция скилла":  head + "import @driving\n  provides: set\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			expect(t, check(t, src, Options{}), ErrSyn, "ключевое слово языка")
		})
	}
}

// Язык замкнут: слово со смыслом — только из словаря. Слова области
// приносит словарь скилла, и только того, что подключён.
func TestClosedVocabulary(t *testing.T) {
	driving := drivingLexicon(t)
	library := map[string]*Skill{
		"driving": {Ops: []string{"path_around", "stop"}, Version: "2.3.1", Lexicon: driving},
		"ops":     {Ops: []string{"reload"}, Version: "1.0"},
	}
	opts := Options{
		Lexicon: BaseLexicon(),
		Library: func(name string) (*Skill, bool) { s, ok := library[name]; return s, ok },
		Skills:  func() []string { return []string{"driving", "ops"} },
	}
	prompt := "goal: northwest\nclasses:\n  Direction : \"northwest\"\n  Fuel : \"fuel\"\n"

	// Слово области без скилла — вне языка, и подсказка называет скилл.
	r := CheckText("t.fp", prompt+"import @ops\n", opts)
	expect(t, r, ErrSyn, "слова \"fuel\" нет в словаре языка")
	expectHint(t, r, ErrSyn, "есть в словаре @driving v2.3.1 — подключите: import @driving v2.3.1")
	// Скилл приносит свои слова.
	expectClean(t, CheckText("t.fp", prompt+"import @driving\n", opts))

	// Слова нет нигде: подсказка — похожее слово.
	r = CheckText("t.fp", "goal: northwest\nclasses:\n  Direction : \"nortwest\"\n", opts)
	expect(t, r, ErrSyn, "слова \"nortwest\" нет в словаре языка")
	expectHint(t, r, ErrSyn, "возможно, имелось в виду northwest")
	expect(t, CheckText("t.fp", "goal: northwest\nclasses:\n  Direction : \"northwest\", \"before\"\n", opts),
		ErrSyn, "служебное слово \"before\" класса не означает")

	// Без библиотеки словари скиллов не подключить: слово вне базового
	// словаря — замечание, а не отказ.
	standalone := CheckText("t.fp", prompt+"import @driving\n", Options{})
	expectClean(t, standalone)
	expect(t, standalone, Note, "словарь @driving без библиотеки скиллов не подключается")

	// Словарь скилла негоден — промпт с ним не проверить.
	library["broken"] = &Skill{LexiconErr: errString("слово \"rock\": не указано nl")}
	expect(t, CheckText("t.fp", prompt+"import @broken\n", opts), ErrUndef, "словарь скилла @broken негоден")
}

type errString string

func (e errString) Error() string { return string(e) }

// withDriving — библиотека со скиллом @driving и его словарём.
func withDriving(t *testing.T, opts Options) Options {
	t.Helper()
	driving := &Skill{Ops: []string{"path_around", "stop"}, Version: "2.3.1", Lexicon: drivingLexicon(t)}
	opts.Library = func(name string) (*Skill, bool) { return driving, name == "driving" }
	return opts
}

// Имя со смыслом — слово словаря или составное из слов словаря через _:
// служебные, ключевые слова и числа тоже годятся частями.
func TestMeaningfulNames(t *testing.T) {
	ok := []string{
		"relations:\n  Fuel --limits--> Movement (fuel_budget)\n",
		"relations:\n  Fuel --limits--> Movement (stop_before_detour)\n",
		"relations:\n  Fuel --limits--> Movement (limit_2)\n",
		"generalization around(obstacle : Obstacle, side : {left, right}) :\n  Trajectory = path_around(obstacle, side)\n" +
			"import @driving\n  provides: path_around\n",
	}
	for _, src := range ok {
		expectClean(t, check(t, head+src, Options{}))
	}
	bad := map[string]string{
		"relations:\n  Fuel --limits--> Movement (fuel_xyz)\n":                                                                          "имя отношения fuel_xyz: слова xyz нет в словаре языка",
		"relations:\n  Fuel --limits--> Movement (fuel__budget)\n":                                                                      "слова «_» нет",
		"generalization around(o : Obstacle) :\n  Trajectory = path_around(o)\nimport @driving\n  provides: path_around\n":              "слот o: слова o нет",
		"generalization around(side : {left, up_hill}) :\n  Trajectory = path_around(side)\nimport @driving\n  provides: path_around\n": "значение домена up_hill: слова hill нет",
	}
	for src, want := range bad {
		expect(t, check(t, head+src, withDriving(t, Options{})), ErrSyn, want)
	}
	// Слово с числом смыслов выше потолка частью имени тоже не годится.
	expect(t, check(t, head+"relations:\n  Fuel --limits--> Movement (rock_limit)\n", Options{MaxMeanings: 1}),
		ErrSyn, "слова rock нет в словаре языка")

	// Имена частных случаев открыты: это имена файлов и узлов, а не слова.
	expectClean(t, check(t, head+"import @driving\n  provides: stop\nhard forbid stop(except = {prod_conf, web_1})\n"+
		"cell c7:\n  set detour.work_mem = o_17\n", withDriving(t, Options{})))
}

func TestUnits(t *testing.T) {
	expectClean(t, check(t, head+"cell c1:\n  set Fuel.reserve = 30 km/h\n  set Fuel.size = 4 MB\n  set Fuel.share = 10 %\n", Options{}))
	r := check(t, head+"cell c1:\n  set Fuel.reserve = 10 bananas\n", Options{})
	expect(t, r, ErrSyn, "единицы bananas нет в словаре языка")
	expectHint(t, r, ErrSyn, "km")
	expect(t, check(t, head+"cell c1:\n  set Fuel.reserve = 10 km/fortnight\n", Options{}), ErrSyn, "единицы fortnight")
}

// Отчёт пишет модель: в нём только слова словаря и имена, которые уже
// есть в промпте.
func TestReportIntroducesNoNames(t *testing.T) {
	base := head + "import @driving\n  provides: stop\nhard forbid stop(except = {prod_conf})\ncell c1:\n  set detour.work_mem = 4 MB\n"
	entry := func(lines ...string) string {
		return base + "report:\n  " + strings.Join(lines, "\n    ") + "\n"
	}
	expectClean(t, check(t, entry("q: fuel_policy", "choice: stop_before_detour", "alternatives: {stop_before_detour, detour_anyway}",
		"basis: not_in_prompt"), withDriving(t, Options{})))
	// Имя из промпта годится, даже если его нет в словаре.
	expectClean(t, check(t, entry("q: work_mem", "choice: 4 MB", "basis: not_in_prompt"), withDriving(t, Options{})))
	expectClean(t, check(t, entry("q: target", "choice: prod_conf", "basis: rule(stop)"), withDriving(t, Options{})))

	expect(t, check(t, entry("q: fuel_policy", "choice: teleport", "basis: not_in_prompt"), withDriving(t, Options{})), ErrSyn,
		"выбор teleport: слова teleport нет ни в словаре языка, ни в промпте — отчёт не вводит новых имён")
	expect(t, check(t, entry("q: vibes", "choice: stop", "basis: not_in_prompt"), withDriving(t, Options{})), ErrSyn, "вопрос vibes")
	expect(t, check(t, entry("q: fuel_policy", "choice: stop", "alternatives: {stop, warp}", "basis: not_in_prompt"),
		withDriving(t, Options{})), ErrSyn, "альтернатива warp")
	expect(t, check(t, entry("q: fuel_policy", "choice: 3 parsecs", "basis: not_in_prompt"), withDriving(t, Options{})), ErrSyn,
		"единицы parsecs")
}
