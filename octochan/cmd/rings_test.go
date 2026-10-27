package cmd

import (
	"strings"
	"testing"

	"octochan/lang"
	"octochan/workspace"
)

// Утверждения модели — вызовы скиллов и записи вне скиллов; что скилл
// делает внутри, принадлежит его вызову.
func TestStatements(t *testing.T) {
	calls := []lang.Invocation{
		{ID: 1, Name: "helper"},
		{ID: 2, Name: "store", Module: "pgconf", Args: []string{"prod.conf"}, Outer: []int{1}},
		{ID: 3, Name: "inner", Module: "pgconf", Outer: []int{1, 2}},
		{ID: 4, Name: "verify", Module: "pgconf"},
	}
	effects := []lang.Effect{
		{Kind: lang.EffectSave, Path: "/a", Calls: []int{1, 2, 3}},
		{Kind: lang.EffectWrite, Path: "/b", Calls: []int{1}},
		{Kind: lang.EffectLive, Path: "postgres://db"},
		{Kind: lang.EffectSave, Path: "/c"},
	}
	got := statements(effects, calls)
	var texts []string
	for _, st := range got {
		texts = append(texts, st.text+" | "+st.rule)
	}
	want := []string{"pgconf.store(prod.conf) | pgconf.store", "pgconf.verify() | pgconf.verify",
		"write /b | write", "save /c | save"}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("утверждения:\n%s", strings.Join(texts, "\n"))
	}
	store := got[0]
	if len(store.calls) != 2 || store.calls[1].ID != 3 || len(store.effects) != 1 || store.effects[0].Path != "/a" {
		t.Fatalf("трасса вызова скилла: %+v", store)
	}
}

func TestBudget(t *testing.T) {
	rejected := func(reason string) *workspace.Decision {
		return &workspace.Decision{Packages: []workspace.DecisionPackage{{Statement: "s", Verdict: "contradiction", Reason: reason}}}
	}
	passed := &workspace.Decision{Packages: []workspace.DecisionPackage{{Statement: "s", Verdict: "well_true"}}}
	cfg := ringsConfig{budget: 3}

	if err := checkBudget(cfg, []*workspace.Decision{rejected("a"), rejected("b")}); err != nil {
		t.Fatalf("два разных отказа подряд: %v", err)
	}
	if err := checkBudget(cfg, []*workspace.Decision{rejected("a"), rejected("b"), rejected("c")}); err == nil ||
		!strings.Contains(err.Error(), "бюджет fp.rings.budget.plan исчерпан") {
		t.Fatalf("бюджет не исчерпан: %v", err)
	}
	if err := checkBudget(cfg, []*workspace.Decision{rejected("a"), rejected("a")}); err == nil ||
		!strings.Contains(err.Error(), "дважды подряд отвергнут по одной причине") {
		t.Fatalf("та же причина дважды: %v", err)
	}
	// Прошедший план обнуляет счёт; бюджет 0 — без предела.
	if err := checkBudget(cfg, []*workspace.Decision{rejected("a"), rejected("b"), passed, rejected("c")}); err != nil {
		t.Fatalf("счёт не обнулился: %v", err)
	}
	if err := checkBudget(ringsConfig{}, []*workspace.Decision{rejected("a"), rejected("b"), rejected("c")}); err != nil {
		t.Fatalf("бюджет 0: %v", err)
	}
}

// setRings задаёт fp.rings.<key> на время теста.
func setRings(t *testing.T, key string, value any) {
	t.Helper()
	setting(t, "fp.rings."+key, value)
}

// Кольца в режиме протокола: утверждение с поддержкой ниже порога
// well_True отвергается, план с ним не проходит, а решение оператора по
// заявке становится надёжностью правил.
func TestRingsWellTrueAndJournal(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf})\n")
	e.write(e.path("good.ochan"), `c = config("prod.conf")
put(c, "max_connections", "200")
pgconf.verify(c)
pgconf.store(c, "prod.conf")
`)

	// Журнала нет — действует условный счёт 8/8, [0,66; 1,00]: проходит и
	// порог плана, и порог заявки. Оценки по умолчанию не видны.
	out := e.must("plan", "good.ochan", "--prompt", "task.fp")
	for _, want := range []string{"Кольца (режим протокола, порог well_True 0.50):",
		"well_True     pgconf.verify(", "well_True     pgconf.store(", "соблюдено  hard forbid save"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[0.") {
		t.Fatalf("оценки видны без fp.rings.show_scores:\n%s", out)
	}
	if out := e.must("fp", "journal"); !strings.Contains(out, "планов 1, с исходом 0") {
		t.Fatalf("журнал после плана:\n%s", out)
	}

	created := e.must("request", "good.ochan", "--prompt", "task.fp")
	id := requestIDPattern.FindStringSubmatch(created)[1]
	if !strings.Contains(created, "порог well_True 0.60") {
		t.Fatalf("заявка проверена не порогом fp.rings.well_true.apply:\n%s", created)
	}
	e.must("approve", id)
	journal := e.must("fp", "journal")
	for _, want := range []string{"с исходом 1, верных 1", "pgconf.store", "1/1"} {
		if !strings.Contains(journal, want) {
			t.Fatalf("согласование не попало в журнал, нет %q:\n%s", want, journal)
		}
	}

	// Откат — исправление оператора: план засчитывается неверным.
	entry := rollbackHint.FindStringSubmatch(e.must("apply", "--request", id))[1]
	e.must("rollback", entry)
	if journal := e.must("fp", "journal"); !strings.Contains(journal, "верных 0") || !strings.Contains(journal, "0/1") {
		t.Fatalf("откат не попал в журнал:\n%s", journal)
	}

	// Без условного счёта одна неудача из одной — интервал широкий и
	// низкий: well_False, план отвергнут целиком.
	setRings(t, "prior", "0/0")
	setRings(t, "show_scores", true)
	out, err := e.ochan("plan", "good.ochan", "--prompt", "task.fp")
	if err == nil || !strings.Contains(err.Error(), "кольца отвергли утверждений: 2 из 2") {
		t.Fatalf("мало проверенные правила прошли кольцо 2: %v\n%s", err, out)
	}
	for _, want := range []string{"well_False    [0.", "--trust pgconf.store", "поддержка плана ["} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	// Оператор принимает утверждения сам; жёсткие правила его доверие не
	// отменяет.
	out = e.must("plan", "good.ochan", "--prompt", "task.fp", "--trust", "pgconf.store,pgconf.verify")
	if !strings.Contains(out, "well_True     [0.") || !strings.Contains(out, "— доверие оператора") {
		t.Fatalf("доверие оператора не пропустило кольцо 2:\n%s", out)
	}
	e.fails("--trust относится к кольцам", "plan", "good.ochan", "--trust", "write")

	// Доверие правилу — решение оператора, не модели.
	t.Setenv(EnvRole, "llm")
	e.fails("роль llm правилам не доверяет", "plan", "good.ochan", "--prompt", "task.fp", "--trust", "pgconf.store")
}

// Кольцо 3 ловит запись, которую запрещает правило «никогда», в самом
// утверждении; второй отказ по той же причине зовёт оператора, третий
// уже не проверяется.
func TestRingsContradictionAndBudget(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf})\n")
	e.write(e.path("bad.ochan"), `save(config("x = 1\n"), "other.conf")
`)

	out, err := e.ochan("plan", "bad.ochan", "--prompt", "task.fp")
	if err == nil || !strings.Contains(err.Error(), "план нарушает жёсткие правила промпта") ||
		!strings.Contains(err.Error(), "кольца отвергли утверждений: 1 из 1") {
		t.Fatalf("противоречие не поймано: %v\n%s", err, out)
	}
	if !strings.Contains(out, "противоречие  save "+e.path("other.conf")) {
		t.Fatalf("кольцо 3 не назвало утверждение:\n%s", out)
	}
	e.fails("причина та же, что в прошлый раз", "plan", "bad.ochan", "--prompt", "task.fp")
	e.fails("дважды подряд отвергнут по одной причине", "plan", "bad.ochan", "--prompt", "task.fp")

	// Новый текст промпта — новая задача и новый счёт.
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf, other_conf})\n")
	e.must("plan", "bad.ochan", "--prompt", "task.fp")

	// Кольца выключены — остаётся арбитр, журнал не пишется.
	setRings(t, "enabled", false)
	if out := e.must("plan", "bad.ochan", "--prompt", "task.fp"); strings.Contains(out, "Кольца") {
		t.Fatalf("кольца работают выключенными:\n%s", out)
	}
}
