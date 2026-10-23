package lang

import (
	"path/filepath"
	"strings"
	"testing"
)

// Конфиги для тестов категорий: test -> test2 меняет max_connections,
// удаляет old_param и добавляет new_param.
const (
	catTest  = "max_connections = 100\nwork_mem = 4MB\nold_param = 1\n"
	catTest2 = "max_connections = 200\nwork_mem = 4MB\nnew_param = on\n"
	catProd  = "max_connections = 100\nwork_mem = 8MB\nold_param = 1\n"
)

func catRun(t *testing.T, body string) string {
	t.Helper()
	src := "test = config(\"" + strings.ReplaceAll(catTest, "\n", "\\n") + "\")\n" +
		"test2 = config(\"" + strings.ReplaceAll(catTest2, "\n", "\\n") + "\")\n" +
		"prod = config(\"" + strings.ReplaceAll(catProd, "\n", "\\n") + "\")\n" +
		"let m = test -> test2\n" + body
	return strings.TrimSpace(mustRun(t, src))
}

func TestMorphismDelta(t *testing.T) {
	cases := map[string]string{
		`print(m)`:      "морфизм текст -> текст: max_connections 100 -> 200; new_param добавить = on; old_param удалить",
		`print(dom(m))`: `{"max_connections": 100, "new_param": null, "old_param": 1}`,
		`print(cod(m))`: `{"max_connections": 200, "new_param": on, "old_param": null}`,
		// К другому конфигу с теми же исходными значениями: work_mem прода
		// не трогается.
		`print(str(m(prod)))`: "max_connections = 200\nnew_param = on\nwork_mem = 8MB",
		// Исходный конфиг не меняется: морфизм возвращает новый.
		"x = m(prod)\nprint(prod[\"max_connections\"])": "100",
		// Значения сравниваются по смыслу.
		`print(str(morph(config("a = 8GB\n") -> config("a = 1\n"))(config("a = 8192MB\n"))))`: "a = 1",
		`print(m == test -> test2, m == test2 -> test)`:                                       "true false",
		`print(str(test | m) == str(test2))`:                                                  "true",
	}
	for body, want := range cases {
		if got := catRun(t, body); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", body, got, want)
		}
	}

	_, err := run(t, "m = config(\"a = 1\\n\") -> config(\"a = 2\\n\")\nm(config(\"a = 5\\n\"))")
	if err == nil || !strings.Contains(err.Error(), "ожидается a = 1, в конфиге 5") {
		t.Fatalf("применение к чужому конфигу: %v", err)
	}
	_, err = run(t, "m = config(\"a = 1\\n\") -> config(\"b = 2\\n\")\nm(config(\"a = 1\\nb = 3\\n\"))")
	if err == nil || !strings.Contains(err.Error(), "ожидается b = нет, в конфиге 3") {
		t.Fatalf("добавление поверх существующего: %v", err)
	}
}

func TestMorphismComposition(t *testing.T) {
	cases := map[string]string{
		// Дельты склеиваются: 100 -> 200, затем 200 -> 300.
		`print(m >> morph({"max_connections": "300"}))`:    "морфизм: max_connections 100 -> 300; new_param добавить = on; old_param удалить",
		`print(m >> inverse(m) == id() >> id())`:           "false",
		`print(commutes(m >> inverse(m), id()))`:           "true",
		`print(inverse(m))`:                                "морфизм inverse(текст -> текст): max_connections 200 -> 100; new_param удалить; old_param добавить = 1",
		`print(str((m >> inverse(m))(test)) == str(test))`: "true",
		// id нейтрален.
		`print(id() >> m == m, m >> id() == m)`: "true true",
		// Ассоциативность.
		"a = morph({\"x\": \"1\"})\nb = config(\"x = 1\\n\") -> config(\"x = 2\\n\")\nc = config(\"x = 2\\n\") -> config(\"x = 3\\n\")\nprint((a >> b) >> c == a >> (b >> c))": "true",
	}
	for body, want := range cases {
		if got := catRun(t, body); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", body, got, want)
		}
	}

	_, err := run(t, "m = config(\"a = 1\\n\") -> config(\"a = 2\\n\")\nm >> m")
	if err == nil || !strings.Contains(err.Error(), "не стыкуются: после первого a = 2, второй ожидает 1") {
		t.Fatalf("несостыковка: %v", err)
	}
	_, err = run(t, "m = morph(fn(c) => c)\ninverse(m)")
	if err == nil || !strings.Contains(err.Error(), "нет обратного") {
		t.Fatalf("обратный к функции: %v", err)
	}
	_, err = run(t, "inverse(morph({\"a\": \"1\"}))")
	if err == nil || !strings.Contains(err.Error(), "прежнее значение a неизвестно") {
		t.Fatalf("обратный к заданию значения: %v", err)
	}
}

func TestCommutes(t *testing.T) {
	cases := map[string]string{
		// Независимые изменения коммутируют: порядок не важен.
		"a = morph({\"work_mem\": \"64MB\"})\nb = morph({\"shared_buffers\": \"1GB\"})\nprint(commutes(a >> b, b >> a))": "true",
		// Конфликтующие — нет, и видно чем.
		"a = morph({\"x\": \"5\"})\nb = morph({\"x\": \"6\"})\nprint(conflicts(a >> b, b >> a))": "[x: по первому пути 6, по второму 5]",
		// Коммутативный квадрат: синхронизировать стенд с боем и
		// мигрировать — или мигрировать и синхронизировать. Синхронизация
		// трогает work_mem, миграция — max_connections: квадрат коммутирует.
		`sync = test -> prod
migrate = morph({"max_connections": "500"})
print(commutes(sync >> migrate, migrate >> sync))`: "true",
		`print(conflicts(m, morph({"max_connections": "200"})))`: "[new_param: по первому пути on, по второму нет, old_param: по первому пути нет, по второму 1]",
		// Разные начала.
		`print(conflicts(m, inverse(m)))`:   "[max_connections: пути начинаются с разных значений — 100 и 200, new_param: пути начинаются с разных значений — нет и on, old_param: пути начинаются с разных значений — 1 и нет]",
		`print(conflicts(m, prod -> test))`: "[max_connections: по первому пути 200, по второму 100, new_param: по первому пути on, по второму нет, old_param: по первому пути нет, по второму 1, work_mem: по первому пути 8MB, по второму 4MB]",
		// Функции — на объектах.
		`f = morph(fn(c) {
    put(c, "x", "1")
    return c
})
print(commutes(f, morph({"x": "1"}), [config("y = 2\n"), config("x = 7\n")]))`: "true",
		`f = morph(fn(c) {
    put(c, "x", "2")
    return c
})
print(conflicts(f, morph({"x": "1"}), [config("y = 2\n")]))`: "[текст: x: по первому пути 2, по второму 1]",
	}
	for body, want := range cases {
		if got := catRun(t, body); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", body, got, want)
		}
	}

	for src, want := range map[string]string{
		"commutes(morph(fn(c) => c), id())": "сравниваются на объектах",
		"a = config(\"a = 1\\n\") -> config(\"a = 2\\n\")\ncommutes(a, a, [config(\"a = 5\\n\")])": "проверять не на чем",
		"commutes(1, 2)": "нужен морфизм",
	} {
		if _, err := run(t, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
}

func TestFunctors(t *testing.T) {
	cases := map[string]string{
		"F = functor({\"max_connections\": \"max_client_conn\"})\nprint(F)":            "функтор {max_connections -> max_client_conn}",
		"F = functor({\"max_connections\": \"max_client_conn\"})\nprint(str(F(prod)))": "max_client_conn = 100\nold_param = 1\nwork_mem = 8MB",
		"F = functor({\"max_connections\": \"max_client_conn\"})\nprint(F(m))":         "морфизм текст -> текст: max_client_conn 100 -> 200; new_param добавить = on; old_param удалить",
		// Образ коммутирует: F(m(c)) == F(m)(F(c)).
		"F = functor({\"max_connections\": \"max_client_conn\"})\nprint(str(F(m(prod))) == str(F(m)(F(prod))))":                                     "true",
		"F = functor({\"max_connections\": \"max_client_conn\"})\nprint(functor_laws(F, [m, morph({\"max_connections\": \"300\"})], [prod, test]))": "[]",
		// Функтор-функция без отображения морфизмов: образ стрелки строится
		// по объекту.
		`half = functor(fn(c) {
    put(c, "pool_size", str(int(c["max_connections"]) / 2))
    return c
})
print(half(m, test))`: "морфизм текст -> текст: max_connections 100 -> 200; new_param добавить = on; old_param удалить; pool_size 50 -> 100",
		`half = functor(fn(c) {
    put(c, "pool_size", str(int(c["max_connections"]) / 2))
    return c
})
print(functor_laws(half, [m, morph({"max_connections": "300"})], [test, prod]))`: "[]",
		// Нарушение законов видно.
		"bad = functor(fn(c) => c, fn(x) => morph({\"x\": \"1\"}))\nprint(functor_laws(bad, [m], [test]))": "[текст: F(id) не тождественен: x: получено 1, ожидалось нет]",
		// Проекция: функтор забывает всё, кроме перечисленного.
		"P = functor({\"max_connections\": \"pgbouncer.default_pool_size\", \"*\": null})\nprint(P(m))":           "морфизм текст -> текст: pgbouncer.default_pool_size 100 -> 200",
		"P = functor({\"old_param\": null})\nprint(str(P(test)))":                                                 "max_connections = 100\nwork_mem = 4MB",
		"P = functor({\"max_connections\": \"pool\", \"*\": null})\nprint(P, functor_laws(P, [m], [test, prod]))": "функтор {max_connections -> pool, * -> null} []",
		// Композиция функторов.
		"F = functor({\"a\": \"b\"})\nG = functor({\"b\": \"c\"})\nprint(str((F >> G)(config(\"a = 1\\n\"))))": "c = 1",
	}
	for body, want := range cases {
		if got := catRun(t, body); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", body, got, want)
		}
	}

	for src, want := range map[string]string{
		"F = functor({\"a\": \"c\", \"b\": \"c\"})\nF(config(\"a = 1\\nb = 2\\n\"))": "переходят в один параметр c",
		"F = functor(fn(c) => c)\nF(morph({\"a\": \"1\"}))":                          "передайте объект",
		"F = functor({\"a\": \"b\"})\nF >> fn(x) => x":                               "функтор композируется только с функтором",
		"functor(5)":              "словарём переименований или функцией",
		"functor({\"*\": \"x\"})": "принимает только null",
	} {
		if _, err := run(t, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
}

func TestCheckCategories(t *testing.T) {
	for src, want := range map[string]string{
		"m = 1 -> 2":           "-> соединяет конфиги, получено целое",
		"m = [1] -> \"a = 1\"": "-> соединяет конфиги, получено список",
		"f = functor({\"a\": \"b\"}) >> fn(x) => x": "функтор композируется только с функтором",
		"m = morph({})\nfor x in m { }":             "нельзя пройти циклом",
	} {
		problems := check(t, src)
		found := false
		for _, p := range problems {
			if strings.Contains(p.Msg, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: проблемы %v, ожидалось %q", src, problems, want)
		}
	}
	for _, src := range []string{
		"m = config(\"a = 1\\n\") -> config(\"a = 2\\n\")\nc = m(config(\"a = 1\\n\"))\nprint(c[\"a\"])",
		"alias apply(m: morph, c: config) { return m(c) }",
		"F = functor({\"a\": \"b\"})\nG = F >> F\nprint(G(config(\"a = 1\\n\")))",
	} {
		if problems := check(t, src); len(problems) > 0 {
			t.Errorf("%s: ложные проблемы %v", src, problems)
		}
	}
	if _, err := Parse("<тест>", "m = a -> b -> c"); err == nil || !strings.Contains(err.Error(), "две стрелки") {
		t.Fatalf("цепочка стрелок: %v", err)
	}
}

// Морфизм в плане: запись результата видна как изменения параметров.
func TestMorphismInPlan(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "test.conf"), catTest)
	writeFile(t, filepath.Join(dir, "test2.conf"), catTest2)
	writeFile(t, filepath.Join(dir, "prod.conf"), catProd)
	_, effects, err := planned(t, dir, `m = config("test.conf") -> config("test2.conf")
save(m(config("prod.conf")), "prod.conf")`)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 1 || !strings.HasSuffix(effects[0].Path, "prod.conf") {
		t.Fatalf("действия: %+v", effects)
	}
	if readFile(t, filepath.Join(dir, "prod.conf")) != catProd {
		t.Fatal("план записал файл")
	}
}

func TestMorphismsInParallel(t *testing.T) {
	src := `f = morph(fn(c) {
    put(c, "n", "1")
    return c
})
parallel for i in range(8) {
    c = f(config("a = 1\n"))
    print(i, c["n"])
}`
	if out := mustRun(t, src); !strings.Contains(out, "0 1\n1 1\n") {
		t.Fatalf("parallel: %q", out)
	}
}
