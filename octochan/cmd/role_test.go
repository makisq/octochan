package cmd

import (
	"slices"
	"strings"
	"testing"
	"time"

	"octochan/rbac"

	"github.com/spf13/cobra"
)

func init() {
	// Тестам не нужна стойкость к перебору — нужна скорость.
	rbac.HashCost = rbac.Cost{Time: 1, Memory: 1024, Threads: 1}
}

// typed подставляет то, что пользователь ввёл бы в ответ на вопросы
// о логине и пароле.
func typed(t *testing.T, input string) {
	t.Helper()
	oldInput, oldTTY := roleInput, roleInputIsTTY
	roleInput = strings.NewReader(input)
	roleInputIsTTY = func() bool { return false }
	t.Cleanup(func() { roleInput, roleInputIsTTY = oldInput, oldTTY })
}

// Каждая команда ochan отнесена к группе прав или доступна всем. Новая
// команда без такого решения была бы доступна только master — и об этом
// никто бы не узнал.
func TestEveryCommandHasPermission(t *testing.T) {
	free := map[string]bool{}
	for _, name := range rbac.Free {
		free[name] = true
	}
	known := map[string]bool{}
	for _, name := range rbac.KnownCommands() {
		known[name] = true
	}

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden {
				continue
			}
			key := commandKey(sub)
			top := strings.SplitN(key, ".", 2)[0]
			if sub.Runnable() && !free[key] && !known[key] && !known[top] {
				t.Errorf("команда %q не отнесена ни к одной группе прав", strings.ReplaceAll(key, ".", " "))
			}
			walk(sub)
		}
	}
	walk(rootCmd)
}

func TestRolesLifecycle(t *testing.T) {
	e := newScriptEnv(t)

	if out := e.must("role"); !strings.Contains(out, "Текущая роль: master") {
		t.Fatalf("после установки — master:\n%s", out)
	}
	list := e.must("role", "list")
	for _, want := range []string{"* master", "all", "llm", "read, plan, request", "встроенная"} {
		if !strings.Contains(list, want) {
			t.Fatalf("в списке ролей нет %q:\n%s", want, list)
		}
	}

	e.must("role", "create", "slave", "--allow", "read,plan")
	e.fails("неизвестное право \"aply\"", "role", "create", "other", "--allow", "aply")

	// Под slave выполнение запрещено, план разрешён.
	e.must("set", "role", "slave")
	e.fails("роль slave не может выполнять apply: нужно право apply", "apply", "--request", "x")
	e.fails("роль slave не может выполнять role create: нужно право admin", "role", "create", "x", "--allow", "read")
	if out := e.must("plan", "tune.ochan"); !strings.Contains(out, "изменить   max_connections") {
		t.Fatalf("план под slave:\n%s", out)
	}

	// back role замкнут: туда и обратно по истории.
	for _, want := range []string{"Роль: master", "Роль: slave", "Роль: master"} {
		if out := e.must("back", "role"); !strings.Contains(out, want) {
			t.Fatalf("ожидалось %q, получено:\n%s", want, out)
		}
	}
	e.must("back", "role", "slave")
	e.must("back", "role", "master")
	e.fails("уже текущая", "back", "role", "master")
	if out := e.must("role"); !strings.Contains(out, "master -> slave -> master -> slave -> master -> slave -> master") {
		t.Fatalf("история:\n%s", out)
	}
}

func TestRolePasswords(t *testing.T) {
	e := newScriptEnv(t)

	typed(t, "s3cret\n")
	e.must("role", "create", "Dan", "--allow", "read", "--nlogin", "dan_k", "--npsw")
	if list := e.must("role", "list"); !strings.Contains(list, "логин, пароль") {
		t.Fatalf("у роли нет логина и пароля:\n%s", list)
	}

	typed(t, "dan_k\nwrong\n")
	e.fails("неверный логин или пароль для роли Dan", "set", "role", "Dan")
	typed(t, "other\ns3cret\n")
	e.fails("неверный логин или пароль", "set", "role", "Dan")

	typed(t, "dan_k\ns3cret\n")
	e.must("set", "role", "dan")
	// Назад в master и снова в Dan — без пароля: в роль уже входили.
	e.must("back", "role")
	e.must("back", "role", "Dan")
	e.must("back", "role", "master")

	// Смена пароля выводит роль из истории: back role без входа не пустит.
	typed(t, "n3w\n")
	e.must("role", "update", "Dan", "--npsw")
	e.fails("нет в истории переключений", "back", "role", "Dan")
	typed(t, "dan_k\nn3w\n")
	e.must("set", "role", "Dan")
	e.must("back", "role")

	e.must("role", "update", "Dan", "--drop-login", "--drop-psw", "--rename", "worker")
	e.must("set", "role", "worker")
	e.must("back", "role")

	e.fails("непонятно, задать пароль или снять", "role", "update", "worker", "--npsw", "--drop-psw")
	e.fails("укажите, что менять", "role", "update", "worker")
	e.fails("встроенная", "role", "delete", "llm")
	e.must("role", "delete", "worker")
}

// ochan-mcp запускает ochan с OCTOCHAN_ROLE=llm: нейросеть работает
// в своей роли, что бы ни было текущей ролью пользователя.
func TestRoleFromEnvironment(t *testing.T) {
	e := newScriptEnv(t)
	t.Setenv(EnvRole, "llm")

	if out := e.must("role"); !strings.Contains(out, "Текущая роль: llm") || !strings.Contains(out, "переключение недоступно") {
		t.Fatalf("роль из окружения:\n%s", out)
	}
	e.must("plan", "tune.ochan")
	e.must("check", "tune.ochan")
	e.fails("роль llm не может выполнять run: нужно право apply", "run", "tune.ochan")
	e.fails("роль llm не может выполнять approve", "approve", "x")
	e.fails("переключение недоступно", "set", "role", "master")
	e.fails("переключение недоступно", "back", "role")

	// Роль с паролем окружением не задать: иначе OCTOCHAN_ROLE=master
	// обходил бы пароль master.
	t.Setenv(EnvRole, "")
	typed(t, "pw\n")
	e.must("role", "update", "master", "--npsw")
	t.Setenv(EnvRole, "master")
	e.fails("роль защищена логином или паролем", "diff", "a", "b")

	t.Setenv(EnvRole, "ghost")
	e.fails("OCTOCHAN_ROLE=ghost: такой роли нет", "diff", "a", "b")
}

// Потолок llm не пробить и через командную строку.
func TestLLMCeilingFromCommandLine(t *testing.T) {
	e := newScriptEnv(t)
	e.fails("выше потолка роли llm", "role", "grant", "llm", "apply")
	e.must("role", "grant", "llm", "diff")
	e.must("role", "revoke", "llm", "request")
	if list := e.must("role", "list"); !strings.Contains(list, "read, plan, diff") {
		t.Fatalf("права llm:\n%s", list)
	}
}

// Право на команду модуля выдаётся по имени, даже если у неё есть
// подкоманды; свои команды с подкомандами по имени не выдаются.
func TestModuleCommandWithSubcommandsIsGrantable(t *testing.T) {
	module := &cobra.Command{Use: "test", Annotations: map[string]string{moduleCommandAnnotation: "module"}}
	module.AddCommand(&cobra.Command{Use: "generate"})
	rootCmd.AddCommand(module)
	t.Cleanup(func() { rootCmd.RemoveCommand(module) })

	extra := moduleCommands()
	if !slices.Contains(extra, "test") {
		t.Fatalf("команда модуля не выдаётся по имени: %v", extra)
	}
	for _, own := range []string{"set", "back", "tls"} {
		if slices.Contains(extra, own) {
			t.Fatalf("своя команда %s выдаётся по имени: %v", own, extra)
		}
	}
}

// Временная роль возвращается сама, временное право пропадает само, и
// оба события видны в журнале аудита.
func TestTemporaryRolesAndPerms(t *testing.T) {
	e := newScriptEnv(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)
	rbac.Now = func() time.Time { return now }
	t.Cleanup(func() { rbac.Now = time.Now })

	e.must("role", "create", "dan", "--allow", "read,plan")
	if out := e.must("role", "grant", "dan", "apply", "--for", "2h"); !strings.Contains(out, "Роль dan: apply до 2026-10-23 14:00 (осталось 2h)") {
		t.Fatalf("выдача на время:\n%s", out)
	}
	e.must("role", "deny", "dan", "rollback", "--until", "12:30")
	if out := e.must("role", "show", "dan"); !strings.Contains(out, "На время:  apply до 2026-10-23 14:00") ||
		!strings.Contains(out, "Запреты на время: rollback до 2026-10-23 12:30 (осталось 30m)") {
		t.Fatalf("карточка роли:\n%s", out)
	}

	if out := e.must("set", "role", "dan", "--for", "1h"); !strings.Contains(out, "Роль: dan до 2026-10-23 13:00 (осталось 1h), затем master") {
		t.Fatalf("роль на время:\n%s", out)
	}
	e.fails("команда rollback роли dan запрещена", "rollback", "x")
	if out := e.must("role"); !strings.Contains(out, "До:           2026-10-23 13:00 (осталось 1h), затем master") {
		t.Fatalf("срок роли:\n%s", out)
	}

	// Через полчаса запрет кончился, право ещё действует.
	now = now.Add(45 * time.Minute)
	e.fails(`запись "1" не найдена`, "rollback", "1")

	// Через час роль вернулась сама.
	now = now.Add(20 * time.Minute)
	out := e.must("role")
	if !strings.Contains(out, "Срок роли dan истёк: текущая роль master") || !strings.Contains(out, "Текущая роль: master") {
		t.Fatalf("возврат роли:\n%s", out)
	}

	// Через два часа и право пропало.
	now = now.Add(time.Hour)
	e.must("set", "role", "dan")
	e.fails("роль dan не может выполнять apply: нужно право apply", "apply", "--request", "x")
	e.must("back", "role")

	journal := e.must("audit")
	for _, want := range []string{"role grant --for=2h dan apply", "role: set role --for=1h dan", "role: role expire master",
		"role: set role dan\n"} {
		if !strings.Contains(journal, want) {
			t.Fatalf("в журнале нет %q:\n%s", want, journal)
		}
	}

	e.fails("выше потолка роли llm", "role", "grant", "llm", "apply", "--for", "1h")
	e.fails("укажите --for или --until, не оба", "role", "grant", "dan", "apply", "--for", "1h", "--until", "13:00")
	e.fails("уже прошёл", "role", "grant", "dan", "apply", "--until", "2001-01-01 00:00")
}
