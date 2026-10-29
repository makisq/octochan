package workspace

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePolicy(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.Local)
	text := "name: new-servers\nscenario: tune.yaml\ntargets: [group=db_new]\n" +
		"params:\n  max_connections: 100..500\n  work_mem: [16MB, 32MB]\n  restart: \"*\"\n  mode: fast\n" +
		"expires: 2026-12-31\n"
	p, err := ParsePolicy("/srv/policies/new.yaml", []byte(text), now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Scenario != filepath.Join("/srv/policies", "tune.yaml") || len(p.Sum) != 64 {
		t.Fatalf("сценарий %q, сумма %q", p.Scenario, p.Sum)
	}
	for _, tc := range []struct {
		param, value string
		ok           bool
	}{
		{"max_connections", "300", true}, {"max_connections", "900", false}, {"max_connections", "много", false},
		{"work_mem", "32MB", true}, {"work_mem", "64MB", false},
		{"restart", "что угодно", true}, {"mode", "fast", true}, {"mode", "slow", false},
	} {
		if got := p.Params[tc.param].Allows(tc.value); got != tc.ok {
			t.Errorf("%s = %s: %v", tc.param, tc.value, got)
		}
	}
	// Дата — включительно, весь день.
	if p.State(time.Date(2026, 12, 31, 23, 0, 0, 0, time.Local)) != nil {
		t.Fatal("в последний день политика уже не действует")
	}
	if err := p.State(time.Date(2027, 1, 1, 0, 0, 1, 0, time.Local)); err == nil || !strings.Contains(err.Error(), "истёк") {
		t.Fatalf("после срока: %v", err)
	}

	for text, want := range map[string]string{
		"name: x\nscenario: s.yaml\nparams: {a: \"*\"}\nexpires: 2026-12-31\n":                                 "нет targets",
		"name: x\nscenario: s.yaml\ntargets: [group=a]\nexpires: 2026-12-31\n":                                 "нет params",
		"name: x\nscenario: s.yaml\ntargets: [group=a]\nparams: {a: \"*\"}\n":                                  "нет expires",
		"name: x\nscenario: s.yaml\ntargets: [group=a]\nparams: {a: \"*\"}\nexpires: 2026-01-01\n":             "уже прошёл",
		"name: x\nscenario: s.yaml\ntargets: [group=a]\nparams: {a: 5..1}\nexpires: 2026-12-31\n":              "пуст",
		"name: x\nscenario: s.yaml\ntargets: [group=a]\nparams: {a: \"*\"}\nexpires: 2026-12-31\nauto: true\n": "auto",
	} {
		if _, err := ParsePolicy("p.yaml", []byte(text), now); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: ожидалось %q, получено %v", text, want, err)
		}
	}
}

func TestPoliciesStore(t *testing.T) {
	now := time.Now()
	store, err := NewPolicies(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &Policy{Name: "p", Expires: now.Add(time.Hour), Params: map[string]ParamRule{"a": {Any: true}}}
	if err := store.Add(p, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(&Policy{Name: "p", Expires: now.Add(time.Hour)}, now); err == nil ||
		!strings.Contains(err.Error(), "уже действует") {
		t.Fatalf("замена действующей: %v", err)
	}
	if _, err := store.Revoke("p", "root", "хватит", now); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("p")
	if err := got.State(now); err == nil || !strings.Contains(err.Error(), "хватит") {
		t.Fatalf("после отзыва: %v", err)
	}
	// Отозванную можно зарегистрировать заново; запись старой — в архиве.
	if err := store.Add(&Policy{Name: "p", Expires: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	if old, _ := filepath.Glob(filepath.Join(store.dir, "old", "p-*.json")); len(old) != 1 {
		t.Fatalf("архив: %v", old)
	}
	if _, err := store.Get("../x"); err == nil {
		t.Fatal("имя с путём принято")
	}
}
