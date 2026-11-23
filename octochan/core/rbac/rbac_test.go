package rbac

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func init() {
	// Тестам не нужна стойкость к перебору — нужна скорость.
	HashCost = Cost{Time: 1, Memory: 1024, Threads: 1}
}

func mustFail(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ожидалась ошибка с %q, получено: %v", want, err)
	}
}

func TestDefaultRoles(t *testing.T) {
	s := Default()
	if s.Current != Master || s.CurrentRole() == nil {
		t.Fatalf("после установки текущая роль %q, ожидалась master", s.Current)
	}

	master, llm := s.Find(Master), s.Find(LLM)
	for _, command := range []string{"apply", "role.create", "!!install", "какой-нибудь-модуль"} {
		if !master.Allows(command) {
			t.Errorf("master не может %s", command)
		}
	}
	for _, command := range []string{"diff", "fp.check", "plan", "request", "check", "role", "set.role"} {
		if !llm.Allows(command) {
			t.Errorf("llm не может %s", command)
		}
	}
	for _, command := range []string{"apply", "approve", "run", "eval", "patch", "rollback", "!!install", "auth", "role.create"} {
		if llm.Allows(command) {
			t.Errorf("llm может %s, а не должна", command)
		}
	}
}

func TestPermissions(t *testing.T) {
	s := Default()
	role, err := s.Create("slave", []string{"read", "plan", "approve"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !role.Allows("approve") || !role.Allows("reject") || role.Allows("apply") {
		t.Fatalf("права группы не раскрылись: %v", role.Perms)
	}

	// Право — и имя отдельной команды.
	if err := s.Grant("slave", []string{"patch"}, nil); err != nil {
		t.Fatal(err)
	}
	if !role.Allows("patch") || role.Allows("rollback") {
		t.Fatal("право на одну команду дало больше или меньше")
	}

	if err := s.Revoke("slave", []string{"approve"}); err != nil {
		t.Fatal(err)
	}
	if role.Allows("reject") {
		t.Fatal("право не забрано")
	}

	mustFail(t, s.Grant("slave", []string{"aply"}, nil), "неизвестное право \"aply\"")
	if err := s.Grant("slave", []string{"dwd"}, []string{"dwd"}); err != nil {
		t.Fatalf("команда модуля не выдаётся: %v", err)
	}
	mustFail(t, s.Revoke("slave", []string{"auth"}), "нет таких прав")
	mustFail(t, s.Grant(Master, []string{"read"}, nil), "у master все права")
	mustFail(t, s.Revoke(Master, []string{All}), "у master права не забираются")
	_, err = s.Create("empty", nil, nil)
	mustFail(t, err, "не указано ни одного права")
}

// Потолок llm не пробить даже master: нейросеть не выполняет и не
// согласовывает.
func TestLLMCeiling(t *testing.T) {
	s := Default()
	for _, perm := range []string{"apply", "approve", "admin", "install", "auth", "run", "patch", All} {
		mustFail(t, s.Grant(LLM, []string{perm}, nil), "выше потолка роли llm")
	}
	if err := s.Grant(LLM, []string{"diff"}, nil); err != nil {
		t.Fatalf("право внутри потолка не выдаётся: %v", err)
	}
}

func TestRoleNames(t *testing.T) {
	s := Default()
	for _, bad := range []string{"", "1role", "роль", "a b", strings.Repeat("a", 33), "-x"} {
		_, err := s.Create(bad, []string{"read"}, nil)
		mustFail(t, err, "имя роли")
	}
	if _, err := s.Create("Dan", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create("dan", []string{"read"}, nil)
	mustFail(t, err, "роль Dan уже есть")
	if s.Find("DAN") == nil {
		t.Fatal("поиск роли зависит от регистра")
	}
}

// back role замкнут: два возврата подряд приводят туда же, откуда начали.
func TestBackRoleIsARing(t *testing.T) {
	s := Default()
	for _, name := range []string{"slave", "Dan"} {
		if _, err := s.Create(name, []string{"read"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	back := func(target string) string {
		t.Helper()
		role, err := s.BackTarget(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Switch(role.Name); err != nil {
			t.Fatal(err)
		}
		return s.Current
	}

	_, err := s.BackTarget("")
	mustFail(t, err, "возвращаться некуда")

	_ = s.Switch("slave")
	steps := []string{back(""), back(""), back(""), back("")}
	if strings.Join(steps, ",") != "master,slave,master,slave" {
		t.Fatalf("back role ходит не по кольцу: %v", steps)
	}

	// С тремя ролями кольцо — между двумя последними.
	_ = s.Switch("Dan")
	if got := back(""); got != "slave" {
		t.Fatalf("из Dan назад — в slave, получено %s", got)
	}
	if got := back(""); got != "Dan" {
		t.Fatalf("и обратно в Dan, получено %s", got)
	}

	// К конкретной роли из истории — по имени, без учёта регистра.
	if got := back("MASTER"); got != Master {
		t.Fatalf("back role master привёл в %s", got)
	}
	_, err = s.BackTarget("master")
	mustFail(t, err, "уже текущая")

	if _, err := s.Create("never", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = s.BackTarget("never")
	mustFail(t, err, "нет в истории переключений")
	_, err = s.BackTarget("ghost")
	mustFail(t, err, "роли ghost нет")
}

func TestHistoryIsBounded(t *testing.T) {
	s := Default()
	if _, err := s.Create("a", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		_ = s.Switch([]string{"a", Master}[i%2])
	}
	if len(s.History) != historyLimit {
		t.Fatalf("история %d записей, предел %d", len(s.History), historyLimit)
	}
}

func TestCredentials(t *testing.T) {
	s := Default()
	if _, err := s.Create("slave", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	role := s.Find("slave")
	if role.NeedsAuth() || !role.Verify("", "") {
		t.Fatal("роль без логина и пароля требует вход")
	}

	login, password := "dan_k", "correct horse"
	if err := s.SetLogin("slave", &login); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword("slave", &password); err != nil {
		t.Fatal(err)
	}
	if !role.NeedsAuth() || !role.Verify(login, password) {
		t.Fatal("верные логин и пароль не приняты")
	}
	for _, c := range [][2]string{{login, "wrong"}, {"other", password}, {"", ""}} {
		if role.Verify(c[0], c[1]) {
			t.Fatalf("принят неверный вход %q/%q", c[0], c[1])
		}
	}

	// Открытым текстом ни логин, ни пароль не хранятся.
	data, err := s.encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{login, password} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("в файле ролей открытым текстом лежит %q", secret)
		}
	}

	// Снятие пароля оставляет только логин.
	if err := s.SetPassword("slave", nil); err != nil {
		t.Fatal(err)
	}
	if !role.Verify(login, "что угодно") || role.Verify("other", "") {
		t.Fatal("после снятия пароля логин проверяется неверно")
	}
	empty := ""
	mustFail(t, s.SetPassword("slave", &empty), "пустое значение")
}

// Новый пароль не подтверждён тем, кто сидит в истории: в роль с
// изменённым паролем back role без входа не пускает.
func TestCredentialChangeClearsHistory(t *testing.T) {
	s := Default()
	if _, err := s.Create("slave", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	_ = s.Switch("slave")
	_ = s.Switch(Master)

	password := "new"
	if err := s.SetPassword("slave", &password); err != nil {
		t.Fatal(err)
	}
	_, err := s.BackTarget("slave")
	mustFail(t, err, "нет в истории переключений")
	if s.History[len(s.History)-1] != Master {
		t.Fatalf("история должна кончаться текущей ролью: %v", s.History)
	}
}

func TestRenameAndDelete(t *testing.T) {
	s := Default()
	if _, err := s.Create("slave", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	_ = s.Switch("slave")
	if err := s.Rename("slave", "worker"); err != nil {
		t.Fatal(err)
	}
	if s.Current != "worker" || s.History[len(s.History)-1] != "worker" {
		t.Fatalf("текущая роль и история не последовали за переименованием: %s %v", s.Current, s.History)
	}

	mustFail(t, s.Delete("worker"), "текущая")
	mustFail(t, s.Delete(LLM), "встроенная")
	mustFail(t, s.Rename(Master, "boss"), "встроенная")
	mustFail(t, s.Rename("worker", "llm"), "роль llm уже есть")

	_ = s.Switch(Master)
	if err := s.Delete("worker"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range s.History {
		if entry == "worker" {
			t.Fatalf("удалённая роль осталась в истории: %v", s.History)
		}
	}
}

func TestStoreFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Current != Master {
		t.Fatal("без файла — роли по умолчанию")
	}

	password := "secret"
	if _, err := s.Create("slave", []string{"read", "plan"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword("slave", &password); err != nil {
		t.Fatal(err)
	}
	_ = s.Switch("slave")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("права файла ролей %v, ожидалось 0600", info.Mode().Perm())
		}
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Current != "slave" || !loaded.Find("slave").Verify("", password) ||
		strings.Join(loaded.History, ",") != "master,slave" {
		t.Fatalf("после чтения: текущая %s, история %v", loaded.Current, loaded.History)
	}

	// Испорченный файл — ошибка, а не тихий откат к master без паролей.
	data, _ := os.ReadFile(path)
	cases := map[string][]byte{
		"чужой файл":     []byte("max_connections = 100\n"),
		"оборванный":     data[:len(data)/2],
		"изменённый бит": flipByte(data, len(data)/2),
	}
	for name, content := range cases {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "повреждено") {
			t.Errorf("%s: ожидалась ошибка повреждения, получено %v", name, err)
		}
	}
}

func flipByte(data []byte, i int) []byte {
	out := append([]byte(nil), data...)
	out[i] ^= 0xff
	return out
}

func TestServerGrant(t *testing.T) {
	s := Default()
	now := time.Unix(1_800_000_000, 0)

	_, err := s.IssueGrant(Master, now.Add(time.Minute))
	mustFail(t, err, "нет ключа сервера")
	mustFail(t, s.CheckGrant(Master, "x", now), "сервер не запускался")

	if err := s.RotateServerKey(); err != nil {
		t.Fatal(err)
	}
	grant, err := s.IssueGrant(Master, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckGrant("MASTER", grant, now); err != nil {
		t.Fatalf("свежий допуск не принят: %v", err)
	}

	mustFail(t, s.CheckGrant(Master, grant, now.Add(2*time.Minute)), "срок допуска истёк")
	mustFail(t, s.CheckGrant(LLM, grant, now), "выдан на роль master")
	forged := strings.Replace(grant, "master", "llm", 1)
	mustFail(t, s.CheckGrant(LLM, forged, now), "подпись допуска не сходится")
	mustFail(t, s.CheckGrant(Master, "master|1|2|3", now), "испорчен")

	// После перезапуска сервера старые допуски недействительны.
	if err := s.RotateServerKey(); err != nil {
		t.Fatal(err)
	}
	mustFail(t, s.CheckGrant(Master, grant, now), "подпись допуска не сходится")
}

// Временные права и запреты действуют до срока и пропадают сами.
func TestTemporaryPerms(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	Now = func() time.Time { return now }
	defer func() { Now = time.Now }()

	s := Default()
	if _, err := s.Create("dan", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	dan := s.Find("dan")
	if err := s.GrantFor("dan", []string{"apply"}, now.Add(2*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if !dan.Allows("apply") || !dan.Allows("run") {
		t.Fatal("временное право не действует")
	}
	if err := s.DenyFor("dan", []string{"rollback"}, now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if dan.Allows("rollback") || !dan.Allows("run") {
		t.Fatal("временный запрет не действует или задел лишнее")
	}

	now = now.Add(90 * time.Minute)
	if !dan.Allows("rollback") {
		t.Fatal("запрет пережил свой срок")
	}
	now = now.Add(time.Hour)
	if dan.Allows("apply") {
		t.Fatal("право пережило свой срок")
	}
	s.Prune(now)
	if len(dan.TempPerms) != 0 || len(dan.TempDeny) != 0 {
		t.Fatalf("истёкшее не убрано: %+v %+v", dan.TempPerms, dan.TempDeny)
	}

	// Потолок llm не обходится и на время.
	if err := s.GrantFor(LLM, []string{"apply"}, now.Add(time.Hour), nil); err == nil {
		t.Fatal("llm получила apply на время")
	}
	for _, until := range []time.Time{now.Add(-time.Minute), now.Add(31 * 24 * time.Hour)} {
		if err := s.GrantFor("dan", []string{"apply"}, until, nil); err == nil {
			t.Fatalf("срок %s принят", until)
		}
	}
	// Revoke снимает и временное.
	if err := s.GrantFor("dan", []string{"plan"}, now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("dan", []string{"plan"}); err != nil || dan.Allows("plan") {
		t.Fatalf("revoke временного: %v", err)
	}
}

// Роль на время возвращается к исходной сама.
func TestTemporaryRole(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	Now = func() time.Time { return now }
	defer func() { Now = time.Now }()

	s := Default()
	for _, name := range []string{"dan", "ops"} {
		if _, err := s.Create(name, []string{"read"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SwitchFor("dan", now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Временная поверх временной возвращает к исходной, а не к dan.
	if err := s.SwitchFor("ops", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Expire(now.Add(59 * time.Minute)); ok || s.Current != "ops" {
		t.Fatal("роль истекла раньше срока")
	}
	from, to, ok := s.Expire(now.Add(time.Hour))
	if !ok || from != "ops" || to != Master || s.Current != Master || !s.CurrentUntil.IsZero() {
		t.Fatalf("возврат: %s -> %s, %v; текущая %s", from, to, ok, s.Current)
	}

	// Обычное переключение снимает срок.
	if err := s.SwitchFor("dan", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Switch("ops"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Expire(now.Add(3 * time.Hour)); ok {
		t.Fatal("срок пережил обычное переключение")
	}

	// Роль, к которой возвращаться, удалили — возврат к llm.
	if err := s.SwitchFor("dan", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.ReturnTo = "ops"
	if err := s.Delete("ops"); err != nil {
		t.Fatal(err)
	}
	if _, to, _ := s.Expire(now.Add(2 * time.Hour)); to != LLM {
		t.Fatalf("возврат к удалённой роли: %s", to)
	}

	// Сохраняется и читается.
	if err := s.SwitchFor("dan", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantFor("dan", []string{"apply"}, now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	data, err := s.encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.ReturnTo != LLM || !back.CurrentUntil.Equal(now.Add(time.Hour)) || len(back.Find("dan").TempPerms) != 1 {
		t.Fatalf("после чтения: %+v", back)
	}
}

// Вложенная команда со своей группой не открывается правом на команду
// верхнего уровня: чтение инвентаря не даёт отмечать новые серверы.
func TestNestedCommandWithOwnGroup(t *testing.T) {
	reader := &Role{Name: "r", Perms: []string{"read"}}
	if !reader.Allows("inventory") || reader.Allows("inventory.ack") {
		t.Fatal("read открывает inventory.ack или не открывает inventory")
	}
	if !(&Role{Name: "a", Perms: []string{"approve"}}).Allows("inventory.ack") {
		t.Fatal("approve не открывает inventory.ack")
	}
	if (&Role{Name: "i", Perms: []string{"inventory"}}).Allows("inventory.ack") {
		t.Fatal("право inventory открыло inventory.ack")
	}
	if !reader.Allows("fp.check") {
		t.Fatal("fp в read больше не открывает fp.check")
	}
}
