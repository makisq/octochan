package lang

import (
	"strings"
	"testing"
)

// Трассировка печатает инструкции с текстом строки, итоги, вызовы алиасов
// и при ошибке — переменные один раз; секреты скрыты.
func TestTrace(t *testing.T) {
	src := `alias check(conf, password) {
    var limit = 100
    if conf > limit {
        fail("слишком много")
    }
    return conf
}
var api_token = "s3cr3t"
print(check(50, "pw"))
var n = check(150, "pw")
`
	var trace strings.Builder
	out, err := runWith(t, src, func(in *Interpreter) {
		in.SetTrace(&trace, map[string]string{"<тест>": src})
	})
	if err == nil || out != "50\n" {
		t.Fatalf("выполнение: %q, %v", out, err)
	}
	got := trace.String()
	for _, want := range []string{
		"trace <тест>:8:1: var api_token = …\n  → api_token = …\n",
		"→ вызов check(50, …)\n",
		"  trace <тест>:2:5: var limit = 100\n    → limit = 100\n",
		"    → return 50\n",
		"    ✗ <тест>:4:13: слишком много",
		"      переменные: conf = 150, limit = 100, password = …\n",
		"      вызовы: check\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("в трассировке нет %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "✗") != 1 || strings.Contains(got, "s3cr3t") || strings.Contains(got, `вызов check(50, "pw")`) {
		t.Fatalf("ошибка дважды или секрет в трассировке:\n%s", got)
	}
	if strings.Contains(got, "→ \"50\"") {
		t.Fatalf("print повторён в трассировке:\n%s", got)
	}
}

func TestSecretName(t *testing.T) {
	for name, want := range map[string]bool{"api_token": true, "password": true, "API_KEY": true, "session-cookie": true,
		"token_lifetime": false, "keep": false, "bypass": false, "limit": false} {
		if SecretName(name) != want {
			t.Errorf("%s: %v", name, !want)
		}
	}
}
