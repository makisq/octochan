package lang

import (
	"strings"
	"testing"
)

// check разбирает исходник и проверяет его без выполнения.
func check(t *testing.T, src string) []Problem {
	t.Helper()

	file, err := Parse("<тест>", src)
	if err != nil {
		t.Fatalf("разбор не удался: %v\nисходник:\n%s", err, src)
	}
	registry := NewRegistry()
	for _, decl := range file.Aliases {
		if err := registry.Add("", decl); err != nil {
			t.Fatalf("регистрация не удалась: %v", err)
		}
	}
	return Check(file, registry)
}

func joinProblems(problems []Problem) string {
	lines := make([]string, len(problems))
	for i, p := range problems {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// Главное, ради чего проверка существует: ошибка в ветке, до которой
// выполнение не дошло, видна до запуска.
func TestCheckFindsErrorInBranchThatDoesNotRun(t *testing.T) {
	src := "x = 1\nif x > 5 {\n    print(unknown_var)\n}\n"

	if _, err := run(t, src); err != nil {
		t.Fatalf("выполнение не должно доходить до ошибки: %v", err)
	}

	problems := check(t, src)
	if len(problems) != 1 {
		t.Fatalf("ожидалась одна ошибка, получено:\n%s", joinProblems(problems))
	}
	if got := problems[0]; got.Pos.Line != 3 || !strings.Contains(got.Msg, `"unknown_var"`) {
		t.Fatalf("неверная находка: %s", got)
	}
}

// Каждая находка проверки — настоящая ошибка выполнения. Исходники
// построены так, что ошибочный код выполняется, и интерпретатор обязан
// на нём упасть. Иначе проверка отвергала бы рабочие сценарии.
func TestCheckFindingsAreRuntimeErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"неизвестная функция", `print(diff_lst("a", "b"))`, `неизвестная функция "diff_lst"; возможно, имелось в виду "diff_list"`},
		{"неизвестное имя", `print(nope)`, `неизвестное имя "nope"`},
		{"переменная блока не видна снаружи", "if true { y = 1 }\nprint(y)", `неизвестное имя "y"`},
		{"переменная try не видна снаружи", "try { q = 1 } catch { }\nprint(q)", `неизвестное имя "q"`},
		{"переменная catch не видна снаружи", "try { fail(\"x\") } catch e { }\nprint(e)", `неизвестное имя "e"`},
		{"переменная цикла не видна снаружи", "for i in [1] { }\nprint(i)", `неизвестное имя "i"`},
		{"алиас не видит переменные вызывающего", "x = 1\nalias f() { return x }\nf()", `неизвестное имя "x"`},
		{"число аргументов алиаса", "alias f(a) { return a }\nf(1, 2)", "алиас f принимает 1 аргумент, передано 2"},
		{"алиас с параметрами без скобок", "alias f(a) { return a }\nprint(f)", "алиас f принимает 1 аргумент, передано 0"},
		{"число аргументов встроенной функции", `len()`, "len: нужно минимум 1 аргумент, передано 0"},
		{"встроенная функция без скобок", `x = diff`, "diff — встроенная функция"},
		{"break вне цикла", `break`, "break вне цикла"},
		{"continue в алиасе вне цикла", "alias f() { continue }\nf()", "continue допустим только внутри for"},
		{"return в parallel for", `parallel for i in [1] { return 1 }`, "return недопустим внутри parallel for"},
		{"break в parallel for", `parallel for i in [1] { break }`, "break недопустим внутри parallel for"},
		{"тип переменной", `x: int = "5"`, "переменная x: объявлен тип целое, присваивается значение типа строка"},
		{"тип при повторном присваивании", "x: int = 1\nx = \"a\"", "объявлен тип целое, присваивается значение типа строка"},
		{"null в типизированную переменную", `x: string = null`, "присваивается значение типа null"},
		{"тип параметра", "alias f(a: int) { return a }\nf(\"x\")", "параметр a алиаса f: объявлен тип целое, передано значение типа строка"},
		{"тип параметра из конвейера", "alias f(a: int, b: int) { return a }\nprint(\"x\" | f(1))", "параметр a алиаса f"},
		{"цикл по числу", `for i in 5 { print(i) }`, "по значению типа целое нельзя пройти циклом"},
		{"тип элемента цикла", `for i: int in ["a"] { print(i) }`, "элемент цикла i: объявлен тип целое"},
		{"список плюс число", `x = [1] + 2`, "к списку можно прибавить только список"},
		{"минус списка", `x = -[1]`, "унарный минус применим только к числу"},
		{"арифметика со словарём", `x = {"a": 1} * 2`, "неприменим к словарь"},
		{"деление на ноль", `x = 1 / 0`, "деление на ноль"},
		{"остаток от деления на ноль", `x = 1 % 0`, "деление на ноль"},
		{"индекс строки", `x = "abc"[0]`, "индексировать можно список, словарь или конфиг"},
		{"конвейер не в вызов", `x = 1 | 2`, "справа от | должен быть вызов"},
		{"имя файла без кавычек", `x = diff(prod.conf, "b.conf")`, "prod.conf выглядит как имя файла"},
		{"неизвестный пакет", `x = devops.проверить()`, `пакет алиасов "devops" не подключён`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := check(t, tc.src)
			if len(problems) == 0 {
				t.Fatalf("проверка не нашла ошибку в:\n%s", tc.src)
			}
			if !strings.Contains(joinProblems(problems), tc.want) {
				t.Fatalf("нет ожидаемой находки %q, получено:\n%s", tc.want, joinProblems(problems))
			}

			if _, err := run(t, tc.src); err == nil {
				t.Fatalf("проверка нашла ошибку, а выполнение прошло:\n%s\nнаходки:\n%s",
					tc.src, joinProblems(problems))
			}
		})
	}
}

// Ложная тревога хуже пропуска: сценарий, который работает, проверка
// пропускать обязана. Каждый исходник здесь сначала выполняется.
func TestCheckAcceptsWorkingPrograms(t *testing.T) {
	sources := map[string]string{
		"присваивания и ветки":     "x = 0\nif x == 0 { x = 1 } else if x > 1 { x = 2 } else { x = 3 }\nprint(x)",
		"переменная снаружи блока": "x = 0\nif true { x = 1 }\nprint(x)",
		"типы": "n: int = 1\nf: float = n\nk: number = 2.5\ns: string = \"a\"\nb: bool = n > 0\n" +
			"l: list = [1]\nm: map = {\"a\": 1}\na: any = null\nprint(n, f, k, s, b, l, m, a)",
		"алиасы до и после объявления": "print(f(2))\nalias f(n: int) { return g(n) * 2 }\nalias g(n) { return n + 1 }",
		"рекурсия":         "alias fact(n) { if n <= 1 { return 1 }\n return n * fact(n - 1) }\nprint(fact(5))",
		"алиас без скобок": "alias greet() { return \"hi\" }\nprint(greet)",
		"конвейер":         "alias twice(x) { return x * 2 }\nprint(lines(\"a\\nb\") | len | twice)\nprint(\"a\" | str())",
		"try и catch":      "try { fail(\"boom\") } catch e { print(\"caught\", e) }\ntry { fail() } catch { print(\"ok\") }",
		"break и continue": "for i in [1, 2, 3] { if i == 1 { continue }\n if i == 3 { break }\n print(i) }",
		"break во вложенном цикле parallel for": "parallel for i in [1, 2] {\n for j in [1, 2] { if j == 2 { break }\n print(i, j) }\n}",
		"return на верхнем уровне":              "print(1)\nreturn\nprint(2)",
		"return в алиасе внутри цикла":          "alias first(l) { for x in l { return x } }\nprint(first([7]))",
		"цикл по словарю и строке":              "m = {\"b\": 1, \"a\": 2}\nfor k in m { print(k, m[k]) }\nfor line in \"x\\ny\" { print(line) }",
		"алиас перекрывает встроенную функцию":  "alias len(a, b) { return a + b }\nprint(len(1, 2))",
		"переменная перекрывает алиас":          "alias f() { return 1 }\nf = 2\nprint(f)",
		"строка как число":                      "x = \"5\" - 1\ny = -\"3\"\nprint(x, y)",
		"сложение строк и чисел":                "print(\"a\" + 1, 1 + \"a\", [1] + [2])",
		"словарь как конфиг":                    "m = map(\"a\", 1)\nput(m, \"b\", 2)\nprint(keys(m), has(m, \"a\"), get(m, \"z\", 0))",
		"параметр принимает целое как дробное":  "alias half(x: float) { return x / 2 }\nprint(half(3))",
	}

	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			if _, err := run(t, src); err != nil {
				t.Fatalf("исходник должен выполняться: %v\n%s", err, src)
			}
			if problems := check(t, src); len(problems) != 0 {
				t.Fatalf("ложная тревога на рабочем сценарии:\n%s\nнаходки:\n%s", src, joinProblems(problems))
			}
		})
	}
}

// Проверка не останавливается на первой ошибке: модели или человеку
// нужно исправить всё за один заход, а не по одной ошибке за запуск.
func TestCheckReportsAllProblemsInOrder(t *testing.T) {
	problems := check(t, "print(a)\nprint(b)\nalias f() { print(c) }")
	if len(problems) != 3 {
		t.Fatalf("ожидались три находки, получено:\n%s", joinProblems(problems))
	}
	for i, line := range []int{1, 2, 3} {
		if problems[i].Pos.Line != line {
			t.Fatalf("находки не упорядочены по строкам:\n%s", joinProblems(problems))
		}
	}
}

func TestCheckRegistryFindsBrokenLibraryAlias(t *testing.T) {
	registry := NewRegistry()
	for module, src := range map[string]string{
		"":       "alias ok() { return 1 }",
		"devops": "alias broken() { return missing() }",
	} {
		file, err := Parse(module+".ochan", src)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Aliases {
			if err := registry.Add(module, decl); err != nil {
				t.Fatal(err)
			}
		}
	}

	problems := CheckRegistry(registry)
	if len(problems) != 1 || !strings.Contains(problems[0].Msg, `"missing"`) {
		t.Fatalf("ожидалась одна находка в пакете devops, получено:\n%s", joinProblems(problems))
	}
}

// Неоднозначное короткое имя — ошибка с объяснением, а не «неизвестная
// функция»: имя есть, просто в двух пакетах сразу.
func TestCheckAmbiguousAliasExplainsItself(t *testing.T) {
	registry := NewRegistry()
	for _, module := range []string{"a", "b"} {
		file, err := Parse(module+".ochan", "alias same() { return 1 }")
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Add(module, file.Aliases[0]); err != nil {
			t.Fatal(err)
		}
	}

	file, err := Parse("<тест>", "print(same())\nprint(same)")
	if err != nil {
		t.Fatal(err)
	}
	problems := Check(file, registry)
	if len(problems) != 2 {
		t.Fatalf("ожидались две находки, получено:\n%s", joinProblems(problems))
	}
	for _, p := range problems {
		if !strings.Contains(p.Msg, "есть сразу в пакетах a, b") {
			t.Fatalf("неоднозначность не объяснена: %s", p)
		}
	}
}

func TestUsesFollowsCallsThroughLibrary(t *testing.T) {
	registry := NewRegistry()
	for module, src := range map[string]string{
		"devops": "alias outer() { return inner() }\nalias inner() { return 1 }\nalias unused() { return 2 }",
	} {
		file, err := Parse(module+".ochan", src)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Aliases {
			if err := registry.Add(module, decl); err != nil {
				t.Fatal(err)
			}
		}
	}

	file, err := Parse("сценарий.ochan", "alias own() { return devops.outer() }\nprint(own())")
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Aliases {
		if err := registry.Add("", decl); err != nil {
			t.Fatal(err)
		}
	}

	var names []string
	for _, decl := range Uses(file, registry) {
		names = append(names, decl.Module+"."+decl.Name)
	}
	if got := strings.Join(names, ","); got != "devops.inner,devops.outer" {
		t.Fatalf("используемые алиасы: %s", got)
	}
}
