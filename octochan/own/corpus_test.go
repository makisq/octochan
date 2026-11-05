package own

import (
	"strings"
	"testing"

	"ochan/fp"
)

// Корпус: каждую пару принял разбор ochan, ответ — логика ochan, близнец
// без предпосылки — с ответом без решений; отложенные — целыми группами.
func TestCorpus(t *testing.T) {
	pairs, rep := Corpus(CorpusOptions{Count: 300, Seed: 7, Test: 0.1, H3: 0.5})
	if len(pairs) < 300 || rep.Train == 0 || rep.Train == len(pairs) {
		t.Fatalf("пар %d, обучение %d: %+v", len(pairs), rep.Train, rep)
	}
	for _, kind := range []string{"перенос", "без предпосылки", "конфликт"} {
		if rep.Kinds[kind] == 0 {
			t.Fatalf("нет пар вида %q: %v", kind, rep.Kinds)
		}
	}
	opts := corpusOptions(fp.BaseLexicon())
	tok, _ := BuildTokenizer(testAlphabet(t), true)
	for i, p := range pairs {
		if ok, why := checkPair(p.Task, p.Answer, opts); !ok {
			t.Fatalf("пара %d не принята: %s\n%s", i, why, p.Task)
		}
		r := fp.CheckText("task", p.Task, opts)
		if undef(r) == 0 {
			plan := fp.PlanAnswer(p.Task, r.Program)
			if fp.Canonical(p.Answer) != fp.Canonical(plan.Text()) {
				t.Fatalf("ответ пары %d — не логика ochan:\n%s\n%s", i, p.Answer, plan.Text())
			}
		} else if strings.Contains(p.Answer, "report:") {
			t.Fatalf("у задачи без предпосылки есть решения:\n%s", p.Answer)
		}
		if err := p.Tokenize(tok); err != nil || p.IDs[0] != tok.Bos || p.IDs[len(p.IDs)-1] != tok.Eos || p.IDs[p.Start-1] != tok.Answer {
			t.Fatalf("токены пары %d: %v", i, err)
		}
	}
	again, _ := Corpus(CorpusOptions{Count: 300, Seed: 7, Test: 0.1, H3: 0.5})
	if again[5].Task != pairs[5].Task {
		t.Fatal("тот же seed — другой корпус")
	}
}
