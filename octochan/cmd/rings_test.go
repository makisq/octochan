package cmd

import (
	"fmt"
	"regexp"
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
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf})\n"+promptCell)
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
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf})\n"+promptCell)
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
	e.write(e.path("task.fp"), promptHead+"hard forbid save(except = {prod_conf, other_conf})\n"+promptCell)
	e.must("plan", "bad.ochan", "--prompt", "task.fp")

	// Кольца выключены — остаётся арбитр, журнал не пишется.
	setRings(t, "enabled", false)
	if out := e.must("plan", "bad.ochan", "--prompt", "task.fp"); strings.Contains(out, "Кольца") {
		t.Fatalf("кольца работают выключенными:\n%s", out)
	}
}

// Нарушение запрета — на счёт модели и промпта: модель, показывая планы с
// нарушениями, не опускает надёжность скилла, который нужен оператору.
func TestForbidViolationsBlameModelAndPrompt(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	setting(t, "roles.llm.limits", []string{})
	e.write(e.path("other.conf"), "max_connections = 100\n")
	e.write(e.path("good.ochan"), `c = config("prod.conf")
put(c, "max_connections", "200")
pgconf.store(c, "prod.conf")
`)

	// Модель перебирает промпты, и в каждом плане нарушает запрет.
	t.Setenv(EnvRole, "llm")
	for i := 0; i < 6; i++ {
		prompt := e.path(fmt.Sprintf("p%d.fp", i))
		e.write(prompt, promptHead+fmt.Sprintf("hard forbid store(except = {prod_conf})\n# %d\n", i))
		e.write(e.path("bad.ochan"), fmt.Sprintf("pgconf.store(config(\"other.conf\"), \"other.conf\") # %d\n", i))
		out, err := e.ochan("plan", "bad.ochan", "--prompt", prompt)
		if err == nil || !strings.Contains(out, "нарушение запрета — на счёт модели и промпта: надёжность правила pgconf.store не меняется") {
			t.Fatalf("план %d: %v\n%s", i, err, out)
		}
	}

	// План оператора со скиллом проходит, как и до серии.
	t.Setenv(EnvRole, "")
	e.write(e.path("task.fp"), promptHead+"hard forbid store(except = {prod_conf})\n"+promptCell)
	if out := e.must("plan", "good.ochan", "--prompt", "task.fp"); !strings.Contains(out, "well_True     pgconf.store(") {
		t.Fatalf("скилл отравлен планами модели:\n%s", out)
	}

	journal := e.must("fp", "journal")
	for _, want := range []string{"Нарушения запретов — на счёт модели и промпта:", "роль llm", "планов 6, с нарушением 6"} {
		if !strings.Contains(journal, want) {
			t.Fatalf("в журнале нет %q:\n%s", want, journal)
		}
	}
	// Ни одного исхода на счёт правила: 0 верных из 0.
	if !strings.Contains(journal, "pgconf.store                     0/0") {
		t.Fatalf("нарушения засчитаны правилу скилла:\n%s", journal)
	}
}

// Частота plan для llm — настройка roles.llm.limits; предел, заданный
// командой role limit, важнее.
func TestConfigRoleLimits(t *testing.T) {
	e := newScriptEnv(t)
	setting(t, "roles.llm.limits", []string{"plan=2/1h"})

	if out := e.must("role", "show", "llm"); !strings.Contains(out, "Пределы:   request=30/1h, plan=2/1h (config.yaml)") {
		t.Fatalf("предел из конфига не виден:\n%s", out)
	}

	t.Setenv(EnvRole, "llm")
	e.must("plan", "tune.ochan")
	e.must("plan", "tune.ochan")
	e.fails("предел роли llm исчерпан: plan=2/1h", "plan", "tune.ochan")

	// Предел хранилища ролей для того же права перекрывает конфиг.
	t.Setenv(EnvRole, "")
	e.must("role", "limit", "llm", "request=30/1h", "plan=5/1h")
	t.Setenv(EnvRole, "llm")
	e.must("plan", "tune.ochan")

	// Опечатка в конфиге — отказ, а не тихое снятие предела.
	setting(t, "roles.llm.limits", []string{"plan=много/1h"})
	e.fails("config.yaml roles.llm.limits", "plan", "tune.ochan")

	// У master пределов не бывает и из конфига.
	t.Setenv(EnvRole, "")
	setting(t, "roles.master.limits", []string{"plan=1/1h"})
	e.must("plan", "tune.ochan")
	e.must("plan", "tune.ochan")
}

// Каждый шаг прошёл кольца, а план целиком держится слабее порога: заявка
// создаётся, но согласует её оператор, проверив план, — слабейшие правила
// он видит по рангу, без чисел.
func TestWeakPlanNeedsOperator(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead)
	// Правило, вызванное много раз, — один множитель.
	e.write(e.path("loop.ochan"), "c = config(\"prod.conf\")\nfor i in range(5) {\n    pgconf.verify(c)\n}\npgconf.store(c, \"prod.conf\")\n")

	if out := e.must("plan", "loop.ochan", "--prompt", "task.fp"); strings.Contains(out, "ниже порога") {
		t.Fatalf("два правила при холодном старте — не слабый план:\n%s", out)
	}
	setRings(t, "show_scores", true)
	if out := e.must("plan", "loop.ochan", "--prompt", "task.fp"); !strings.Contains(out, "по различным правилам, их 2 из 6 утверждений") {
		t.Fatalf("поддержка плана не по различным правилам:\n%s", out)
	}
	setRings(t, "show_scores", false)

	setRings(t, "plan_support.apply", 0.9)
	created := e.must("request", "loop.ochan", "--prompt", "task.fp")
	if !strings.Contains(created, "Поддержка плана ниже порога fp.rings.plan_support (0.90)") ||
		!strings.Contains(created, "Самые слабые правила, по порядку: pgconf.verify, pgconf.store") ||
		strings.Contains(created, "[0.") {
		t.Fatalf("слабый план:\n%s", created)
	}
	id := requestIDPattern.FindStringSubmatch(created)[1]
	if card := e.must("requests", id); !strings.Contains(card, "Внимание:  поддержка плана ниже порога") {
		t.Fatalf("карточка:\n%s", card)
	}
	e.fails("повторите с --yes", "approve", id)
	e.must("approve", id, "--yes")
	if out := e.must("fp", "journal"); !strings.Contains(out, "Калибровка планов с пометкой «слабый план»") {
		t.Fatalf("слой калибровки:\n%s", out)
	}
}

// Журнал учится только на том, что говорит о плане: отказ не по плану и
// откат не из-за плана правилам не засчитываются, а инцидент после
// согласования исправляется поздней поправкой.
func TestJournalOperatorKinds(t *testing.T) {
	e := newScriptEnv(t)
	e.skill("1.2.0")
	e.write(e.path("task.fp"), promptHead)
	// Значение выбирает модель и говорит о нём в отчёте.
	script := func(name, value string) {
		e.write(e.path(name), "c = config(\"prod.conf\")\nput(c, \"max_connections\", \""+value+"\")\npgconf.verify(c)\npgconf.store(c, \"prod.conf\")\n")
		e.write(e.path(name+".fp"), "report:\n  q: prod_conf.max_connections\n    choice: "+value+"\n    basis: not_in_prompt\n")
	}
	request := func(name string) string {
		return requestIDPattern.FindStringSubmatch(e.must("request", name, "--prompt", "task.fp", "--report", name+".fp"))[1]
	}
	storeLine := func() string {
		for _, line := range strings.Split(e.must("fp", "journal"), "\n") {
			if strings.Contains(line, "pgconf.store") {
				return strings.Join(strings.Fields(line), " ")
			}
		}
		return ""
	}

	script("a.ochan", "200")
	id := request("a.ochan")
	e.fails("--reason-kind \"later\"", "reject", id, "--reason", "x", "--reason-kind", "later")
	if out := e.must("reject", id, "--reason", "нет окна", "--reason-kind", "timing"); !strings.Contains(out, "правилам плана он не засчитан") {
		t.Fatalf("reject timing:\n%s", out)
	}
	if line := storeLine(); !strings.HasPrefix(line, "pgconf.store 0/0") {
		t.Fatalf("отказ не по плану засчитан: %q", line)
	}

	script("b.ochan", "201")
	id = request("b.ochan")
	e.must("approve", id)
	if line := storeLine(); !strings.HasPrefix(line, "pgconf.store 1/1") {
		t.Fatalf("после согласования: %q", line)
	}
	e.fails("укажите исход", "fp", "journal", "amend", id, "--reason", "INC-1")
	if out := e.must("fp", "journal", "amend", id, "--wrong", "--reason", "INC-1"); !strings.Contains(out, "план неверный") {
		t.Fatalf("amend:\n%s", out)
	}
	if line := storeLine(); !strings.HasPrefix(line, "pgconf.store 0/1") {
		t.Fatalf("после поправки: %q", line)
	}

	// После инцидента правила пакета опустились ниже порога заявки: и
	// store, и verify, у которого своих исходов нет, — авторское «8 из 8»
	// подтянулось к тому, как пакет показал себя.
	script("c.ochan", "202")
	e.fails("кольца отвергли утверждений: 2 из 2", "request", "c.ochan", "--prompt", "task.fp", "--report", "c.ochan.fp")

	// Откат не из-за плана исход не меняет.
	id = requestIDPattern.FindStringSubmatch(e.must("request", "c.ochan", "--prompt", "task.fp", "--report", "c.ochan.fp",
		"--trust", "pgconf.verify", "--trust", "pgconf.store"))[1]
	e.must("approve", id)
	out := e.must("apply", "--request", id)
	entry := regexp.MustCompile(`ochan rollback ([0-9a-f]+)`).FindStringSubmatch(out)
	if entry == nil {
		t.Fatalf("нет записи для отката:\n%s", out)
	}
	if out := e.must("rollback", entry[1], "--not-plan"); !strings.Contains(out, "правилам плана он не засчитан") {
		t.Fatalf("rollback --not-plan:\n%s", out)
	}
	if line := storeLine(); !strings.HasPrefix(line, "pgconf.store 1/2") {
		t.Fatalf("после отката не по плану: %q", line)
	}
}
