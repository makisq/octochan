package i18n

import (
	"errors"
	"strings"
	"testing"
)

func restore(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := Use(Source); err != nil {
			t.Fatal(err)
		}
	})
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"eng": "en", " English ": "en", "RU": "ru", "русский": "ru", "JP": "jp", "de": "de"} {
		if got := Normalize(in); got != want {
			t.Errorf("%q → %q, ожидалось %q", in, got, want)
		}
	}
}

func TestUseAndFallback(t *testing.T) {
	restore(t)
	if T("Доступные команды:") != "Доступные команды:" {
		t.Fatal("русский — исходный язык")
	}
	if err := Use("eng"); err != nil {
		t.Fatal(err)
	}
	if Current() != "en" || T("Доступные команды:") != "Available commands:" {
		t.Fatalf("английский: %s, %q", Current(), T("Доступные команды:"))
	}
	if T("строка, которой нет в словаре") != "строка, которой нет в словаре" {
		t.Fatal("без перевода текст должен остаться как есть")
	}

	err := Use("jp")
	if err == nil || !strings.Contains(err.Error(), `язык "jp" не найден`) || Current() != "en" {
		t.Fatalf("неизвестный язык: %v, текущий %s", err, Current())
	}
}

// Язык модуля: чего нет в нём, берётся из английского; встроенный
// перевод модуль не переписывает.
func TestModuleLanguage(t *testing.T) {
	restore(t)
	if err := Register(Language{Code: "xx", Name: "Тест", Origin: "lang-xx",
		Entries: map[string]string{"Согласовать заявку": "xx-approve"}}); err != nil {
		t.Fatal(err)
	}
	if err := Use("xx"); err != nil {
		t.Fatal(err)
	}
	if T("Согласовать заявку") != "xx-approve" || T("Отклонить заявку") != "Reject a request" {
		t.Fatalf("%q, %q", T("Согласовать заявку"), T("Отклонить заявку"))
	}

	if err := Register(Language{Code: "eng", Origin: "чужой", Entries: map[string]string{"Отклонить заявку": "подмена"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := Lookup("en", "Отклонить заявку"); got != "Reject a request" {
		t.Fatalf("модуль переписал встроенный перевод: %q", got)
	}

	for _, bad := range []Language{{Code: ""}, {Code: "ru"}, {Code: "a/b"}} {
		if err := Register(bad); err == nil {
			t.Errorf("язык %q принят", bad.Code)
		}
	}
}

func TestErrorfKeepsWrapping(t *testing.T) {
	restore(t)
	if err := Use("en"); err != nil {
		t.Fatal(err)
	}
	base := errors.New("x")
	err := Errorf("роль %s не может выполнять %s: нужно право %s", "llm", "apply", fmtWrap{base})
	if !strings.HasPrefix(err.Error(), "role llm may not run apply") {
		t.Fatalf("%v", err)
	}
	wrapped := Errorf("внимание: история оболочки недоступна: %w", base)
	if !errors.Is(wrapped, base) {
		t.Fatal("%w потерян")
	}
}

type fmtWrap struct{ err error }

func (f fmtWrap) String() string { return f.err.Error() }
