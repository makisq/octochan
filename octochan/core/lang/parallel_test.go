package lang

import (
	"context"
	"strings"
	"testing"
)

func TestTryCatchContinuesAfterError(t *testing.T) {
	// Основной сценарий предметной области: часть целей упала,
	// остальные обработаны, отчёт получен.
	src := `
for сервер in ["srv1", "srv2", "srv3"] {
    try {
        if сервер == "srv2" { fail("сервер недоступен") }
        print(сервер, "ок")
    } catch ошибка {
        print(сервер, "не удалось:", ошибка)
    }
}
print("готово")
`
	got := strings.TrimSpace(mustRun(t, src))
	want := strings.Join([]string{
		"srv1 ок",
		"srv2 не удалось: сервер недоступен",
		"srv3 ок",
		"готово",
	}, "\n")

	if got != want {
		t.Errorf("получено:\n%s\nожидалось:\n%s", got, want)
	}
}

func TestTryWithoutErrorVariable(t *testing.T) {
	src := `
try { fail("что-то") } catch { print("перехвачено") }
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "перехвачено" {
		t.Errorf("получено %q", got)
	}
}

func TestTryRequiresCatch(t *testing.T) {
	// try без catch означал бы «проглотить ошибку молча» — ровно то
	// поведение, из-за которого сбои остаются незамеченными.
	if _, err := run(t, `try { fail("x") }`); err == nil {
		t.Fatal("try без catch разобрался")
	}
}

func TestTryDoesNotCatchControlFlow(t *testing.T) {
	// break, continue и return — управление, а не ошибки.
	src := `
for n in [1, 2, 3] {
    try {
        if n == 2 { break }
        print(n)
    } catch { print("перехвачено", n) }
}
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "1" {
		t.Errorf("получено %q: break был перехвачен catch", got)
	}

	src = `
alias проверить() {
    try { return "из try" } catch { return "из catch" }
}
print(проверить())
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "из try" {
		t.Errorf("получено %q: return был перехвачен catch", got)
	}
}

func TestTryCatchNested(t *testing.T) {
	src := `
try {
    try { fail("внутренняя") } catch e { fail("внешняя после " + e) }
} catch e {
    print(e)
}
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "внешняя после внутренняя" {
		t.Errorf("получено %q", got)
	}
}

func TestParallelForOutputIsDeterministic(t *testing.T) {
	// Итерации выполняются одновременно, но вывод обязан быть
	// воспроизводимым, иначе результат нельзя сравнить с предыдущим прогоном.
	src := `parallel for n in [1, 2, 3, 4, 5, 6, 7, 8] { print("сервер", n) }`

	first := mustRun(t, src)
	for i := 0; i < 15; i++ {
		if got := mustRun(t, src); got != first {
			t.Fatalf("вывод нестабилен:\n%s\nпротив\n%s", got, first)
		}
	}

	lines := strings.Split(strings.TrimSpace(first), "\n")
	if len(lines) != 8 {
		t.Fatalf("получено %d строк, ожидалось 8", len(lines))
	}
	if lines[0] != "сервер 1" || lines[7] != "сервер 8" {
		t.Errorf("порядок нарушен: %q ... %q", lines[0], lines[7])
	}
}

func TestParallelForCollectsAllFailures(t *testing.T) {
	// Отказ одной итерации не должен скрывать остальные.
	_, err := run(t, `
parallel for n in [1, 2, 3, 4] {
    if n == 2 { fail("вторая упала") }
    if n == 4 { fail("четвёртая упала") }
}
`)
	if err == nil {
		t.Fatal("отказы итераций не обнаружены")
	}

	msg := err.Error()
	for _, want := range []string{"2 из 4", "вторая упала", "четвёртая упала"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в сообщении нет %q: %s", want, msg)
		}
	}
}

func TestParallelForWithTryReportsAndContinues(t *testing.T) {
	// Полная связка: раскатать на несколько целей, поймать отказы,
	// продолжить и отчитаться.
	src := `
parallel for сервер in ["srv1", "srv2", "srv3", "srv4"] {
    try {
        if сервер == "srv2" { fail("недоступен") }
        print(сервер, "применено")
    } catch ошибка {
        print(сервер, "ПРОПУЩЕН:", ошибка)
    }
}
print("итог подведён")
`
	got := strings.TrimSpace(mustRun(t, src))
	want := strings.Join([]string{
		"srv1 применено",
		"srv2 ПРОПУЩЕН: недоступен",
		"srv3 применено",
		"srv4 применено",
		"итог подведён",
	}, "\n")

	if got != want {
		t.Errorf("получено:\n%s\nожидалось:\n%s", got, want)
	}
}

func TestParallelForIsolatesAssignments(t *testing.T) {
	// Тело видит копию внешних переменных, а его присваивания наружу
	// не выходят. Иначе две итерации писали бы в одну переменную
	// одновременно — это гонка, а не накопление.
	src := `
счётчик = 0
внешняя = "видна"
parallel for n in [1, 2, 3] {
    print(внешняя, n)
    счётчик = счётчик + n
}
print("счётчик снаружи:", счётчик)
`
	got := strings.TrimSpace(mustRun(t, src))
	if !strings.Contains(got, "видна 1") {
		t.Errorf("внешняя переменная не видна: %s", got)
	}
	if !strings.HasSuffix(got, "счётчик снаружи: 0") {
		t.Errorf("присваивание вышло наружу: %s", got)
	}
}

func TestParallelForDoesNotShareContainers(t *testing.T) {
	// Словарь — ссылочное значение; без копирования put() из разных
	// итераций писал бы в одну Go-map, что даёт гонку данных.
	// Тест осмыслен только под -race, но проверяет и семантику.
	src := `
общий = {"a": 0}
parallel for n in [1, 2, 3, 4, 5, 6] {
    put(общий, "a", n)
    put(общий, str(n), n)
}
print(len(общий), общий["a"])
`
	if got := strings.TrimSpace(mustRun(t, src)); got != "1 0" {
		t.Errorf("получено %q: изменения контейнера вышли за пределы итерации", got)
	}
}

func TestParallelForRejectsBreak(t *testing.T) {
	// Прерывать нечего: итерации уже идут одновременно.
	_, err := run(t, `parallel for n in [1, 2, 3] { break }`)
	if err == nil {
		t.Fatal("break внутри parallel for принят")
	}
	if !strings.Contains(err.Error(), "parallel") {
		t.Errorf("непонятное сообщение: %v", err)
	}
}

func TestParallelForEmptyList(t *testing.T) {
	if _, err := run(t, `parallel for n in [] { fail("не должно выполниться") }`); err != nil {
		t.Errorf("пустой список дал ошибку: %v", err)
	}
}

func TestParallelForTypedVariable(t *testing.T) {
	if _, err := run(t, `parallel for n: целое in [1, 2, 3] { print(n) }`); err != nil {
		t.Errorf("типизированная переменная цикла не принята: %v", err)
	}
	if _, err := run(t, `parallel for n: целое in [1, "два"] { print(n) }`); err == nil {
		t.Error("нарушение типа в parallel for не обнаружено")
	}
}

func TestParallelForRespectsCancelledContext(t *testing.T) {
	file, err := Parse("<тест>", `parallel for n in [1, 2, 3] { print(n) }`)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out strings.Builder
	interp := NewInterpreter(ctx, NewRegistry(), &out)
	if err := interp.RunFile(file); err == nil {
		t.Fatal("отменённый контекст не прервал параллельный цикл")
	}
}

func TestParallelForCallsAliases(t *testing.T) {
	// У каждой итерации свой стек вызовов: общий стек портился бы
	// одновременными вызовами.
	src := `
alias обработать(сервер) {
    return сервер + ":готово"
}
parallel for сервер in ["a", "b", "c"] {
    print(обработать(сервер))
}
`
	got := strings.TrimSpace(mustRun(t, src))
	if got != "a:готово\nb:готово\nc:готово" {
		t.Errorf("получено %q", got)
	}
}

func TestParallelForErrorMentionsFailedItem(t *testing.T) {
	// В сообщении должно быть видно, какая именно цель упала.
	_, err := run(t, `parallel for сервер in ["srv-ok", "srv-bad"] {
    if сервер == "srv-bad" { fail("отказ") }
}`)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if !strings.Contains(err.Error(), "srv-bad") {
		t.Errorf("в сообщении нет упавшей цели: %v", err)
	}
	if strings.Contains(err.Error(), "srv-ok") {
		t.Errorf("в сообщении упомянута успешная цель: %v", err)
	}
}
