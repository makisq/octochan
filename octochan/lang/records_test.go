package lang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const serverRecord = `record Сервер {
    имя: string
    порт: int = 5432
    теги: list = []
}
alias адрес(s: Сервер) { return s.имя + ":" + str(s.порт) }
`

func TestRecords(t *testing.T) {
	cases := map[string]string{
		`print(Сервер("db1"))`:                                 `Сервер(имя: "db1", порт: 5432, теги: [])`,
		`print(Сервер(имя: "db2", порт: 6432).порт)`:           "6432",
		`print(Сервер("db3", теги: ["prod"]).теги)`:            "[prod]",
		"s = Сервер(\"db1\")\ns.порт = 5433\nprint(s.адрес())": "db1:5433",
		// let запрещает новое значение переменной, а не изменение записи.
		"let s = Сервер(\"db1\")\ns.порт = 1\nprint(s.порт)": "1",
		// Запись передаётся как словарь: алиас меняет ту же запись.
		"alias сдвинуть(s: Сервер) { s.порт = s.порт + 1 }\ns = Сервер(\"a\")\nсдвинуть(s)\nprint(s.порт)": "5433",
		`print(Сервер("a") == Сервер("a"), Сервер("a") == Сервер("b"))`:                                    "true false",
		`print(to_json(Сервер("a")))`: `{"имя":"a","порт":5432,"теги":[]}`,
		`print(type_of(Сервер("a")), type_of(1), type_of(null), is(Сервер("a"), "Сервер"), is(1, "int"))`: "Сервер int null true true",
		// Вызов через точку — первый аргумент.
		`print(Сервер("db1").адрес(), "  Prod ".trim().lower(), [3, 1, 2].sort().len())`: "db1:5432 prod 3",
		// Поле-функция вызывается без самого значения.
		"m = {\"удвоить\": fn(x) => x * 2}\nprint(m.удвоить(21))": "42",
		// Словарь и список через точку и индекс.
		"m = {\"a\": 1}\nm.b = 2\nm[\"c\"] = 3\nprint(m, m.b)": `{"a": 1, "b": 2, "c": 3} 2`,
		"l = [1, 2, 3]\nl[-1] = 30\nprint(l)":                  "[1, 2, 30]",
		// Именованные аргументы алиаса.
		"alias вычесть(a, b) { return a - b }\nprint(вычесть(b: 1, a: 10), вычесть(10, b: 4))": "9 6",
		"f = fn(a, b) => a - b\nprint(f(b: 1, a: 3))":                                          "2",
		// Переменная перекрывает пакет с тем же именем: x.поле — её поле.
		"s = Сервер(\"x\")\nprint(s.имя)":                                       "x",
		`print(join(each([Сервер("a"), Сервер("b", порт: 1)], fn адрес), ","))`: "a:5432,b:1",
	}
	for body, want := range cases {
		if got := strings.TrimSpace(mustRun(t, serverRecord+body)); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", body, got, want)
		}
	}

	for body, want := range map[string]string{
		`Сервер()`:                            "не задано поле имя",
		`Сервер("a", 1, [], 4)`:               "принимает 3 аргумента, передано 4",
		`Сервер(имя: "a", хост: "b")`:         `нет поля "хост"`,
		`Сервер(1)`:                           "поле имя записи Сервер: объявлен тип строка",
		"s = Сервер(\"a\")\ns.порт = \"x\"":   "поле порт записи Сервер: объявлен тип целое",
		"s = Сервер(\"a\")\nprint(s.хост)":    `у записи Сервер нет поля "хост"; есть: имя, порт, теги`,
		"s = Сервер(\"a\")\ns = 5":            "тип Сервер задан первым значением",
		`print(5.поле)`:                       "у значения типа целое нет полей",
		`len(x: [1])`:                         "встроенные функции принимают аргументы только по порядку",
		"alias f(a) { return a }\nf(b: 1)":    `нет параметра "b"`,
		"alias f(a, b) { return a }\nf(b: 1)": "не передан параметр a",
	} {
		if _, err := run(t, serverRecord+body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", body, err, want)
		}
	}
}

const addressable = `interface Адресуемый {
    имя: string
    адрес()
}
alias описание(x: Адресуемый) { return "узел " + x.адрес() }
`

func TestInterfaces(t *testing.T) {
	cases := map[string]string{
		// Метод по умолчанию работает для любой подходящей записи.
		`print(описание(Сервер("db1")), Сервер("db2").описание())`:                                  "узел db1:5432 узел db2:5432",
		`print(is(Сервер("a"), "Адресуемый"), is({"имя": "a"}, "Адресуемый"), is(5, "Адресуемый"))`: "true false false",
		// Словарь с полями и полем-функцией подходит.
		"m = {\"имя\": \"m\", \"адрес\": fn() => \"m:1\"}\nprint(is(m, \"Адресуемый\"), описание(m))": "true узел m:1",
	}
	for body, want := range cases {
		if got := strings.TrimSpace(mustRun(t, serverRecord+addressable+body)); got != want {
			t.Errorf("%s\nполучено %q, ожидалось %q", body, got, want)
		}
	}
	_, err := run(t, serverRecord+addressable+`описание({"имя": "x"})`)
	if err == nil || !strings.Contains(err.Error(), "не подходит под интерфейс Адресуемый: нет метода адрес()") {
		t.Fatalf("неподходящий словарь: %v", err)
	}
	_, err = run(t, serverRecord+addressable+`Адресуемый()`)
	if err == nil || !strings.Contains(err.Error(), "интерфейс: его не создают") {
		t.Fatalf("создание интерфейса: %v", err)
	}
}

func TestRecordsInParallel(t *testing.T) {
	src := serverRecord + `s = Сервер("a")
parallel for i in range(8) {
    s.порт = i
    print(i, s.порт)
}
print(s.порт)`
	out := mustRun(t, src)
	if !strings.Contains(out, "0 0\n1 1\n") || !strings.HasSuffix(strings.TrimSpace(out), "\n5432") {
		t.Fatalf("parallel: %q", out)
	}
}

func TestParseDeclarations(t *testing.T) {
	for src, want := range map[string]string{
		"record R { a: int\n a: int }":       `поле "a" объявлено дважды`,
		"record R { a: int b: int }":         "лишнее в теле записи R",
		"if true { record R { a } }":         "record можно писать только на верхнем уровне",
		"interface I {\n    m()\n    m()\n}": `"m" объявлено в интерфейсе дважды`,
		"x = import":                         "неожиданный import",
		"print(1); import devops":            "import пишется отдельной строкой",
		"f(a: 1, 2)":                         "аргументы по порядку пишутся до именованных",
		"f(a: 1, a: 2)":                      "аргумент a передан дважды",
		"1 = 2":                              "присвоить можно переменной, полю",
		"f() = 2":                            "присвоить можно переменной, полю",
	} {
		if _, err := Parse("<тест>", src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
	file, err := Parse("<тест>", "import devops v2.1 as d # проверки\nimport net\nprint(1)")
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Imports) != 2 || *file.Imports[0] != (ImportDecl{Pos: file.Imports[0].Pos, Package: "devops", Version: "2.1", As: "d"}) ||
		file.Imports[1].Package != "net" || len(file.Stmts) != 1 {
		t.Fatalf("импорты: %+v %+v", file.Imports[0], file.Imports[1])
	}
}

func TestCheckRecords(t *testing.T) {
	for body, want := range map[string]string{
		"x: Нет = 1":                        `неизвестный тип "Нет"`,
		"alias f(x: Нет) { return x }":      `неизвестный тип "Нет"`,
		"s = Сервер(\"a\")\nprint(s.хост)":  `у записи Сервер нет поля "хост"`,
		"s = Сервер(\"a\")\ns.порт = \"x\"": "поле порт: объявлен тип целое",
		`Сервер()`:                          "не задано поле имя",
		`Сервер(хост: "a")`:                 `нет поля "хост"`,
		`Сервер(1)`:                         "объявлен тип строка, передано значение типа целое",
		"x: string = Сервер(\"a\")":         "объявлен тип строка",
		"alias f(a) { return a }\nf(b: 1)":  `нет параметра "b"`,
		`len(x: [1])`:                       "только по порядку",
		"record R { a: int = \"x\" }":       "значение по умолчанию типа строка",
		"x = 5\nprint(x.поле)":              "у значения типа целое нет полей",
		"alias g(x: Адресуемый) { return x }\nrecord Голый { имя: string }\ng(Голый(\"a\"))": "объявлен тип Адресуемый",
		`Адресуемый()`: "интерфейс: его не создают",
	} {
		problems := check(t, serverRecord+addressable+body)
		found := false
		for _, p := range problems {
			if strings.Contains(p.Msg, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: проблемы %v, ожидалось %q", body, problems, want)
		}
	}
	for _, body := range []string{
		"s = Сервер(\"a\", порт: 1)\nprint(s.адрес(), s.имя, описание(s), s.описание())",
		"m = {\"a\": 1}\nm.b = 2\nprint(m.a, m.f(1))",
		"alias f(x: Адресуемый) { return адрес(x) }",
		"print(\"a\".upper(), [1].len())",
		"alias вычесть(a, b) { return a - b }\nprint(вычесть(b: 1, a: 2))",
	} {
		if problems := check(t, serverRecord+addressable+body); len(problems) > 0 {
			t.Errorf("%s: ложные проблемы %v", body, problems)
		}
	}
}

// Пакет с версией в каталоге: import проверяет версию и даёт второе имя.
func TestImport(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "devops")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pkg, ManifestName), "name: devops\nversion: 2.1.0\n")
	writeFile(t, filepath.Join(pkg, "net.ochan"), "record Узел { хост: string }\nalias проверить(u: Узел) { return u.хост != \"\" }\n")

	registry := NewRegistry()
	if err := registry.LoadDir("devops", pkg); err != nil {
		t.Fatal(err)
	}

	runImport := func(src string) (string, error) {
		file, err := Parse("s.ochan", src)
		if err != nil {
			return "", err
		}
		own := registry.Clone()
		if err := own.AddFile("", file); err != nil {
			return "", err
		}
		if problems := Check(file, own); len(problems) > 0 {
			return "", &RuntimeError{Msg: problems[0].String()}
		}
		var out strings.Builder
		err = NewInterpreter(context.Background(), own, &out).RunFile(file)
		return out.String(), err
	}

	out, err := runImport("import devops v2 as d\nu = d.Узел(\"db1\")\nalias f(x: d.Узел) { return x.хост }\nprint(u, d.проверить(u), u.проверить(), f(u))")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != `devops.Узел(хост: "db1") true true db1` {
		t.Fatalf("вывод: %q", out)
	}
	for src, want := range map[string]string{
		"import devops v3":   "import devops v3: подключена версия 2.1.0",
		"import devops v2.2": "подключена версия 2.1.0",
		"import nope":        "import nope: пакет не подключён; подключены: devops",
		"import devops as devops\nimport devops as x\nimport devops as x": "",
	} {
		_, err := runImport(src)
		if want == "" {
			if err != nil {
				t.Errorf("%s: %v", src, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
	for _, src := range []string{"import devops v2.1.0", "import devops 2", "import devops"} {
		if _, err := runImport(src); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}

	// Второе имя из одного сценария не видно в другом.
	if _, err := runImport("print(d.проверить)"); err == nil {
		t.Fatal("второе имя пакета утекло в другой сценарий")
	}

	// Зависимости заявки — и алиас, и запись пакета.
	file, err := Parse("s.ochan", "import devops as d\nu = d.Узел(\"a\")\nprint(u.проверить())")
	if err != nil {
		t.Fatal(err)
	}
	own := registry.Clone()
	if err := own.AddFile("", file); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, dep := range Dependencies(file, own) {
		names = append(names, dep.Module+"."+dep.Name)
	}
	if got := strings.Join(names, ","); got != "devops.Узел,devops.проверить" {
		t.Fatalf("зависимости: %s", got)
	}

	// В библиотеке import не пишут.
	writeFile(t, filepath.Join(pkg, "bad.ochan"), "import devops\n")
	if err := NewRegistry().LoadDir("devops", pkg); err == nil || !strings.Contains(err.Error(), "import пишут в сценарии") {
		t.Fatalf("import в библиотеке: %v", err)
	}
}
