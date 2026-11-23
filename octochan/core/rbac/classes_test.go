package rbac

import (
	"strings"
	"testing"
)

// Команда модуля с классом read открывается правом read — и у llm, — а
// исполнительная и команда без класса — нет. Потолок llm прежний.
func TestModuleClasses(t *testing.T) {
	t.Cleanup(func() { SetClasses(nil) })
	skipped := SetClasses(map[string]string{
		"pgstat":         "read",
		"pgstat.restart": "execution",
		"pgx.vacuum":     "apply",
		"pgx":            "Read",
		"diff":           "apply", // команда ядра: класс не меняет
		"odd":            "chaos", // неизвестный класс
	})
	if len(skipped) != 2 || !strings.Contains(strings.Join(skipped, "\n"), "diff: команда ядра") ||
		!strings.Contains(strings.Join(skipped, "\n"), "odd: неизвестный класс") {
		t.Fatalf("отклонённые классы: %v", skipped)
	}

	llm := Default().Find(LLM)
	cases := []struct {
		command string
		allowed bool
	}{
		{"pgstat", true},
		{"pgstat.status", true},   // подкоманда без своего класса — класс pgstat
		{"pgstat.restart", false}, // свой класс — apply
		{"pgx", true},
		{"pgx.vacuum", false},
		{"other", false}, // команда модуля без класса — как раньше
		{"diff", true},
		{"apply", false},
		{"!!install", false},
	}
	for _, c := range cases {
		if got := llm.Allows(c.command); got != c.allowed {
			t.Errorf("llm: %s — %v, ожидалось %v", c.command, got, c.allowed)
		}
	}

	// Право apply открывает исполнительные команды модулей, запрет read
	// закрывает читающие.
	ops := &Role{Name: "ops", Perms: []string{"apply"}, Deny: []string{"read"}}
	if !ops.Allows("pgstat.restart") || ops.Allows("pgstat") || ops.Allows("pgstat.status") {
		t.Fatalf("ops: restart %v, pgstat %v, status %v", ops.Allows("pgstat.restart"), ops.Allows("pgstat"), ops.Allows("pgstat.status"))
	}

	// Выдать llm команду модуля по имени можно, только если её класс — под
	// потолком.
	s := Default()
	extra := []string{"pgstat", "pgstat.restart", "other"}
	if err := s.Grant(LLM, []string{"pgstat"}, extra); err != nil {
		t.Fatalf("read-команда модуля не выдаётся llm: %v", err)
	}
	mustFail(t, s.Grant(LLM, []string{"pgstat.restart"}, extra), "выше потолка роли llm")
	mustFail(t, s.Grant(LLM, []string{"other"}, extra), "выше потолка роли llm")

	// Перезагрузка модуля без аннотации убирает класс.
	SetClasses(map[string]string{"pgx": "read"})
	if llm.Allows("pgstat") {
		t.Fatal("класс выгруженного модуля остался")
	}
}
