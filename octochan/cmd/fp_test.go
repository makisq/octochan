package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/fp"
)

// Скиллы промпта — пакеты алиасов: import сверяется с библиотекой,
// а не принимается на слово.
func TestFPCheckUsesAliasLibrary(t *testing.T) {
	example, err := filepath.Abs("../examples/fp/connections.fp")
	if err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile("../examples/fp/pgconf/pgconf.ochan")
	if err != nil {
		t.Fatal(err)
	}
	lexicon, err := os.ReadFile("../examples/fp/pgconf/lexicon.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile("../examples/fp/pgconf/package.yaml")
	if err != nil {
		t.Fatal(err)
	}

	e := newScriptEnv(t)
	out, err := e.ochan("fp", "check", example)
	if err == nil || !strings.Contains(out, "⊥undef: скилла @pgconf нет в библиотеке") {
		t.Fatalf("скилл без пакета принят: %v\n%s", err, out)
	}

	if out := e.must("fp", "check", "--standalone", example); !strings.Contains(out, "P ∈ ℒ") {
		t.Fatalf("без библиотеки скилл даёт объявленное в provides:\n%s", out)
	}

	pkg := filepath.Join(e.home, ".octochan", "modules", "pgconf")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(pkg, "pgconf.ochan"), string(skill))
	// Реестр алиасов кешируется на процесс: новый пакет виден новому запуску.
	resetAliasCache()

	// Пример требует v1, а у пакета без манифеста версии нет.
	out, err = e.ochan("fp", "check", example)
	if err == nil || !strings.Contains(out, "промпт требует @pgconf v1, а у скилла в библиотеке версии нет") {
		t.Fatalf("требование версии к безверсионному пакету принято: %v\n%s", err, out)
	}

	e.write(filepath.Join(pkg, "package.yaml"), string(manifest))
	resetAliasCache()

	// Слово области приносит словарь скилла: без него «connections» вне
	// языка.
	out, err = e.ochan("fp", "check", example)
	if err == nil || !strings.Contains(out, `⊥syn: слова "connections" нет в словаре языка`) {
		t.Fatalf("слово без словаря скилла принято: %v\n%s", err, out)
	}
	e.write(filepath.Join(pkg, "lexicon.yaml"), string(lexicon))
	resetAliasCache()

	out = e.must("fp", "check", example)
	for _, want := range []string{"P ∈ ℒ", "K:       connection_limit, memory_budget, set_param, store",
		"Скилл:   @pgconf 1.2.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}

	if out := e.must("aliases"); !strings.Contains(out, "Пакет pgconf 1.2.0 — правка параметров конфигурации PostgreSQL") {
		t.Fatalf("aliases не показывает версию пакета:\n%s", out)
	}

	e.write(e.path("v2.fp"), "goal: tuned\nclasses:\n  State : \"tuned\"\nimport @pgconf v2\n")
	out, err = e.ochan("fp", "check", "v2.fp")
	if err == nil || !strings.Contains(out, "промпт требует @pgconf v2, а в библиотеке версия 1.2.0") {
		t.Fatalf("несовпадение версии не найдено: %v\n%s", err, out)
	}

	// Операции, которой в пакете нет, скилл не даёт.
	e.write(e.path("bad.fp"), "goal: tuned\nclasses:\n  State : \"tuned\"\nimport @pgconf\n  provides: restart\n")
	e.fails("⊥undef 1", "fp", "check", "bad.fp")
}

func TestFPCheckReportsCodes(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("p.fp"), "goal: tuned\nclasses:\n  State : \"tuned\"\nrequires: a, b\nhow are you?\n")
	out, err := e.ochan("fp", "check", "p.fp")
	if err == nil || !strings.Contains(err.Error(), "p.fp: отказов 1 (⊥syn 1)") {
		t.Fatalf("ошибка: %v\n%s", err, out)
	}
	if !strings.Contains(out, "p.fp:5:12: ⊥syn: вопрос — не высказывание языка") {
		t.Fatalf("вывод:\n%s", out)
	}

	e.fails("--name задаёт имя промпта, поданного на stdin", "fp", "check", "--name", "x", "p.fp")
}

func TestFPProtocolCommand(t *testing.T) {
	e := newScriptEnv(t)
	if out := e.must("fp", "protocol"); out != fp.Protocol() {
		t.Fatal("ochan fp protocol отдаёт не тот текст, что проверяет пакет fp")
	}

	// fp.mode: natural — модель принимает задачи на естественном языке.
	setting(t, "fp.mode", "natural")
	out := e.must("fp", "protocol")
	if out != fp.ProtocolFor(fp.Natural) || !strings.Contains(out, "tasks come to you in\nnatural language") {
		t.Fatalf("в режиме natural не тот протокол:\n%s", out)
	}
	if e.must("fp", "protocol", "--mode", "formal") != fp.Protocol() {
		t.Fatal("--mode formal не показал формальный протокол")
	}
	setting(t, "fp.mode", "prose")
	e.fails(`fp.mode: "prose", допустимо formal или natural`, "fp", "protocol")
}

// Язык замкнут: слово со смыслом — из словаря, а словарь виден
// оператору и модели командой fp words.
func TestFPWords(t *testing.T) {
	e := newScriptEnv(t)
	out := e.must("fp", "words")
	for _, want := range []string{"server", "Node, Service", "n=2 < nl=4", "Служебные: a, after",
		"Единицы:   B, GB", "Слов со смыслом"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в словаре нет %q:\n%s", want, out)
		}
	}
	out = e.must("fp", "words", "set", "before", "limit")
	for _, want := range []string{"set     ключевое слово языка", "before  служебное слово", "limit   Limit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в статьях нет %q:\n%s", want, out)
		}
	}
	e.fails("нет ни в одном словаре: rock", "fp", "words", "rock")

	// Потолок fp.max_meanings убирает из языка многозначные слова.
	setting(t, "fp.max_meanings", 1)
	if out := e.must("fp", "words", "server"); !strings.Contains(out, "(вне языка при fp.max_meanings = 1)") {
		t.Fatalf("потолок не виден:\n%s", out)
	}
	e.write(e.path("web.fp"), "goal: tuned\nclasses:\n  State : \"tuned\"\n  Service : \"server\"\n")
	if out, err := e.ochan("fp", "check", "web.fp"); err == nil || !strings.Contains(out, "а потолок fp.max_meanings = 1") {
		t.Fatalf("многозначное слово при потолке 1 принято: %v\n%s", err, out)
	}
}

// Свои слова оператор подключает словарём в config.yaml.
func TestOperatorLexicon(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("rock.fp"), "goal: tuned\nclasses:\n  State : \"tuned\"\n  Obstacle : \"rock\"\n")
	if out, err := e.ochan("fp", "check", "rock.fp"); err == nil || !strings.Contains(out, `слова "rock" нет в словаре языка`) {
		t.Fatalf("слово вне словаря принято: %v\n%s", err, out)
	}

	lexicon := e.path("words.yaml")
	e.write(lexicon, "words:\n  rock: {senses: [Obstacle, Genre], nl: 9}\n")
	setting(t, "fp.lexicons", []string{lexicon})
	e.must("fp", "check", "rock.fp")
	if out := e.must("fp", "words", "rock"); !strings.Contains(out, "Obstacle, Genre") || !strings.Contains(out, lexicon) {
		t.Fatalf("слово оператора:\n%s", out)
	}

	// Негодный словарь — отказ настройки, а не тихо пропущенные слова.
	e.write(lexicon, "words:\n  rock: {senses: [Obstacle, Genre], nl: 2}\n")
	e.fails("config.yaml fp.lexicons", "fp", "check", "rock.fp")
}

// Пакет приносит словарь своей области: !!install его проверяет и
// копирует, import подключает к промпту.
func TestPackageLexicon(t *testing.T) {
	e := newScriptEnv(t)
	src := filepath.Join(t.TempDir(), "roads")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(src, "lib.ochan"), "alias around(c: config) {\n    return c\n}\n")
	e.write(filepath.Join(src, "package.yaml"), "version: 1.0.0\n")
	e.write(filepath.Join(src, fp.LexiconName), "words:\n  rock: {senses: [Obstacle], nl: 9\n")
	e.fails("пакет roads не установлен", "!!install", src)

	e.write(filepath.Join(src, fp.LexiconName), "words:\n  rock: {senses: [Obstacle], nl: 9}\n")
	if out := e.must("!!install", src); !strings.Contains(out, "Словарь пакета: слов 1 — ochan fp words --skill roads") {
		t.Fatalf("установка словаря:\n%s", out)
	}
	if out := e.must("fp", "words", "--skill", "roads"); !strings.Contains(out, "rock") || !strings.Contains(out, "@roads") {
		t.Fatalf("словарь скилла:\n%s", out)
	}

	prompt := "goal: tuned\nclasses:\n  State : \"tuned\"\n  Obstacle : \"rock\"\n"
	e.write(e.path("task.fp"), prompt)
	out, err := e.ochan("fp", "check", "task.fp")
	if err == nil || !strings.Contains(out, "есть в словаре @roads v1.0.0 — подключите: import @roads v1.0.0") {
		t.Fatalf("подсказка о словаре скилла: %v\n%s", err, out)
	}
	e.write(e.path("task.fp"), prompt+"import @roads v1\n")
	e.must("fp", "check", "task.fp")
}
