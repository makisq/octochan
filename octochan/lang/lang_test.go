package lang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run выполняет исходник и возвращает напечатанное.
func run(t *testing.T, src string) (string, error) {
	t.Helper()

	file, err := Parse("<тест>", src)
	if err != nil {
		return "", err
	}

	registry := NewRegistry()
	for _, decl := range file.Aliases {
		if err := registry.Add("", decl); err != nil {
			return "", err
		}
	}

	var out strings.Builder
	interp := NewInterpreter(context.Background(), registry, &out)
	// Выполнение обязано пройти до чтения буфера: в записи вида
	// `return out.String(), interp.RunFile(file)` Go вычисляет out.String()
	// первым и всегда отдаёт пустую строку.
	err = interp.RunFile(file)
	return out.String(), err
}

// mustRun требует успешного выполнения.
func mustRun(t *testing.T, src string) string {
	t.Helper()
	out, err := run(t, src)
	if err != nil {
		t.Fatalf("выполнение не удалось: %v\nисходник:\n%s", err, src)
	}
	return out
}

func TestArithmeticAndComparison(t *testing.T) {
	cases := map[string]string{
		`print(2 + 3 * 4)`:        "14",
		`print((2 + 3) * 4)`:      "20",
		`print(7 / 2)`:            "3.5",
		`print(7 % 2)`:            "1",
		`print(-5 + 2)`:           "-3",
		`print(1 < 2)`:            "true",
		`print(2 <= 2)`:           "true",
		`print("a" + "b")`:        "ab",
		`print(1 == 1.0)`:         "true",
		`print("8" == 8)`:         "true",
		`print(not 0)`:            "true",
		`print(true and false)`:   "false",
		`print(false or 1)`:       "true",
		`print([1, 2] + [3])`:     "[1, 2, 3]",
		`print(len([1, 2, 3]))`:   "3",
		`print([10, 20, 30][1])`:  "20",
		`print([10, 20, 30][-1])`: "30",
	}

	for src, want := range cases {
		t.Run(src, func(t *testing.T) {
			got := strings.TrimSpace(mustRun(t, src))
			if got != want {
				t.Errorf("получено %q, ожидалось %q", got, want)
			}
		})
	}
}

func TestShortCircuit(t *testing.T) {
	// Правая часть and не должна вычисляться, если левая ложна:
	// иначе fail() прервал бы сценарий.
	if _, err := run(t, `print(false and fail("не должно выполниться"))`); err != nil {
		t.Errorf("and вычислил правую часть: %v", err)
	}
	if _, err := run(t, `print(true or fail("не должно выполниться"))`); err != nil {
		t.Errorf("or вычислил правую часть: %v", err)
	}
}

func TestIfElseChain(t *testing.T) {
	src := `
x = 5
if x > 10 {
    print("больше")
} else if x > 3 {
    print("средне")
} else {
    print("меньше")
}
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "средне" {
		t.Errorf("получено %q", got)
	}
}

func TestForLoop(t *testing.T) {
	src := `
сумма = 0
for n in [1, 2, 3, 4] {
    if n == 3 { continue }
    if n == 4 { break }
    сумма = сумма + n
}
print(сумма)
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "3" {
		t.Errorf("получено %q, ожидалось 3", got)
	}
}

func TestForOverLines(t *testing.T) {
	// Строка обходится построчно — это делает конвейер полезным в цикле.
	src := `
for строка in lines("a\nb\nc") {
    print(строка)
}
`
	got := strings.TrimSpace(mustRun(t, src))
	if got != "a\nb\nc" {
		t.Errorf("получено %q", got)
	}
}

func TestAliasWithParamsAndRecursion(t *testing.T) {
	src := `
alias факториал(n) {
    if n <= 1 { return 1 }
    return n * факториал(n - 1)
}
print(факториал(5))
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "120" {
		t.Errorf("получено %q, ожидалось 120", got)
	}
}

func TestAliasCallsAlias(t *testing.T) {
	src := `
alias удвоить(x) { return x * 2 }
alias учетверить(x) { return удвоить(удвоить(x)) }
print(учетверить(3))
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "12" {
		t.Errorf("получено %q, ожидалось 12", got)
	}
}

func TestInfiniteRecursionIsStopped(t *testing.T) {
	// Без ограничения глубины это повесило бы процесс.
	_, err := run(t, `
alias бесконечность() { return бесконечность() }
бесконечность()
`)
	if err == nil {
		t.Fatal("ожидалась ошибка о слишком глубокой рекурсии")
	}
	if !strings.Contains(err.Error(), "рекурси") {
		t.Errorf("непонятное сообщение: %v", err)
	}
}

func TestAliasDoesNotSeeCallerVariables(t *testing.T) {
	// Изоляция окружения: иначе алиасы из чужого пакета могли бы
	// незаметно зависеть от переменных вызывающего.
	_, err := run(t, `
alias показать() { return секрет }
секрет = 42
показать()
`)
	if err == nil {
		t.Fatal("алиас увидел переменную вызывающего")
	}
}

func TestArityMismatch(t *testing.T) {
	_, err := run(t, `
alias пара(a, b) { return a }
пара(1)
`)
	if err == nil || !strings.Contains(err.Error(), "принимает 2") {
		t.Fatalf("ожидалась ошибка об арности, получено: %v", err)
	}
}

func TestPipeline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.conf")
	if err := os.WriteFile(path, []byte("shared_buffers = 8GB\nmax_connections = 100\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// read даёт текст, validate принимает его из конвейера.
	src := `print(read("` + filepath.ToSlash(path) + `") | validate())`
	if got := strings.TrimSpace(mustRun(t, src)); got != "2" {
		t.Errorf("получено %q, ожидалось 2", got)
	}
}

func TestPipeBindsTighterThanComparison(t *testing.T) {
	// a | b != 0 должно читаться как (a | b) != 0.
	src := `print(lines("x\ny") | len() != 0)`
	if got := strings.TrimSpace(mustRun(t, src)); got != "true" {
		t.Errorf("получено %q, ожидалось true", got)
	}
}

func TestDiffBuiltin(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "prod.conf")
	right := filepath.Join(dir, "test.conf")

	// 8GB и 8192MB равны, max_connections различается.
	if err := os.WriteFile(left, []byte("shared_buffers = 8GB\nmax_connections = 100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(right, []byte("shared_buffers = 8192MB\nmax_connections = 200\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := `print(diff("` + filepath.ToSlash(left) + `", "` + filepath.ToSlash(right) + `"))`
	if got := strings.TrimSpace(mustRun(t, src)); got != "1" {
		t.Errorf("получено %q, ожидалось 1 расхождение", got)
	}
}

func TestTypeAnnotationsAreOptional(t *testing.T) {
	// Без аннотаций всё работает как раньше — это условие «необязательности».
	src := `
без_типа = 5
с_типом: целое = 7
латиницей: string = "ок"
print(без_типа + с_типом, латиницей)
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "12 ок" {
		t.Errorf("получено %q", got)
	}
}

func TestTypeMismatchOnDeclaration(t *testing.T) {
	// Строка не считается числом намеренно: иначе аннотация не ловила бы
	// ровно ту ошибку, ради которой её пишут.
	_, err := run(t, `порт: целое = "5432"`)
	if err == nil {
		t.Fatal("строка присвоена переменной типа целое без ошибки")
	}
	if !strings.Contains(err.Error(), "целое") {
		t.Errorf("непонятное сообщение: %v", err)
	}
}

func TestDeclaredTypeSurvivesReassignment(t *testing.T) {
	// Аннотация действует не только в момент объявления.
	_, err := run(t, `
счётчик: целое = 0
счётчик = "уже не число"
`)
	if err == nil {
		t.Fatal("повторное присваивание обошло объявленный тип")
	}
}

func TestIntWidensToNumber(t *testing.T) {
	// Целое там, где объявлено число или дробное, — расширение без потерь.
	if _, err := run(t, `x: число = 5`); err != nil {
		t.Errorf("целое не принято как число: %v", err)
	}
	if _, err := run(t, `y: дробное = 5`); err != nil {
		t.Errorf("целое не принято как дробное: %v", err)
	}
	if _, err := run(t, `z: целое = 5.5`); err == nil {
		t.Error("дробное принято как целое")
	}
}

func TestParamTypeCheckedAtCall(t *testing.T) {
	src := `
alias повторить(текст: строка, раз: целое) {
    return раз
}
повторить(42, 2)
`
	_, err := run(t, src)
	if err == nil {
		t.Fatal("число передано в параметр типа строка без ошибки")
	}
	for _, want := range []string{"повторить", "текст", "строка"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("в сообщении нет %q: %v", want, err)
		}
	}
}

func TestTypedLoopVariable(t *testing.T) {
	if _, err := run(t, `for n: целое in [1, 2, 3] { print(n) }`); err != nil {
		t.Errorf("корректный цикл не прошёл: %v", err)
	}
	if _, err := run(t, `for n: целое in [1, "два"] { print(n) }`); err == nil {
		t.Error("строка в цикле с типом целое прошла без ошибки")
	}
}

func TestUnknownTypeName(t *testing.T) {
	_, err := run(t, `x: непонятно = 1`)
	if err == nil || !strings.Contains(err.Error(), "неизвестный тип") {
		t.Fatalf("ожидалась ошибка о неизвестном типе, получено: %v", err)
	}
}

// writeConf создаёт временный конфиг и возвращает путь с прямыми слэшами,
// пригодный для вставки в исходник сценария.
func writeConf(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(path)
}

func TestMapLiteralAndIndex(t *testing.T) {
	src := `
адреса = {"srv1": "10.0.0.1", "srv2": "10.0.0.2"}
print(адреса["srv2"], len(адреса))
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "10.0.0.2 2" {
		t.Errorf("получено %q", got)
	}
}

func TestMapLiteralIsNotConfusedWithBlock(t *testing.T) {
	// Фигурные скобки заняты блоками; литерал распознаётся там,
	// где ожидается выражение.
	src := `
карта = {"a": 1}
if карта["a"] == 1 {
    print("ок")
}
for ключ in {"x": 1, "y": 2} {
    print(ключ)
}
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "ок\nx\ny" {
		t.Errorf("получено %q", got)
	}
}

func TestMapIterationIsStable(t *testing.T) {
	// Обход map в Go случаен; сценарий обязан печатать одно и то же.
	src := `for k in {"в": 1, "а": 2, "б": 3} { print(k) }`
	first := mustRun(t, src)
	for i := 0; i < 20; i++ {
		if got := mustRun(t, src); got != first {
			t.Fatalf("порядок обхода нестабилен: %q против %q", got, first)
		}
	}
}

func TestMapHelpers(t *testing.T) {
	src := `
м = map("a", 1, "b", 2)
print(keys(м), values(м))
print(has(м, "a"), has(м, "нет"))
print(get(м, "нет", "по умолчанию"))
put(м, "c", 3)
print(len(м))
print(merge({"a": 1}, {"a": 9, "b": 2}))
`
	got := strings.TrimSpace(mustRun(t, src))
	want := strings.Join([]string{
		`[a, b] [1, 2]`,
		`true false`,
		`по умолчанию`,
		`3`,
		`{"a": 9, "b": 2}`,
	}, "\n")
	if got != want {
		t.Errorf("получено:\n%s\nожидалось:\n%s", got, want)
	}
}

func TestConfigValueIsParsedOnce(t *testing.T) {
	path := writeConf(t, "prod.conf", "shared_buffers = 8GB\nmax_connections = 100\n")

	src := `
цель = config("` + path + `")
print(цель["max_connections"], цель["shared_buffers"], len(цель))
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "100 8GB 2" {
		t.Errorf("получено %q", got)
	}
}

func TestConfigStillWorksAfterFileIsGone(t *testing.T) {
	// Разобранный конфиг живёт в переменной и не привязан к файлу:
	// это и есть доказательство, что повторного чтения не происходит.
	dir := t.TempDir()
	path := filepath.Join(dir, "prod.conf")
	if err := os.WriteFile(path, []byte("max_connections = 100\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	file, err := Parse("<тест>", `цель = config("`+filepath.ToSlash(path)+`")`)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	interp := NewInterpreter(context.Background(), NewRegistry(), &out)
	if err := interp.RunFile(file); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// Повторный разбор здесь упал бы: файла больше нет.
	cfg, err := interp.configOf(StringValue("нет-такого-файла"))
	if err == nil {
		t.Fatalf("несуществующий путь разобрался: %+v", cfg)
	}
}

func TestConfigDiffAcceptsParsedValues(t *testing.T) {
	left := writeConf(t, "a.conf", "shared_buffers = 8GB\nmax_connections = 100\n")
	right := writeConf(t, "b.conf", "shared_buffers = 8192MB\nmax_connections = 200\n")

	src := `
эталон = config("` + left + `")
цель = config("` + right + `")
print(diff(эталон, цель), diff_list(эталон, цель))
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "1 [max_connections]" {
		t.Errorf("получено %q", got)
	}
}

func TestConfigSections(t *testing.T) {
	path := writeConf(t, "s.conf", "[server]\nport = 5432\n[client]\nport = 6432\ntimeout = 30\n")

	src := `
c = config("` + path + `")
print(c["server.port"], c["timeout"])
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "5432 30" {
		t.Errorf("получено %q", got)
	}

	// Короткое имя, встречающееся в двух секциях, не должно разрешаться молча.
	_, err := run(t, `print(config("`+path+`")["port"])`)
	if err == nil || !strings.Contains(err.Error(), "уточните") {
		t.Errorf("неоднозначный параметр разрешился без ошибки: %v", err)
	}
}

func TestConfigPutAndSave(t *testing.T) {
	dir := t.TempDir()
	src := writeConf(t, "in.conf", "max_connections = 100\n")
	out := filepath.ToSlash(filepath.Join(dir, "out.conf"))

	script := `
c = config("` + src + `")
put(c, "max_connections", "200")
put(c, "server.port", "5433")
save(c, "` + out + `")
print(c["max_connections"], c["server.port"])
`
	if got := strings.TrimSpace(mustRun(t, script)); got != "200 5433" {
		t.Errorf("получено %q", got)
	}

	// Записанный файл должен читаться обратно тем же парсером.
	reread := `print(config("` + out + `")["server.port"])`
	if got := strings.TrimSpace(mustRun(t, reread)); got != "5433" {
		t.Errorf("после записи и чтения получено %q", got)
	}
}

func TestConfigAndMapTypeAnnotations(t *testing.T) {
	path := writeConf(t, "t.conf", "max_connections = 100\n")

	src := `
цель: конфиг = config("` + path + `")
адреса: словарь = {"srv1": "10.0.0.1"}
print(цель["max_connections"], адреса["srv1"])
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "100 10.0.0.1" {
		t.Errorf("получено %q", got)
	}

	if _, err := run(t, `x: конфиг = "просто строка"`); err == nil {
		t.Error("строка присвоена переменной типа конфиг")
	}
	if _, err := run(t, `y: словарь = [1, 2]`); err == nil {
		t.Error("список присвоен переменной типа словарь")
	}
}

func TestMissingKeyErrorsList(t *testing.T) {
	// Сообщение должно подсказывать, какие ключи есть.
	_, err := run(t, `print({"a": 1, "b": 2}["в"])`)
	if err == nil {
		t.Fatal("ожидалась ошибка об отсутствующем ключе")
	}
	if !strings.Contains(err.Error(), "a, b") {
		t.Errorf("нет списка ключей: %v", err)
	}
}

func TestNamespaces(t *testing.T) {
	registry := NewRegistry()

	userFile, err := Parse("user.ochan", `alias проверить() { return "пользовательский" }`)
	if err != nil {
		t.Fatal(err)
	}
	pkgFile, err := Parse("devops.ochan", `alias проверить() { return "из пакета" }`)
	if err != nil {
		t.Fatal(err)
	}

	if err := registry.Add("", userFile.Aliases[0]); err != nil {
		t.Fatal(err)
	}
	if err := registry.Add("devops", pkgFile.Aliases[0]); err != nil {
		t.Fatal(err)
	}

	// Короткое имя отдаёт пользовательский алиас.
	decl, err := registry.Lookup("", "проверить")
	if err != nil {
		t.Fatal(err)
	}
	if decl.Module != "" {
		t.Errorf("короткое имя ушло в пакет %q", decl.Module)
	}

	// Квалифицированное — из пакета.
	decl, err = registry.Lookup("devops", "проверить")
	if err != nil {
		t.Fatal(err)
	}
	if decl.Module != "devops" {
		t.Errorf("получен алиас из %q", decl.Module)
	}
}

func TestAmbiguousShortNameIsError(t *testing.T) {
	// Два пакета с одинаковым именем алиаса не должны разрешаться молча.
	registry := NewRegistry()
	for _, pkg := range []string{"пакет_а", "пакет_б"} {
		file, err := Parse(pkg+".ochan", `alias общий() { return 1 }`)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Add(pkg, file.Aliases[0]); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := registry.Lookup("", "общий"); err == nil {
		t.Fatal("неоднозначное имя разрешилось без ошибки")
	} else if !strings.Contains(err.Error(), "уточните пакет") {
		t.Errorf("непонятное сообщение: %v", err)
	}
}

func TestDuplicateAliasInSameSpace(t *testing.T) {
	registry := NewRegistry()
	file, err := Parse("dup.ochan", "alias повтор() { return 1 }\nalias повтор() { return 2 }")
	if err != nil {
		t.Fatal(err)
	}

	if err := registry.Add("", file.Aliases[0]); err != nil {
		t.Fatal(err)
	}
	if err := registry.Add("", file.Aliases[1]); err == nil {
		t.Fatal("повторное объявление прошло без ошибки")
	}
}

func TestParseErrorsAreLocated(t *testing.T) {
	cases := map[string]string{
		"незакрытый блок":      "alias a() {",
		"число с единицами":    `x = 8GB`,
		"двойной амперсанд":    `x = 1 && 2`,
		"восклицательный знак": `x = !1`,
		"лишнее после команды": "print(1) print(2)",
		"незакрытая строка":    `x = "abc`,
		"алиас внутри блока":   "if true { alias a() { return 1 } }",
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := run(t, src); err == nil {
				t.Fatalf("ожидалась ошибка разбора для %q", src)
			}
		})
	}
}

func TestUnknownNameSuggestsAlternative(t *testing.T) {
	_, err := run(t, `prnt("привет")`)
	if err == nil {
		t.Fatal("ожидалась ошибка о неизвестной функции")
	}
	if !strings.Contains(err.Error(), "print") {
		t.Errorf("нет подсказки про print: %v", err)
	}
}

func TestErrorMentionsPositionAndStack(t *testing.T) {
	_, err := run(t, `
alias внешний() { return внутренний() }
alias внутренний() { return fail("сломалось") }
внешний()
`)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}

	msg := err.Error()
	for _, want := range []string{"сломалось", "внешний", "внутренний"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в сообщении нет %q: %s", want, msg)
		}
	}
}

func TestContextCancellationStopsLoop(t *testing.T) {
	file, err := Parse("<тест>", `
alias бесконечный_цикл() {
    for x in [1, 2, 3] { print(x) }
}
бесконечный_цикл()
`)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // отменяем заранее

	var out strings.Builder
	interp := NewInterpreter(ctx, NewRegistry(), &out)
	for _, decl := range file.Aliases {
		if err := interp.registry.Add("", decl); err != nil {
			t.Fatal(err)
		}
	}

	if err := interp.RunFile(file); err == nil {
		t.Fatal("отменённый контекст не прервал выполнение")
	}
}

func TestRegistryCloneIsIndependent(t *testing.T) {
	base := NewRegistry()
	file, err := Parse("lib.ochan", "alias shared() { return 1 }")
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Add("lib", file.Aliases[0]); err != nil {
		t.Fatal(err)
	}

	script, err := Parse("сценарий.ochan", "alias own() { return 2 }")
	if err != nil {
		t.Fatal(err)
	}
	// Тот же сценарий дважды — как два запуска в одной оболочке.
	for i := 0; i < 2; i++ {
		clone := base.Clone()
		if err := clone.Add("", script.Aliases[0]); err != nil {
			t.Fatalf("запуск %d: %v", i+1, err)
		}
		if _, err := clone.Lookup("", "shared"); err != nil {
			t.Fatalf("копия потеряла алиас пакета: %v", err)
		}
	}
	if _, err := base.Lookup("", "own"); err == nil {
		t.Fatal("алиас копии попал в оригинал")
	}
}

func writeManifest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPackageManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.ochan"), []byte("alias f() { return 1 }"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Без манифеста пакет просто без версии.
	registry := NewRegistry()
	if err := registry.LoadDir("pgconf", dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Package("pgconf"); ok {
		t.Fatal("у пакета без манифеста появилось описание")
	}

	writeManifest(t, dir, "# скилл\nname: pgconf\nversion: v1.2.0\ndescription: \"правка параметров\"\n")
	registry = NewRegistry()
	if err := registry.LoadDir("pgconf", dir); err != nil {
		t.Fatal(err)
	}
	pkg, ok := registry.Package("pgconf")
	if !ok || pkg.Version != "1.2.0" || pkg.Description != "правка параметров" {
		t.Fatalf("описание пакета: %+v", pkg)
	}
	if clone, _ := registry.Clone().Package("pgconf"); clone != pkg {
		t.Fatal("копия реестра потеряла описание пакета")
	}

	cases := map[string]string{
		"version: 1.2.3.4\n":            "ожидалось вида 1, 1.2 или 1.2.3",
		"version: latest\n":             "ожидалось вида",
		"verison: 1.0\n":                "неизвестное поле \"verison\"",
		"name: other\nversion: 1\n":     "имя пакета задаёт каталог",
		"description: x\n":              "не указана version",
		"[deps]\nother = 1\nversion: 1": "секции не поддерживаются",
	}
	for content, want := range cases {
		writeManifest(t, dir, content)
		err := NewRegistry().LoadDir("pgconf", dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("манифест %q: ожидалась ошибка с %q, получено %v", content, want, err)
		}
	}
}
