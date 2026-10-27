package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"

	"octochan/config"
)

// fakeRunner — исполнитель, который ничего не делает, но помнит вызовы.
type fakeRunner struct {
	kind    string
	planned int
	applied int
}

func (r *fakeRunner) Kind() string { return r.kind }

func (r *fakeRunner) Plan(context.Context, *Scenario) (*config.Plan, error) {
	r.planned++
	return &config.Plan{}, nil
}

func (r *fakeRunner) Apply(context.Context, *Scenario, *config.Plan) (*Result, error) {
	r.applied++
	return &Result{Refs: []string{"ref-1"}, Summary: "готово"}, nil
}

func TestResolvePicksRunnerByService(t *testing.T) {
	// Способ выполнения выбирается сервисом, а не флагом команды:
	// раньше apply требовал --rlm, то есть внешняя система была корнем
	// операции, а не одним из исполнителей.
	rlmLike := &fakeRunner{kind: "RLM"}
	localLike := &fakeRunner{kind: "локально"}

	Register("тест_через_rlm", rlmLike)
	Register("тест_локально", localLike)

	runner, err := Resolve("тест_локально")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if runner.Kind() != "локально" {
		t.Errorf("выбран исполнитель %q", runner.Kind())
	}

	if _, err := runner.Apply(context.Background(), &Scenario{Service: "тест_локально"}, &config.Plan{}); err != nil {
		t.Fatal(err)
	}
	if localLike.applied != 1 || rlmLike.applied != 0 {
		t.Errorf("вызовы разошлись: локально=%d, rlm=%d", localLike.applied, rlmLike.applied)
	}
}

func TestResolveUnknownServiceListsAvailable(t *testing.T) {
	Register("тест_известный", &fakeRunner{kind: "тест"})

	_, err := Resolve("чего-то-нет")
	if err == nil {
		t.Fatal("незарегистрированный сервис разрешился")
	}
	if !strings.Contains(err.Error(), "тест_известный") {
		t.Errorf("в сообщении нет списка доступных сервисов: %v", err)
	}
}

func TestParseRejectsConfigAsScenario(t *testing.T) {
	// Отдельная ошибка нужна команде apply, чтобы объяснить пользователю,
	// что он подал конфиг вместо сценария.
	_, err := Parse("prod.conf", []byte(`{"targets":[{"svm_ci":"CI-1"}]}`))
	if !errors.Is(err, ErrNotScenario) {
		t.Fatalf("ожидалась ErrNotScenario, получено: %v", err)
	}
}

func TestWithOverridesDoesNotTouchOriginal(t *testing.T) {
	// Переопределения не должны просачиваться в разобранный сценарий:
	// его же могут выполнить повторно с другими параметрами.
	original, err := Parse("<тест>", []byte(`{"service":"s","parameters":{"role":"standalone"},"targets":[{"svm_ci":"CI-1"}]}`))
	if err != nil {
		t.Fatal(err)
	}

	modified := original.WithOverrides(map[string]string{"role": "replica", "port": "5433"})

	if got := original.Parameters["role"]; got != "standalone" {
		t.Errorf("исходный сценарий изменился: role=%v", got)
	}
	if got := modified.Parameters["role"]; got != "replica" {
		t.Errorf("переопределение не применилось: role=%v", got)
	}
	if got := modified.Parameters["port"]; got != "5433" {
		t.Errorf("новый параметр не добавился: port=%v", got)
	}
	if _, leaked := original.Parameters["port"]; leaked {
		t.Error("новый параметр просочился в исходный сценарий")
	}
}

func TestWithOverridesEmptyReturnsSame(t *testing.T) {
	s := &Scenario{Service: "s", Parameters: map[string]any{"a": 1}}
	if s.WithOverrides(nil) != s {
		t.Error("без переопределений должен возвращаться тот же сценарий")
	}
}

func TestAllCIsFlattensTargets(t *testing.T) {
	s, err := Parse("<тест>", []byte(`{"service":"s","targets":[{"svm_ci":"CI-1"},{"svm_ci":["CI-2","CI-3"]}]}`))
	if err != nil {
		t.Fatal(err)
	}

	got := s.AllCIs()
	want := []string{"CI-1", "CI-2", "CI-3"}
	if len(got) != len(want) {
		t.Fatalf("получено %v, ожидалось %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("позиция %d: %q, ожидалось %q", i, got[i], want[i])
		}
	}
}

// waitingRunner умеет ждать; проверяем распознавание необязательной возможности.
type waitingRunner struct{ fakeRunner }

func (r *waitingRunner) Wait(context.Context, string) (string, error) { return "completed", nil }

func TestWaiterIsOptional(t *testing.T) {
	plain := &fakeRunner{kind: "без ожидания"}
	if _, ok := any(plain).(Waiter); ok {
		t.Error("исполнитель без Wait опознан как умеющий ждать")
	}

	waiting := &waitingRunner{fakeRunner{kind: "с ожиданием"}}
	w, ok := any(waiting).(Waiter)
	if !ok {
		t.Fatal("исполнитель с Wait не опознан")
	}
	if status, err := w.Wait(context.Background(), "ref"); err != nil || status != "completed" {
		t.Errorf("Wait вернул %q, %v", status, err)
	}
}
