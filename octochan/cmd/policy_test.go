package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Запрет перекрывает право, в том числе all; у master запретов не бывает.
func TestRoleDenyOverridesAll(t *testing.T) {
	e := newScriptEnv(t)
	e.must("role", "create", "ops", "--allow", "all", "--deny", "admin,serve")
	e.fails("master — владелец", "role", "deny", "master", "serve")
	e.fails("неизвестное право", "role", "deny", "ops", "servve")

	t.Setenv(EnvRole, "ops")
	e.must("diff", "prod.conf", "prod.conf")
	e.must("plan", "tune.ochan")
	e.fails("команда role create роли ops запрещена", "role", "create", "x", "--allow", "read")
	e.fails("команда serve роли ops запрещена", "serve")
	// Без этих команд не узнать свою роль: их не запретить.
	if out := e.must("role", "show"); !strings.Contains(out, "Запреты:   admin, serve") {
		t.Fatalf("role show:\n%s", out)
	}

	t.Setenv(EnvRole, "")
	e.must("role", "undeny", "ops", "admin")
	t.Setenv(EnvRole, "ops")
	e.must("role", "create", "x", "--allow", "read")
}

// Область: текущий каталог, пути в аргументах, флагах и сценариях —
// только внутри каталогов роли.
func TestRoleScope(t *testing.T) {
	e := newScriptEnv(t)
	inside := filepath.Join(e.work, "team")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(inside, "a.conf"), "max_connections = 1\n")
	e.write(filepath.Join(inside, "peek.ochan"), "print(read(\"../prod.conf\"))\n")
	e.write(filepath.Join(inside, "ok.ochan"), "c = config(\"a.conf\")\nput(c, \"max_connections\", \"2\")\nsave(c, \"a.conf\")\n")
	if err := os.Symlink(e.path("prod.conf"), filepath.Join(inside, "link.conf")); err != nil {
		t.Fatal(err)
	}

	e.must("role", "create", "dan", "--allow", "read,plan,request,apply")
	e.must("role", "scope", "dan", inside)
	e.fails("master — владелец", "role", "scope", "master", inside)

	t.Setenv(EnvRole, "dan")
	e.fails("вне области роли dan", "diff", "prod.conf", "prod.conf")

	if err := os.Chdir(inside); err != nil {
		t.Fatal(err)
	}
	e.must("diff", "a.conf", "a.conf")
	e.fails("../prod.conf вне области роли dan", "diff", "a.conf", "../prod.conf")
	e.fails("вне области роли dan", "diff", "a.conf", e.path("prod.conf"))
	e.fails("link.conf вне области роли dan", "diff", "a.conf", "link.conf")
	e.fails("вне области роли dan", "plan", "--sandbox", "--root", e.work, "ok.ochan")
	e.fails("используйте run --sandbox", "eval", "1 + 1")

	// Сценарий роли с областью — всегда в песочнице, даже без флага.
	e.fails("вне разрешённых каталогов", "run", "peek.ochan")
	e.must("run", "ok.ochan")
	if data, _ := os.ReadFile(filepath.Join(inside, "a.conf")); !strings.Contains(string(data), "max_connections = 2") {
		t.Fatalf("сценарий в области не выполнился: %s", data)
	}

	t.Setenv(EnvRole, "")
	e.must("role", "scope", "dan", "--clear")
	t.Setenv(EnvRole, "dan")
	e.must("diff", "a.conf", "../prod.conf")
}

// Рабочий каталог ochan закрыт для всех ролей, кроме master, а для
// сценариев — для всех.
func TestOchanDirectoryIsProtected(t *testing.T) {
	e := newScriptEnv(t)
	roles := filepath.Join(e.home, ".octochan", "roles.bin")
	e.must("role", "create", "dan", "--allow", "read,apply")
	e.write(e.path("steal.ochan"), "print(read(\""+filepath.ToSlash(roles)+"\"))\n")
	e.write(e.path("overwrite.ochan"), "write(\""+filepath.ToSlash(roles)+"\", \"x\")\n")
	secret := filepath.Join(e.home, ".octochan", "secret.conf")
	e.write(secret, "token = x\n")

	e.fails("рабочий каталог ochan", "run", "steal.ochan")
	// master может накрыть песочницей домашний каталог, но язык всё равно
	// не пустит сценарий в рабочий каталог ochan.
	e.fails("рабочий каталог ochan", "run", "--sandbox", "--root", e.home, "overwrite.ochan")
	e.must("print", secret)

	t.Setenv(EnvRole, "dan")
	e.fails("рабочий каталог ochan: роли dan он закрыт", "print", secret)
	// Конфиг, заданный окружением, закрыт так же, хоть и лежит снаружи.
	e.fails("рабочий каталог ochan: роли dan он закрыт", "print", os.Getenv("OCTOCHAN_CONFIG"))
	e.fails("рабочий каталог ochan", "run", "steal.ochan")
	e.fails("содержит рабочий каталог ochan", "run", "--sandbox", "--root", e.home, "steal.ochan")
	if data, err := os.ReadFile(roles); err != nil || string(data) == "x" {
		t.Fatalf("хранилище ролей тронуто: %v", err)
	}
}

// Предел частоты считается по журналу аудита; отказ тоже записан.
func TestRoleLimits(t *testing.T) {
	e := newScriptEnv(t)
	e.must("role", "create", "bot", "--allow", "read")
	e.fails("право=число/окно", "role", "limit", "bot", "diff=2")
	e.fails("окно от 1m до 31d", "role", "limit", "bot", "diff=2/10s")
	e.must("role", "limit", "bot", "diff=2/1h")

	t.Setenv(EnvRole, "bot")
	e.must("diff", "prod.conf", "prod.conf")
	e.must("validate", "prod.conf") // не под пределом
	e.must("diff", "prod.conf", "prod.conf")
	e.fails("предел роли bot исчерпан: diff=2/1h", "diff", "prod.conf", "prod.conf")

	t.Setenv(EnvRole, "")
	out := e.must("audit", "--role", "bot", "--denied")
	if !strings.Contains(out, "предел роли bot исчерпан") {
		t.Fatalf("отказ по пределу не в журнале:\n%s", out)
	}
	if out := e.must("role", "show", "llm"); !strings.Contains(out, "request=30/1h") {
		t.Fatalf("у llm нет предела заявок по умолчанию:\n%s", out)
	}
}

// Свою заявку роль не согласует; заявку нейросети согласуют только с
// подтверждением.
func TestApprovalSeparationOfDuties(t *testing.T) {
	e := newScriptEnv(t)
	e.must("role", "create", "dan", "--allow", "read,plan,request,approve")
	e.must("role", "create", "eve", "--allow", "read,approve")

	t.Setenv(EnvRole, "dan")
	id := requestIDPattern.FindStringSubmatch(e.must("request", "tune.ochan"))[1]
	e.fails("свою заявку не согласуют", "approve", id)
	if card := e.must("requests", id); !strings.Contains(card, "роль dan") {
		t.Fatalf("роль автора не в заявке:\n%s", card)
	}
	t.Setenv(EnvRole, "eve")
	if out := e.must("approve", id); !strings.Contains(out, "роль eve") {
		t.Fatalf("решение без роли:\n%s", out)
	}

	t.Setenv(EnvRole, "llm")
	llmID := requestIDPattern.FindStringSubmatch(e.must("request", "tune.ochan"))[1]
	t.Setenv(EnvRole, "")
	typed(t, "")
	e.fails("повторите с --yes", "approve", llmID)

	oldTTY := roleInputIsTTY
	roleInputIsTTY = func() bool { return true }
	t.Cleanup(func() { roleInputIsTTY = oldTTY })
	roleInput = strings.NewReader("не тот\n")
	e.fails("номер не совпал", "approve", llmID)
	roleInput = strings.NewReader(llmID + "\n")
	if out := e.must("approve", llmID); !strings.Contains(out, "Заявку создала нейросеть") {
		t.Fatalf("согласование заявки нейросети без плана:\n%s", out)
	}

	roleInputIsTTY = func() bool { return false }
	t.Setenv(EnvRole, "llm")
	other := requestIDPattern.FindStringSubmatch(e.must("request", "tune.ochan"))[1]
	t.Setenv(EnvRole, "")
	e.must("approve", "--yes", other)
}

// Журнал аудита: решения записаны, цепочка проверяется, правка видна.
func TestAuditJournal(t *testing.T) {
	e := newScriptEnv(t)
	e.must("diff", "prod.conf", "prod.conf")
	t.Setenv(EnvRole, "llm")
	e.fails("не может выполнять run", "run", "tune.ochan")
	t.Setenv(EnvRole, "ghost")
	e.fails("такой роли нет", "diff", "a", "b")
	t.Setenv(EnvRole, "")

	out := e.must("audit", "-n", "0")
	for _, want := range []string{"master", "diff prod.conf prod.conf", "- ", "llm", "ghost"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в журнале нет %q:\n%s", want, out)
		}
	}
	verified := e.must("audit", "verify")
	if !strings.Contains(verified, "Журнал аудита цел") {
		t.Fatalf("verify:\n%s", verified)
	}

	path := filepath.Join(e.home, ".octochan", "audit.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"role":"llm"`, `"role":"master"`, 1)
	if tampered == string(data) {
		t.Fatal("в журнале нет записи llm")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	e.fails("не прошёл проверку", "audit", "verify")
}
