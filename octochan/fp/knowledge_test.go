package fp

import (
	"strings"
	"testing"
)

const knowledgeFile = `protocol: 1
generalizations:
  - key: 75cf2d265cfe
    shape:
      - class Log
      - relation Log modifies Storage
    similarity: 0.8
    instances: 8
    outcomes: [1.0, 5.8]
    rules:
      fp.modifies: [1.0, 5.8]
`

// Знания пакета: обобщения со счётом исходов; опечатка, невозможный счёт
// или знания новее сборки — отказ, а не модель без знаний.
func TestParseKnowledge(t *testing.T) {
	k, err := ParseKnowledge("knowledge.yaml", []byte(knowledgeFile))
	if err != nil {
		t.Fatal(err)
	}
	g := k.Generalizations[0]
	if len(k.Generalizations) != 1 || g.Key != "75cf2d265cfe" || g.Instances != 8 || g.Rules["fp.modifies"][1] != 5.8 {
		t.Fatalf("знания: %+v", g)
	}
	for _, c := range []struct{ from, to, want string }{
		{"instances: 8", "instance: 8", "field instance not found"},
		{"outcomes: [1.0, 5.8]", "outcomes: [7.0, 5.8]", "успехов от 0 до числа исходов"},
		{"outcomes: [1.0, 5.8]", "outcomes: [1.0]", "ожидалось [успехов, исходов]"},
		{"similarity: 0.8", "similarity: 0", "similarity 0 — доля"},
		{"fp.modifies:", "modifies:", "правило вывода fp.<тип>"},
		{"protocol: 1", "protocol: 2", "обновите ochan"},
		{"key: 75cf2d265cfe", "key: \"75cf 2d\"", "ключ"},
	} {
		_, err := ParseKnowledge("knowledge.yaml", []byte(strings.Replace(knowledgeFile, c.from, c.to, 1)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s → %s: %v", c.from, c.to, err)
		}
	}
	if _, err := ParseKnowledge("knowledge.yaml", []byte("protocol: 1\n")); err == nil || !strings.Contains(err.Error(), "нет обобщений") {
		t.Errorf("без обобщений: %v", err)
	}

	// Задача подключает знания import'ом: негодные знания — задачу не
	// проверить, как и с негодным словарём.
	prompt := "goal: available\n\nclasses:\n  Log : \"log\"\n  Storage : \"disk\"\n  State : \"available\"\n\n" +
		"relations:\n  Log --modifies--> Storage (log_size)\n  Storage --strives_to--> State\n\n" +
		"import @pg_incidents v1\n\ncell c1:\n  set Log.size = 900 MB\n"
	library := map[string]*Skill{"pg_incidents": {Version: "1.0", Knowledge: k}}
	opts := Options{Lexicon: BaseLexicon(), Library: func(name string) (*Skill, bool) { s, ok := library[name]; return s, ok }}
	if r := CheckText("t.fp", prompt, opts); r.Errors() > 0 {
		t.Fatalf("знания подключаются: %v", r.Diagnostics)
	}
	library["pg_incidents"] = &Skill{Version: "1.0", KnowledgeErr: errString("обобщение 1: нет outcomes")}
	expect(t, CheckText("t.fp", prompt, opts), ErrUndef, "знания пакета @pg_incidents негодны")
}
