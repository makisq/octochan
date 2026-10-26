package workspace

import (
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
	if counts["a"] != (Counts{Uses: 2, Successes: 1}) || counts["b"] != (Counts{Uses: 1, Successes: 1}) {
		t.Fatalf("счёт правил: %+v", counts)
	}
	if got := Decided(all); len(got) != 2 || got[0].ScriptSum != "s1" || got[1].ScriptSum != "s2" {
		t.Fatalf("планы с исходом: %+v", got)
	}
}

// Противоречие ложится на своё утверждение, а не на соседей по плану;
// неудача плана целиком — на всех.
func TestFailureAttribution(t *testing.T) {
	bad := decision("p", "s1", 1, ArbiterRejected, "", "set_param", "write")
	bad.Packages[1].Verdict = "contradiction"
	missing := decision("p", "s2", 2, ArbiterRejected, "", "set_param", "store")
	counts := RuleCounts([]*Decision{bad, missing})
	want := map[string]Counts{"write": {Uses: 1}, "set_param": {Uses: 1}, "store": {Uses: 1}}
	for rule, c := range want {
		if counts[rule] != c {
			t.Errorf("%s: %+v, ожидалось %+v", rule, counts[rule], c)
		}
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
