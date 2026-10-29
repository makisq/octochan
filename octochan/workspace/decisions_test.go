package workspace

import (
	"math"
	"testing"
	"time"
)

func decision(promptSum, scriptSum string, at int, arbiter, operator string, rules ...string) *Decision {
	d := &Decision{PromptSum: promptSum, ScriptSum: scriptSum, Time: time.Unix(int64(at), 0).UTC(),
		Arbiter: arbiter, Operator: operator}
	for _, rule := range rules {
		d.Packages = append(d.Packages, DecisionPackage{Statement: rule, Rules: []string{rule}, Verdict: "well_true"})
	}
	return d
}

func TestOutcomeIsBlindToRing2(t *testing.T) {
	cases := []struct {
		name             string
		d                *Decision
		correct, decided bool
	}{
		{"план без заявки", decision("p", "s", 1, ArbiterPassed, ""), false, false},
		{"арбитр отверг", decision("p", "s", 1, ArbiterRejected, ""), false, true},
		{"оператор согласовал", decision("p", "s", 1, ArbiterPassed, OperatorApproved), true, true},
		{"оператор откатил", decision("p", "s", 1, ArbiterPassed, OperatorRolledBack), false, true},
	}
	for _, c := range cases {
		if correct, decided := c.d.Outcome(); correct != c.correct || decided != c.decided {
			t.Errorf("%s: (%v, %v)", c.name, correct, decided)
		}
	}

	// Отказ кольца 2 исходом не считается: иначе журнал подтверждал бы
	// сам себя. Противоречие кольца 3 — булева проверка, считается.
	weak := decision("p", "s", 1, ArbiterPassed, "", "a")
	weak.Packages[0].Verdict = "well_false"
	if _, decided := weak.Outcome(); decided {
		t.Error("отказ кольца 2 засчитан исходом")
	}
	if !weak.Rejected() {
		t.Error("план с отказом кольца 2 не отвергнут")
	}
	weak.Packages[0].Verdict = "contradiction"
	if correct, decided := weak.Outcome(); correct || !decided {
		t.Error("противоречие кольца 3 не засчитано неудачей")
	}
}

func TestRuleCountsOncePerPlan(t *testing.T) {
	all := []*Decision{
		// Один план показан трижды: считается по последнему вердикту.
		decision("p", "s1", 1, ArbiterRejected, "", "a", "b"),
		decision("p", "s1", 2, ArbiterPassed, OperatorApproved, "a", "b"),
		decision("p", "s1", 3, ArbiterPassed, OperatorApproved, "a", "b"),
		// Правило дважды в одном плане — одно применение.
		decision("p", "s2", 4, ArbiterPassed, OperatorRejected, "a", "a"),
		// Без вердикта не считается.
		decision("q", "s3", 5, ArbiterPassed, "", "b"),
	}
	counts := RuleCounts(all)
	if raw(counts["a"]) != (Counts{Uses: 2, Successes: 1}) || raw(counts["b"]) != (Counts{Uses: 1, Successes: 1}) {
		t.Fatalf("счёт правил: %+v", counts)
	}
	if got := Decided(all); len(got) != 2 || got[0].ScriptSum != "s1" || got[1].ScriptSum != "s2" {
		t.Fatalf("планы с исходом: %+v", got)
	}
}

// Нарушение запрета ложится на модель и промпт, а не на правила скиллов;
// нарушенное «всегда» и решение оператора — на все утверждения плана.
func TestFailureAttribution(t *testing.T) {
	// Кольцо 3: write нарушил запрет.
	contradiction := decision("p", "s1", 1, ArbiterRejected, "", "set_param", "write")
	contradiction.Packages[1].Verdict = "contradiction"
	contradiction.ArbiterReasons = []string{"hard forbid write()"}
	// Арбитр: только запрет.
	forbidden := decision("p", "s2", 2, ArbiterRejected, "", "set_param", "store")
	forbidden.ArbiterReasons = []string{"hard forbid store(except = {prod_conf})"}
	// Арбитр: не хватило «всегда».
	missing := decision("p", "s3", 3, ArbiterRejected, "", "set_param", "store")
	missing.ArbiterReasons = []string{"hard require verify(scope = {web})"}
	// Причина не записана — как прежде.
	unknown := decision("q", "s4", 4, ArbiterRejected, "", "reload")
	// Оператор откатил.
	rolledBack := decision("q", "s5", 5, ArbiterPassed, OperatorRolledBack, "reload")

	counts := RuleCounts([]*Decision{contradiction, forbidden, missing, unknown, rolledBack})
	want := map[string]Counts{"write": {}, "set_param": {Uses: 1}, "store": {Uses: 1}, "reload": {Uses: 2}}
	for rule, c := range want {
		if raw(counts[rule]) != c {
			t.Errorf("%s: %+v, ожидалось %+v", rule, counts[rule], c)
		}
	}

	// План с нарушением запрета для калибровки — по-прежнему неверный план.
	if correct, decided := contradiction.Outcome(); correct || !decided {
		t.Error("план с нарушением запрета засчитан верным или без исхода")
	}
}

func TestBlames(t *testing.T) {
	llm := func(d *Decision) *Decision { d.Role = "llm"; d.Prompt = "task.fp"; return d }
	a := llm(decision("p1234567890", "s1", 1, ArbiterRejected, "", "store"))
	a.ArbiterReasons = []string{"hard forbid store(except = {prod_conf})"}
	b := llm(decision("p1234567890", "s2", 2, ArbiterRejected, "", "store"))
	b.Packages[0].Verdict = "contradiction"
	// Тот же план показан ещё раз — считается один раз.
	again := llm(decision("p1234567890", "s2", 3, ArbiterRejected, "", "store"))
	again.Packages[0].Verdict = "contradiction"
	// Нарушенное «всегда» — не нарушение запрета.
	c := llm(decision("p1234567890", "s3", 4, ArbiterRejected, "", "store"))
	c.ArbiterReasons = []string{"hard require verify()"}
	operator := decision("other", "s4", 5, ArbiterPassed, OperatorApproved, "store")
	operator.Role, operator.Prompt = "master", "other.fp"

	byPrompt, byRole := Blames([]*Decision{a, b, again, c, operator})
	if got := byPrompt["task.fp (p1234567)"]; got != (Blame{Plans: 3, Violations: 2}) {
		t.Errorf("промпт: %+v (%v)", got, byPrompt)
	}
	if got := byRole["llm"]; got != (Blame{Plans: 3, Violations: 2}) {
		t.Errorf("llm: %+v", got)
	}
	if got := byRole["master"]; got != (Blame{Plans: 1}) {
		t.Errorf("master: %+v", got)
	}
}

func TestDecisionsStore(t *testing.T) {
	j, err := NewDecisions(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := decision("p", "s", 2, ArbiterPassed, "", "a")
	second := decision("p", "s", 1, ArbiterPassed, "", "a")
	for _, d := range []*Decision{first, second} {
		if err := j.Append(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.SetOperator(first.ID, OperatorApproved, "r1"); err != nil {
		t.Fatal(err)
	}
	all, err := j.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != second.ID || all[1].Operator != OperatorApproved || all[1].Request != "r1" {
		t.Fatalf("журнал: %+v %+v", all[0], all[1])
	}
	if err := j.SetOperator("nope", OperatorApproved, ""); err == nil {
		t.Fatal("решение по несуществующей записи принято")
	}
}

// raw — счётчики без весов давности.
func raw(c Counts) Counts { return Counts{Uses: c.Uses, Successes: c.Successes} }

// Отказ не по плану и поздняя поправка: правилам засчитывается только то,
// что говорит о плане.
func TestOperatorKindsAndAmendments(t *testing.T) {
	timing := decision("p", "s1", 1, ArbiterPassed, OperatorRejected, "store")
	timing.OperatorKind = KindTiming
	wrongPlan := decision("p", "s2", 2, ArbiterPassed, OperatorRejected, "store")
	wrongPlan.OperatorKind = KindPlan
	incident := decision("p", "s3", 3, ArbiterPassed, OperatorApproved, "store")
	incident.Amendments = []Amendment{{Correct: false, Reason: "INC-1"}}
	counts := RuleCounts([]*Decision{timing, wrongPlan, incident})
	if raw(counts["store"]) != (Counts{Uses: 2, Successes: 0}) {
		t.Fatalf("store: %+v", counts["store"])
	}

	// Затухание: исход возраста half-life весит вдвое меньше.
	old := decision("q", "o1", 0, ArbiterPassed, OperatorApproved, "reload")
	now := old.Time.Add(90 * 24 * time.Hour)
	c := RuleCountsAt([]*Decision{old}, now, 90*24*time.Hour)["reload"]
	if c.Uses != 1 || math.Abs(c.WUses-0.5) > 1e-9 || math.Abs(c.WSuccesses-0.5) > 1e-9 {
		t.Fatalf("затухание: %+v", c)
	}

	pooled := PooledExcept(map[string]Counts{"pg.a": {Uses: 3, Successes: 1}, "pg.b": {Uses: 2, Successes: 2},
		"other.c": {Uses: 9, Successes: 9}}, "pg.b")
	if pooled.Uses != 3 || pooled.Successes != 1 {
		t.Fatalf("пакет: %+v", pooled)
	}
}

// Выход из слота обобщения и отчёт, расходящийся с планом, — на счёт
// модели: правила скиллов они не позорят.
func TestModelFaultsDoNotBlameRules(t *testing.T) {
	slot := decision("p", "s1", 1, ArbiterRejected, "", "store")
	slot.ArbiterReasons = []string{"generalization raise(value : 100..500) = set_param"}
	report := decision("p", "s2", 2, ArbiterRejected, "", "store")
	report.ArbiterReasons = []string{"НЕТ В ОТЧЁТЕ prod_conf.max_connections = 300 — литерал сценария"}
	require := decision("p", "s3", 3, ArbiterRejected, "", "store")
	require.ArbiterReasons = []string{"hard require verify(scope = {web})"}
	if c := raw(RuleCounts([]*Decision{slot, report, require})["store"]); c != (Counts{Uses: 1}) {
		t.Fatalf("store: %+v", c)
	}
	if !slot.Violated() || !report.Violated() || require.Violated() {
		t.Fatal("вина модели")
	}
}
