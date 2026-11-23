package lang

import (
	"strings"
	"testing"
)

// В сценарии своей модели нет комментариев, а в строке — только то, что
// можно проверить: print("перезапусти, пожалуйста") не пройдёт.
func TestModelText(t *testing.T) {
	known := func(s string) bool { return s == "prod.conf" || s == "max_connections" || s == "8GB" }
	ok := `c = config("prod.conf")
put(c, "max_connections", "8GB")
save(c, 'prod.conf')
`
	if errs := ModelText("s.ochan", ok, known); len(errs) > 0 {
		t.Fatalf("допустимый сценарий: %v", errs)
	}
	bad := `c = config("prod.conf")  # поднимаю лимит
print("please restart the server")
`
	errs := ModelText("s.ochan", bad, known)
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "s.ochan:1:26: комментарий") ||
		!strings.Contains(errs[1].Error(), `s.ochan:2:7: строка "please restart the server"`) {
		t.Fatalf("находки: %v", errs)
	}
}
