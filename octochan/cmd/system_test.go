package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/rbac"
)

// systemPolicyFile ставит системную политику ролей для теста: путь
// задаётся при сборке, проверка владельца подменяется — в каталоге
// теста файл не принадлежит root.
func (e *scriptEnv) systemPolicyFile(text string, check error) {
	e.t.Helper()
	path := filepath.Join(e.t.TempDir(), "roles.yaml")
	e.write(path, text)
	oldPath, oldCheck := rbac.SystemPolicyPath, rbac.SystemCheck
	rbac.SystemPolicyPath = path
	rbac.SystemCheck = func(string) error { return check }
	e.t.Cleanup(func() { rbac.SystemPolicyPath, rbac.SystemCheck = oldPath, oldCheck })
}

// Роль подтверждает не рабочий каталог пользователя, а файл, которым
// владеет root: хранилище ролей может права только сузить.
func TestSystemRolePolicy(t *testing.T) {
	e := newScriptEnv(t)
	me := rbac.CurrentIdentity().User
	if me == "" {
		t.Skip("пользователя ОС не найти по uid")
	}
	e.must("role") // хранилище ролей по умолчанию: master текущая
	e.systemPolicyFile("users:\n  "+me+": [ops, llm]\nroles:\n  ops:\n    allow: [read, plan, request]\n"+
		"  llm:\n    allow: [read, plan, request, apply]\n", nil)

	// master в хранилище не положена — работает первая положенная роль.
	out := e.must("role")
	if !strings.Contains(out, "роль master пользователю "+me) || !strings.Contains(out, "Текущая роль: ops") {
		t.Fatalf("role:\n%s", out)
	}
	proof := e.must("role", "proof")
	for _, want := range []string{"uid сообщает ядро", "Положено:           ops, llm",
		"Потолок:            read, plan, request (системная политика)", "Доказано:", "Не доказано:"} {
		if !strings.Contains(proof, want) {
			t.Fatalf("role proof: нет %q:\n%s", want, proof)
		}
	}
	e.fails("роль ops не может выполнять approve", "approve", "0000")
	e.fails("не положена системной политикой", "set", "role", "master")

	// Хранилище ролей — в руках пользователя: дописал ops all — потолок
	// остался.
	store, path, err := loadRoles()
	if err != nil {
		t.Fatal(err)
	}
	store.Find("ops").Perms = []string{rbac.All}
	if err := store.Save(path); err != nil {
		t.Fatal(err)
	}
	e.fails("роль ops не может выполнять approve", "approve", "0000")
	if out := e.must("role", "show"); !strings.Contains(out, "Потолок:   read, plan, request") {
		t.Fatalf("role show:\n%s", out)
	}

	// Потолок llm не выше встроенного, что бы ни написал файл.
	e.must("set", "role", "llm")
	e.fails("роль llm не может выполнять apply", "apply", "--request", "0000")

	// Окружение роль не поднимает.
	t.Setenv(EnvRole, rbac.Master)
	e.fails("не положена системной политикой", "role")
	os.Unsetenv(EnvRole)

	// Файлу, которому нельзя верить, ochan не верит — и не работает.
	e.systemPolicyFile("users:\n  "+me+": [master]\n", errors.New("в roles.yaml может писать не только root"))
	e.fails("верить нельзя: в roles.yaml может писать не только root", "role")
}
