package fp

import (
	"strings"
	"testing"
)

// Текст модели на языке, дописываемый по строке: каждая строка проходит,
// пока продолжение может закончить текст; приветствие, кириллица и
// комментарий — нет.
func TestStatementSyntax(t *testing.T) {
	doc := "goal: count\n\nclasses:\n  Count : \"count\"\n  Period : \"period\"\n\ncell incidents:\n" +
		"  set Count.total = 3\n  set Period.length = 7 d\n"
	prefix := ""
	for _, line := range strings.SplitAfter(doc, "\n") {
		if line == "" {
			continue
		}
		if bad := StatementSyntax(prefix, line, Options{}); len(bad) > 0 {
			t.Fatalf("строка %q после\n%s\nотвергнута: %v", line, prefix, bad)
		}
		prefix += line
	}
	if r := CheckModelText("m", doc, Options{}); !r.InLanguage() {
		t.Fatalf("готовый текст: %v", r.Diagnostics)
	}

	for _, c := range []struct{ prefix, line, want string }{
		{"", "Sure, here is the report\n", "такой последовательности нет"},
		{"goal: count\n", "please restart the server\n", "такой последовательности нет"},
		{"goal: count\n\nclasses:\n", "  Count : \"hello\"\n", "нет в словаре"},
		{"goal: count\n\nclasses:\n  Count : \"count\"\n\ncell c:\n", "  set Count.note = привет\n", "вне ASCII"},
		{"goal: count\n\nclasses:\n  Count : \"count\"\n\ncell c:\n", "  set Count.total = 3  # всё хорошо\n", "комментарий"},
		{"goal: count\n\nclasses:\n  Count : \"count\"\n\ncell c:\n", "  set Count.total = hello world\n", "лишнее"},
		// Пустой раздел, за которым начат другой, — уже не «недописан».
		{"goal: count\n\nclasses:\n", "cell c:\n", "раздел classes: пуст"},
	} {
		bad := StatementSyntax(c.prefix, c.line, Options{})
		if len(bad) == 0 || !strings.Contains(bad[0].Msg, c.want) {
			t.Errorf("%q: %v, ожидалось «%s»", c.line, bad, c.want)
		}
	}
	// Цель без класса — снимаемо, пока classes: не написан; в готовом
	// тексте — отказ.
	if r := CheckModelText("m", "goal: count\n", Options{}); r.InLanguage() {
		t.Fatal("цель без класса в готовом тексте прошла")
	}
}
