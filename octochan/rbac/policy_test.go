package rbac

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDenyOverridesPermissions(t *testing.T) {
	role := &Role{Name: "ops", Perms: []string{All}, Deny: []string{"admin", "serve"}}
	for command, want := range map[string]bool{
		"diff": true, "apply": true, "role.create": false, "role.grant": false,
		"serve": false, "tls.init": true, "role": true, "role.list": true, "set.role": true,
	} {
		if got := role.Allows(command); got != want {
			t.Errorf("%s: %v, ожидалось %v", command, got, want)
		}
	}
	// Запрет команды верхнего уровня закрывает и подкоманды, но не те,
	// без которых не узнать свою роль.
	role.Deny = []string{"role"}
	if role.Allows("role.create") || !role.Allows("role.show") || !role.Allows("role") {
		t.Error("запрет role должен закрыть role create, но не role и role show")
	}
}

func TestPolicyNotForMaster(t *testing.T) {
	s := Default()
	if err := s.DenyPerms(Master, []string{"serve"}, nil); err == nil {
		t.Error("master получил запрет")
	}
	if err := s.SetScope(Master, []string{"/srv"}); err == nil {
		t.Error("master получил область")
	}
	if err := s.SetLimits(Master, []Limit{{Perm: "apply", Count: 1, Window: time.Hour}}, nil); err == nil {
		t.Error("master получил предел")
	}
	if err := s.SetScope(LLM, []string{"srv"}); err == nil {
		t.Error("относительный каталог области принят")
	}
}

func TestParseLimit(t *testing.T) {
	for text, want := range map[string]string{
		"apply=10/1h": "apply=10/1h", "request=30/60m": "request=30/1h", "approve=5/2d": "approve=5/2d",
		"diff=3/90m": "diff=3/90m",
	} {
		limit, err := ParseLimit(text)
		if err != nil || limit.String() != want {
			t.Errorf("%s: %v %v, ожидалось %s", text, limit, err, want)
		}
	}
	for _, bad := range []string{"apply", "apply=0/1h", "apply=x/1h", "apply=1/30s", "apply=1/40d", "=1/1h", "apply=1/1y"} {
		if _, err := ParseLimit(bad); err == nil {
			t.Errorf("%s принят", bad)
		}
	}
}

func TestLimitsReplaceSamePermission(t *testing.T) {
	s := Default()
	if _, err := s.Create("bot", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	set := func(text string) {
		limit, err := ParseLimit(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetLimits("bot", []Limit{limit}, nil); err != nil {
			t.Fatal(err)
		}
	}
	set("diff=2/1h")
	set("read=100/1d")
	set("diff=5/1h")
	bot := s.Find("bot")
	if len(bot.Limits) != 2 || bot.Limits[0].Count != 5 {
		t.Fatalf("пределы: %v", bot.Limits)
	}
	if got := bot.LimitsFor("diff"); len(got) != 2 {
		t.Fatalf("на diff действуют оба предела: %v", got)
	}
	if bot.MaxWindow() != 24*time.Hour {
		t.Fatalf("самое длинное окно: %s", bot.MaxWindow())
	}
	if llm := Default().Find(LLM); len(llm.LimitsFor("request")) != 1 {
		t.Fatal("у llm по умолчанию нет предела заявок")
	}
}

func TestScope(t *testing.T) {
	dir := filepath.FromSlash("/srv/team")
	role := &Role{Name: "dan", Paths: []string{dir}}
	for path, want := range map[string]bool{
		"/srv/team": true, "/srv/team/a.conf": true, "/srv/team/x/../b": true,
		"/srv/teammate/a": false, "/srv": false, "/etc/passwd": false,
	} {
		if got := role.InScope(filepath.Clean(filepath.FromSlash(path))); got != want {
			t.Errorf("%s: %v, ожидалось %v", path, got, want)
		}
	}
	if !(&Role{Name: "x"}).InScope("/anything") {
		t.Error("роль без области ограничена")
	}
}

func TestPolicySurvivesSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := Default()
	if _, err := s.Create("dan", []string{"read", "apply"}, nil); err != nil {
		t.Fatal(err)
	}
	// Каталог берётся из TempDir: «/srv/team» абсолютен только в Unix, на
	// Windows SetScope его отвергал, а ошибка молча терялась в «_ =».
	team := filepath.Join(t.TempDir(), "team")
	for _, err := range []error{
		s.DenyPerms("dan", []string{"rollback"}, nil),
		s.SetScope("dan", []string{team}),
		s.SetLimits("dan", []Limit{{Perm: "apply", Count: 3, Window: time.Hour}}, nil),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dan := loaded.Find("dan")
	if len(dan.Deny) != 1 || len(dan.Paths) != 1 || len(dan.Limits) != 1 || dan.Limits[0].Window != time.Hour {
		t.Fatalf("политика не сохранилась: %+v", dan)
	}
}
