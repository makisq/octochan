package lang

import (
	"strings"
	"testing"
)

func TestFunctionValues(t *testing.T) {
	cases := map[string]string{
		// Лямбда с телом-выражением и с телом-блоком.
		"double = fn(x) => x * 2\nprint(double(21))":                        "42",
		"add = fn(a: int, b: int) {\n    return a + b\n}\nprint(add(2, 3))": "5",
		// Ссылка на алиас и на встроенную функцию.
		"alias inc(x) { return x + 1 }\nf = fn inc\nprint(f(1), call(fn len, [1, 2, 3]))": "2 3",
		// Функцию возвращают и сразу вызывают.
		"alias adder(n) { return fn(x) => x + n }\nprint(adder(10)(5))": "15",
		// Функции в списке.
		"fs = [fn(x) => x + 1, fn(x) => x * 10]\nprint(fs[1](4))": "40",
		// Функция — аргумент алиаса.
		"alias twice(f: fn, x) { return f(f(x)) }\nprint(twice(fn(v) => v * 3, 2))": "18",
		// Снимок: функция видит значение на момент создания.
		"n = 1\nf = fn() => n\nn = 2\nprint(f(), n)": "1 2",
		// Словарь в снимке — тот же словарь.
		"m = {}\nadd = fn(k) => put(m, k, 1)\nadd(\"a\")\nprint(keys(m))": "[a]",
		// Тело функции, переданной аргументом, — инструкции по строкам.
		"print(each([1, 2], fn(x) {\n    y = x * 10\n    return y + 1\n}))": "[11, 21]",
		"print(fn(x) => x)":                                              "fn(x)",
		"alias a() { return 1 }\nprint(fn a)":                            "fn a",
		"f = fn len\ng = f\nprint(f == g, (fn(x) => x) == (fn(x) => x))": "true false",
	}
	for src, want := range cases {
		if got := strings.TrimSpace(mustRun(t, src)); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", src, got, want)
		}
	}
}

func TestRecursion(t *testing.T) {
	src := `let fact = fn(n: int): int {
    if n <= 1 {
        return 1
    }
    return n * fact(n - 1)
}
alias fib(n) {
    if n < 2 { return n }
    return fib(n - 1) + fib(n - 2)
}
print(fact(10), fib(15))
# Глубина обычной рекурсии.
alias depth(n) {
    if n == 0 { return 0 }
    return 1 + depth(n - 1)
}
print(depth(900))`
	if got := strings.TrimSpace(mustRun(t, src)); got != "3628800 610\n900" {
		t.Fatalf("рекурсия: %q", got)
	}

	_, err := run(t, "let loop = fn(n) => loop(n + 1)\nloop(0)")
	if err == nil || !strings.Contains(err.Error(), "слишком глубокая вложенность") {
		t.Fatalf("бесконечная рекурсия: %v", err)
	}
	if !strings.Contains(err.Error(), "в функции loop") {
		t.Fatalf("в стеке не видно функции: %v", err)
	}
}

func TestHigherOrderBuiltins(t *testing.T) {
	cases := map[string]string{
		`print(each([1, 2, 3], fn(x) => x * x))`:                            "[1, 4, 9]",
		`print(filter([1, -2, 3, -4], fn(x) => x > 0))`:                     "[1, 3]",
		`print(reduce([1, 2, 3, 4], fn(acc, x) => acc + x, 0))`:             "10",
		`print(join(sort(["b", "c", "a"]), ","))`:                           "a,b,c",
		`print(join(sort(["ccc", "a", "bb"], fn(s) => len(s)), ","))`:       "a,bb,ccc",
		`print(join(sort([3, 1, 2], fn(x) => -x), ","))`:                    "3,2,1",
		`print(call(fn(a, b) => a - b, 10, 3))`:                             "7",
		`print(partial(fn(a, b) => a - b, 10)(3))`:                          "7",
		`print(compose(fn(x) => x + 1, fn(x) => x * 10)(2))`:                "30",
		`print(id()(5), id() == id())`:                                      "5 true",
		`print(join(each(range(3), fn(i) => str(i)), ","))`:                 "0,1,2",
		`print(join(each(range(2, 8, 3), fn(i) => str(i)), ","))`:           "2,5",
		`print(join(each(range(3, 0, -1), fn(i) => str(i)), ","))`:          "3,2,1",
		`print(len(range(0)))`:                                              "0",
		"m = {\"b\": 2, \"a\": 1}\nprint(join(each(m, fn(k) => k), \",\"))": "a,b",
	}
	for src, want := range cases {
		if got := strings.TrimSpace(mustRun(t, src)); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", src, got, want)
		}
	}

	for src, want := range map[string]string{
		`each([1], 5)`:                      "нужна функция",
		`sort([1, [2]])`:                    "нельзя сравнить",
		`range(1, 2, 0)`:                    "шаг не может быть нулём",
		`range(10000000)`:                   "больше предела",
		`call(fn(x) => x)`:                  "принимает 1 аргумент, передано 0",
		`each([1], fn(x: string) => x)`:     "объявлен тип строка",
		`print(call(fn(x): int => "a", 1))`: "результат функции",
	} {
		if _, err := run(t, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
}

func TestCompose(t *testing.T) {
	cases := map[string]string{
		"inc = fn(x) => x + 1\ndbl = fn(x) => x * 2\nprint((inc >> dbl)(3), (dbl >> inc)(3))": "8 7",
		// Голое имя алиаса в композиции — функция, а не вызов.
		"alias inc(x) { return x + 1 }\nalias dbl(x) { return x * 2 }\nf = inc >> dbl >> inc\nprint(f(1), f)": "5 fn inc >> fn dbl >> fn inc",
		// Конвейер с композицией: x | f >> g — это x | (f >> g).
		"inc = fn(x) => x + 1\nprint(3 | inc >> str >> lines)": "[4]",
		"print(5 | fn(x) => x * 2)":                            "10",
		"f = fn(x) => x + 100\nprint(1 | f)":                   "101",
		"f = fn(x, y) => x - y\nprint(10 | f(4))":              "6",
	}
	for src, want := range cases {
		if got := strings.TrimSpace(mustRun(t, src)); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", src, got, want)
		}
	}
	if _, err := run(t, "f = 5 >> 6"); err == nil || !strings.Contains(err.Error(), "ожидалась функция") {
		t.Fatalf("композиция чисел: %v", err)
	}
}

func TestLet(t *testing.T) {
	if got := strings.TrimSpace(mustRun(t, "let n = 5\nif true {\n    var n = 6\n    print(n)\n}\nprint(n)")); got != "6\n5" {
		t.Fatalf("let: %q", got)
	}
	for src, want := range map[string]string{
		"let n = 5\nn = 6":                 "объявлена через let",
		"let n = 5\nif true { n = 6 }":     "объявлена через let",
		"let n: int = \"a\"":               "объявлен тип целое",
		"n = 1\nf = fn() { n = 2 }\nf()":   "переменная снаружи функции",
		"let f = fn() => 1\nf = fn() => 2": "объявлена через let",
	} {
		if _, err := run(t, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
	if _, err := Parse("<тест>", "let n"); err == nil || !strings.Contains(err.Error(), "у let всегда есть значение") {
		t.Fatalf("let без значения: %v", err)
	}
}

func TestVariableShadowingBuiltinStillCallsIt(t *testing.T) {
	// Переменная с другим значением вызов не перехватывает: сценарии,
	// написанные до функций-значений, работают как прежде.
	if got := strings.TrimSpace(mustRun(t, "len = 5\nprint(len([1, 2]), len)")); got != "2 5" {
		t.Fatalf("получено %q", got)
	}
	if _, err := run(t, "x = 5\nx(1)"); err == nil || !strings.Contains(err.Error(), "x — не функция, а целое") {
		t.Fatalf("вызов числа: %v", err)
	}
}

func TestCheckFunctions(t *testing.T) {
	for src, want := range map[string]string{
		"let n = 1\nn = 2":                 "объявлена через let",
		"n = 1\nf = fn() { n = 2 }":        "переменная снаружи функции",
		"f = fn(x) => y":                   `неизвестное имя "y"`,
		"x = 5\nx(1)":                      "x — не функция, а целое",
		"g = fn nope":                      `неизвестная функция "nope"`,
		"f = fn(x) { break }":              "break допустим только внутри for",
		"x = 5\nf = fn x":                  "x — не функция, а целое",
		"f = 1 >> fn(x) => x":              "оператор >> соединяет функции",
		"f = fn(x): int => \"a\"":          "результат функции",
		"for x in fn(y) => y { print(x) }": "нельзя пройти циклом",
		"print(1 | 5)":                     "справа от |",
		"print([1](2))":                    "вызвать можно функцию",
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
		"let fact = fn(n) => fact(n - 1)",
		"f = fn(x) => x\nprint(f(1), each([1], f), 1 | f)",
		"alias a(x) { return x }\nf = a >> a\nprint(f(1))",
		"len = 5\nprint(len([1]))",
		"m = {}\nf = fn(k) => put(m, k, 1)",
		"n = 1\nf = fn() {\n    var n = 2\n    return n\n}",
		"alias mk() { return fn(x) => x }\nprint(mk()(1))",
	} {
		if problems := check(t, src); len(problems) > 0 {
			t.Errorf("%s: ложные проблемы %v", src, problems)
		}
	}
}

// Алиас, на который сценарий ссылается через fn или композицию, — тоже
// зависимость заявки.
func TestUsesSeesFunctionReferences(t *testing.T) {
	registry := NewRegistry()
	lib, err := Parse("lib.ochan", "alias a(x) { return x }\nalias b(x) { return x }\nalias c(x) { return x }\nalias d(x) { return x }")
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range lib.Aliases {
		if err := registry.Add("lib", decl); err != nil {
			t.Fatal(err)
		}
	}
	file, err := Parse("s.ochan", "f = fn lib.a\ng = lib.b >> lib.c\nprint(f(1), g(2))")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, decl := range Uses(file, registry) {
		names = append(names, decl.Name)
	}
	if got := strings.Join(names, ","); got != "a,b,c" {
		t.Fatalf("используемые алиасы: %s", got)
	}
}

// Функции в parallel for: у каждой итерации своя копия снимка, и put в
// словарь из снимка не гоняется между итерациями.
func TestFunctionsInParallel(t *testing.T) {
	src := `m = {}
mark = fn(k) => put(m, k, 1)
parallel for n in [1, 2, 3, 4, 5, 6, 7, 8] {
    mark(str(n))
    print(n, len(keys(m)))
}
print(len(keys(m)))`
	out := mustRun(t, src)
	if !strings.Contains(out, "1 1\n2 1\n") || !strings.HasSuffix(strings.TrimSpace(out), "\n0") {
		t.Fatalf("parallel: %q", out)
	}
}

func TestFunctionsInPlan(t *testing.T) {
	src := `write_all = fn(names) => each(names, fn(n) => write(n + ".txt", n))
write_all(["a", "b"])`
	dir := t.TempDir()
	_, effects, err := planned(t, dir, src)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range effects {
		paths = append(paths, e.Path)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "a.txt") || !strings.HasSuffix(paths[1], "b.txt") {
		t.Fatalf("действия функций не попали в план: %v", paths)
	}
}

func TestTextBuiltins(t *testing.T) {
	cases := map[string]string{
		`print(upper("ab"), lower("CD"), trim("  x  "), trim("/a/", "/"))`:            "AB cd x a",
		`print(join(split("a,b,c", ","), "|"), replace("a-b-c", "-", "+"))`:           "a|b|c a+b+c",
		`print(contains("prod.conf", "prod"), contains([1, 2], 2), contains([1], 3))`: "true true false",
		`print(starts_with("db1", "db"), ends_with("x.conf", ".yaml"))`:               "true false",
		`print("  Prod " | trim >> lower >> fn(s) => s + ".conf")`:                    "prod.conf",
	}
	for src, want := range cases {
		if got := strings.TrimSpace(mustRun(t, src)); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", src, got, want)
		}
	}
	if _, err := run(t, `split("a", "")`); err == nil {
		t.Error("пустой разделитель принят")
	}
	if _, err := run(t, `upper([1])`); err == nil || !strings.Contains(err.Error(), "нужна строка") {
		t.Errorf("upper списка: %v", err)
	}
}
