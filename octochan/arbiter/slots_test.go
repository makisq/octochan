package arbiter

import (
	"strings"
	"testing"

	"octochan/fp"
	"octochan/lang"
)

const slotsPrompt = `goal: tuned

classes:
  State  : "tuned"
  Config : "config"
  Limit  : "limit"

relations:
  Config --strives_to--> State

generalization raise(param : {max_connections, work_mem}, value : 100..300) :
  Config = set_param(param, value)

generalization grow(size : {16 MB, 32 MB}) :
  Config = resize(size)

generalization keep(path : {prod_conf}) :
  Config = store(path)

instance raise_pool = raise(param := max_connections, value := 200)
instance raise_mem = raise(param := work_mem, value := 300)

import @pgconf v1
  provides: Config, set_param, store, resize
  requires_class: Config
`

func parseSlots(t *testing.T, text string) *fp.Program {
	t.Helper()
	prog, diags := fp.Parse("t.fp", text)
	if len(diags) > 0 {
		t.Fatal(diags)
	}
	return prog
}

func TestCheckSlots(t *testing.T) {
	prog := parseSlots(t, slotsPrompt)
	sig := func(c lang.Invocation) []string {
		if c.Name == "store" {
			return []string{"c", "path"}
		}
		return nil // set_param — параметры неизвестны: по месту
	}
	check := func(calls ...lang.Invocation) map[string][]string {
		out := map[string][]string{}
		for _, o := range CheckSlots(prog, calls, sig) {
			out[o.Generalization.Name] = o.Violations
		}
		return out
	}

	ok := check(
		lang.Invocation{ID: 1, Name: "set_param", Module: "pgconf", Args: []string{"max_connections", "200"}},
		lang.Invocation{ID: 2, Name: "store", Module: "pgconf", Args: []string{"/etc/pg/prod.conf", "/srv/prod.conf"}},
		lang.Invocation{ID: 3, Name: "resize", Module: "pgconf", Args: []string{"32MB"}},
	)
	if len(ok["raise"])+len(ok["keep"])+len(ok["grow"]) != 0 {
		t.Fatalf("законный план: %v", ok)
	}

	bad := check(
		lang.Invocation{ID: 1, Name: "set_param", Module: "pgconf", Args: []string{"max_connections", "9000"}},
		lang.Invocation{ID: 2, Name: "set_param", Module: "pgconf", Args: []string{"work_mem", "200"}},
		lang.Invocation{ID: 3, Name: "store", Module: "pgconf", Args: []string{"c", "other.conf"}},
		lang.Invocation{ID: 4, Name: "set_param", Module: "pgconf", Args: []string{"work_mem"}},
		lang.Invocation{ID: 6, Name: "resize", Module: "pgconf", Args: []string{"64MB"}},
		// Вызов изнутри скилла — реализация библиотеки, не решение модели.
		lang.Invocation{ID: 5, Name: "set_param", Module: "pgconf", Args: []string{"shared_buffers", "1"}, Outer: []int{2}},
	)
	got := strings.Join(append(append(bad["raise"], bad["keep"]...), bad["grow"]...), "\n")
	for _, want := range []string{
		"value = 9000 вне диапазона 100..300",
		"size = 64MB вне домена {16 MB, 32 MB}",
		"pgconf.set_param(work_mem, 200): не совпадает ни с одним частным случаем промпта (raise_pool, raise_mem)",
		"path = other.conf вне домена {prod_conf}",
		"слот param не сопоставить с аргументами",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "shared_buffers") {
		t.Error("проверен вызов изнутри скилла")
	}

	// Ячейка задаёт значение слота.
	prog = parseSlots(t, slotsPrompt+"\ncell c1:\n  set keep.path = test_conf\n")
	if out := CheckSlots(prog, []lang.Invocation{{ID: 1, Name: "store", Args: []string{"c", "prod.conf"}}}, sig); len(out) != 3 ||
		!strings.Contains(strings.Join(out[2].Violations, ""), "промпт задаёт keep.path = test_conf") {
		t.Fatalf("ячейка: %+v", out)
	}

	for value, name := range map[string]string{"prod.conf": "prod_conf", "/srv/prod.conf": "prod_conf", "30km/h": "30 km/h", "16MB": "16MB"} {
		if !sameValue(value, name) {
			t.Errorf("%s ≠ %s", value, name)
		}
	}
	if sameValue("prod.conf", "test_conf") {
		t.Error("prod.conf = test_conf")
	}
}
