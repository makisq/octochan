package lang

import (
	"strings"
	"testing"
)

// TestWhileAndStrings — цикл while, символ строки по индексу, substr,
// chars, bytes и from_bytes.
func TestWhileAndStrings(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"while", `
var i = 0
var acc = []
while i < 5 {
    i = i + 1
    if i == 2 { continue }
    if i == 4 { break }
    acc = acc + [i]
}
print(join(acc, ""))
`, "13"},
		{"index", `
s = "привет"
print(s[0] + s[-1] + s[2])
`, "пти"},
		{"reverse", `
s = "abc"
var i = len(s) - 1
var r = ""
while i >= 0 {
    r = r + s[i]
    i = i - 1
}
print(r)
`, "cba"},
		{"substr", `print(substr("привет, мир", 0, 6) + "|" + substr("привет, мир", 8) + "|" + substr("abcdef", -3, -1))`, "привет|мир|de"},
		{"chars", `print(join(chars("да"), "-"))`, "д-а"},
		{"bytes", `
b = bytes("я!")
print(join([len(b), b[0], b[2], from_bytes(b)], " "))
`, "3 209 33 я!"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := run(t, c.src)
			if err != nil {
				t.Fatalf("ошибка: %v", err)
			}
			if got := strings.TrimSpace(out); got != c.want {
				t.Fatalf("получено %q, ожидалось %q", got, c.want)
			}
		})
	}

	errs := []struct {
		name, src, want string
	}{
		{"index out of range", `x = "ab"[5]`, "вне строки длиной 2"},
		{"substr reversed", `x = substr("abc", 2, 1)`, "дальше конца"},
		{"substr out of range", `x = substr("abc", 0, 9)`, "вне строки длиной 3"},
		{"from_bytes", `x = from_bytes([1, 300])`, "не байт"},
	}
	for _, c := range errs {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ошибка %v, ожидалось «%s»", err, c.want)
			}
		})
	}
}

// Проверка без выполнения понимает while и индекс строки.
func TestCheckWhileAndStringIndex(t *testing.T) {
	src := "var i = 0\nwhile i < 3 {\n    i = i + 1\n    if i == 2 { break }\n}\ns = \"abc\"\nc = s[1]\nx = upper(c)\n"
	if problems := check(t, src); len(problems) > 0 {
		t.Fatalf("лишние замечания:\n%s", joinProblems(problems))
	}
	if problems := check(t, "x = 5[0]\n"); len(problems) == 0 {
		t.Fatal("индекс числа прошёл проверку")
	}
}
