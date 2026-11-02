package fp

import (
	"strings"
	"testing"
)

// Имя объекта в тексте модели — из задачи, из словаря объектов ochan или
// из слов языка; новое — только ячейка cK. please_restart не пройдёт.
func TestForeignNames(t *testing.T) {
	task := "goal: tuned\n\nclasses:\n  Config : \"config\"\n  State  : \"tuned\"\n\n" +
		"relations:\n  Config --strives_to--> State\n\ncell c1:\n  set Config.work_mem = 4 MB\n  set prod_conf.owner = db_main\n"
	names := NamesOf(task)
	names.Known = func(name string) bool { return name == "db7" }

	ok := task + "\ncell c2:\n  set Config.limit = 300\n  set Config.host = db7\n  set Config.state = stopped\n"
	r := CheckModelText("model", ok, Options{})
	if bad := r.ForeignNames(ok, names, 0); len(bad) > 0 {
		t.Fatalf("допустимые имена отвергнуты: %v", bad)
	}

	for _, c := range []struct{ line, want string }{
		{"  set Config.state = please_restart\n", "please_restart"},
		{"  set Config.owner = db8\n", "db8"},
		{"  set hello_world.owner = db_main\n", "hello_world"},
	} {
		text := task + "\ncell c2:\n" + c.line
		r := CheckModelText("model", text, Options{})
		bad := r.ForeignNames(text, names, 0)
		if len(bad) != 1 || !strings.Contains(bad[0].Msg, "«"+c.want+"»") {
			t.Errorf("%q: %v", c.line, bad)
		}
	}

	// Путь binds_to — только как в задаче.
	text := task + "\ncell c2:\n  set Config.port = 5432\n  binds_to: src/secret/leak.go:1-2\n"
	r = CheckModelText("model", text, Options{})
	if bad := r.ForeignNames(text, names, 0); len(bad) != 1 || !strings.Contains(bad[0].Msg, "не из задачи") {
		t.Fatalf("binds_to: %v", bad)
	}

	// Без задачи — только словарь объектов и слова языка.
	letter := "goal: count\n\nclasses:\n  Count : \"count\"\n\ncell c1:\n  set Count.total = 3\n  set Count.host = db7\n"
	r = CheckModelText("model", letter, Options{})
	if bad := r.ForeignNames(letter, &Names{Known: names.Known}, 0); len(bad) > 0 {
		t.Fatalf("письмо без задачи: %v", bad)
	}
}
