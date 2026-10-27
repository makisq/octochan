package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"octochan/config"
	"octochan/scenario"
)

// brokenExecutor падает по заказу: partial — успев сделать первую задачу,
// refused — отказав до первого обращения к системе.
type brokenExecutor struct {
	x    *extExecutor
	mode string
}

func (b *brokenExecutor) Kind() string { return "Broken" }
func (b *brokenExecutor) Plan(ctx context.Context, s *scenario.Scenario) (*config.Plan, error) {
	return b.x.Plan(ctx, s)
}
func (b *brokenExecutor) Apply(ctx context.Context, s *scenario.Scenario, plan *config.Plan) (*scenario.Result, error) {
	switch b.mode {
	case "partial":
		return &scenario.Result{Refs: []string{"job-" + plan.Changes[0].Target}},
			fmt.Errorf("система отказала на второй задаче")
	case "refused":
		return nil, scenario.NotStarted(errors.New("система недоступна"))
	}
	return b.x.Apply(ctx, s, plan)
}

// lineOf — строка списка заявок с этим номером (не та, что на него ссылается).
func lineOf(out, id string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "  "+id+"  ") {
			return line
		}
	}
	return ""
}

// Порядок между заявками и семантика сбоя: A выполнена, B ждала A и
// упала — A остаётся, B пересобирают, заявки после B не выполнятся.
func TestRequestOrder(t *testing.T) {
	e := newScriptEnv(t)
	ext := &extExecutor{remote: "v1"}
	broken := &brokenExecutor{x: ext}
	scenario.Register("test-ext", ext)
	scenario.Register("test-broken", broken)
	t.Cleanup(func() { scenario.Unregister("test-ext"); scenario.Unregister("test-broken") })
	e.write(e.path("a.yaml"), "service: test-ext\nparameters: {limit: 200}\ntargets: [{svm_ci: CI1}]\n")
	e.write(e.path("b.yaml"), "service: test-broken\nparameters: {limit: 300}\ntargets: [{svm_ci: CI2}, {svm_ci: CI3}]\n")

	a := requestIDPattern.FindStringSubmatch(e.must("request", "a.yaml"))[1]
	out := e.must("request", "b.yaml", "--after", a[:4])
	b := requestIDPattern.FindStringSubmatch(out)[1]
	if !strings.Contains(out, "Выполнится после: "+a) {
		t.Fatalf("request --after:\n%s", out)
	}
	c := requestIDPattern.FindStringSubmatch(e.must("request", "a.yaml", "limit=400", "--after", b))[1]
	if line := lineOf(e.must("requests"), b); !strings.Contains(line, "ждёт решения (зависит от "+a+")") {
		t.Fatalf("в списке нет зависимости: %q", line)
	}

	e.must("approve", a)
	e.must("approve", b)
	list := e.must("requests")
	if line := lineOf(list, b); !strings.Contains(line, "ожидает (зависит от "+a+")") {
		t.Fatalf("согласованная заявка не ожидает: %q", line)
	}
	if ready := e.must("requests", "--ready"); !strings.Contains(ready, a) || strings.Contains(ready, b) {
		t.Fatalf("--ready до выполнения A:\n%s", ready)
	}
	e.fails("заявка "+b+" ожидает: зависит от "+a+" (согласована)", "apply", "--request", b)

	e.must("apply", "--request", a)
	if ready := e.must("requests", "--ready"); !strings.Contains(ready, b) {
		t.Fatalf("--ready после выполнения A:\n%s", ready)
	}

	// Отказ до начала выполнения заявку не трогает.
	broken.mode = "refused"
	out, err := e.ochan("apply", "--request", b)
	if err == nil || !strings.Contains(out, "Выполнение не начиналось: заявка "+b+" остаётся согласованной") {
		t.Fatalf("отказ исполнителя: %v\n%s", err, out)
	}

	// Сбой посреди выполнения: B упала, A остаётся, C не выполнится.
	broken.mode = "partial"
	out, err = e.ochan("apply", "--request", b)
	if err == nil {
		t.Fatalf("сбой не дошёл до пользователя:\n%s", out)
	}
	for _, want := range []string{
		"Заявка " + b + " упала.",
		"До сбоя сделано: job-CI2.",
		"остаются выполненными — её сбой их не откатывает: " + a,
		"Заявку не повторяют: оформите её заново (ochan request b.yaml --after " + a + ")",
		"ochan compensate " + b,
		"Не выполнятся, пока их не оформят заново после новой: " + c,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("в объяснении сбоя нет %q:\n%s", want, out)
		}
	}
	card := e.must("requests", a)
	if !strings.Contains(card, "Состояние: выполнена") || !strings.Contains(card, "Перед:     "+b+" (упала)") {
		t.Fatalf("карточка A:\n%s", card)
	}
	card = e.must("requests", b)
	if !strings.Contains(card, "Сбой:      система отказала") || !strings.Contains(card, "После:     "+a+" (выполнена)") {
		t.Fatalf("карточка B:\n%s", card)
	}
	if line := lineOf(e.must("requests"), c); !strings.Contains(line, "не выполнится ("+b+" упала)") {
		t.Fatalf("заявка после упавшей: %q", line)
	}
	e.fails("не выполнится: "+b+" упала", "approve", c)
	e.fails("не согласована (упала)", "apply", "--request", b)
	e.fails("--after "+b+": заявка упала", "request", "a.yaml", "--after", b)
	// Сделанное до сбоя возвращают отдельно: заявка, упавшая на полпути,
	// годится для обратного действия.
	e.fails("исполнитель Broken не умеет обратного действия", "compensate", b)

	// B пересобирают — новой заявкой после A.
	broken.mode = ""
	again := requestIDPattern.FindStringSubmatch(e.must("request", "b.yaml", "--after", a))[1]
	e.must("approve", again)
	e.must("apply", "--request", again)
	if line := lineOf(e.must("requests"), again); !strings.Contains(line, "выполнена") {
		t.Fatalf("пересобранная B: %q", line)
	}
}
