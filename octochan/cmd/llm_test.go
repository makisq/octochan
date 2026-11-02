package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ochan/embedded"
	"ochan/report"
	"ochan/workspace"

	"github.com/spf13/viper"
)

const formalNote = "goal: count\n\nclasses:\n  Count : \"count\"\n  Period : \"period\"\n\ncell c1:\n" +
	"  set Count.total = 3\n  set Period.window = 7 d\n"

// llm.mode и прежний fp.mode: natural — по-старому из fp.mode; embedded с
// natural вместе нельзя — своя модель знает только формальный промпт.
func TestLLMMode(t *testing.T) {
	newScriptEnv(t)
	t.Cleanup(func() { viper.Set("llm.mode", ""); viper.Set("fp.mode", "formal") })
	for _, c := range []struct{ llm, fpMode, want string }{
		{"", "formal", "protocol"}, {"", "natural", "natural"}, {"embedded", "formal", "embedded"},
		{"natural", "formal", "natural"},
	} {
		viper.Set("llm.mode", c.llm)
		viper.Set("fp.mode", c.fpMode)
		if mode, err := llmMode(); err != nil || string(mode) != c.want {
			t.Errorf("llm.mode %q, fp.mode %q: %s %v", c.llm, c.fpMode, mode, err)
		}
	}
	viper.Set("llm.mode", "chat")
	if _, err := llmMode(); err == nil || !strings.Contains(err.Error(), "protocol, embedded или natural") {
		t.Fatalf("неизвестный режим: %v", err)
	}
}

// Своя модель за протоколом движка: ответ — только язык, сводка колец —
// в stderr и в журнале; fp journal пишет кольцо 0 только в embedded.
func TestLLMGenerate(t *testing.T) {
	e := newScriptEnv(t)
	srv := httptest.NewServer(embedded.Handler(embedded.Info{Engine: "ochan-demo", Model: "demo"},
		&embedded.Demo{Answer: formalNote, Chatter: map[int]string{0: "Sure, here is the report"}}))
	defer srv.Close()
	viper.Set("llm.endpoint", srv.URL)
	t.Cleanup(func() { viper.Set("llm.mode", ""); viper.Set("llm.endpoint", "http://127.0.0.1:8080") })

	e.fails("llm generate — для своей модели: llm.mode: embedded", "llm", "generate")
	if out := e.must("fp", "journal"); !strings.Contains(out, "Режим: protocol") || strings.Contains(out, "кольце 0") {
		t.Fatalf("fp journal в protocol:\n%s", out)
	}

	viper.Set("llm.mode", "embedded")
	if out := e.must("llm"); !strings.Contains(out, "embedded (своя модель — логика и кольца в её коде; чужой движок ведёт декодер ochan: "+
		"кольцо 0, τ=0.42 — на позицию не больше 2 токенов)") || !strings.Contains(out, "протокол v1, ochan-demo, модель demo") ||
		!strings.Contains(out, "Подход:  протокольный — чужой движок ведёт декодер ochan: кольцо 0, τ=0.42; выводимое из посылок пишет логика ochan") {
		t.Fatalf("llm:\n%s", out)
	}
	out := e.must("llm", "generate", "--out", e.path("answer.fp"))
	if e.read("answer.fp") != formalNote || !strings.Contains(out, "утверждений вне ℒ 1") || strings.Contains(out, "Sure") {
		t.Fatalf("generate:\n%s\nответ:\n%s", out, e.read("answer.fp"))
	}
	if out := e.must("fp", "journal"); !strings.Contains(out, "Отклонено на кольце 0: ") ||
		!strings.Contains(out, "генераций 1, с ответом 1") {
		t.Fatalf("fp journal в embedded:\n%s", out)
	}
	if out := e.must("fp", "check", e.path("answer.fp")); !strings.Contains(out, "Режим:   embedded (своя модель — логика и кольца в её коде; чужой движок ведёт декодер ochan: кольцо 0, τ=0.42") {
		t.Fatalf("fp check:\n%s", out)
	}

	// Задача не на языке модели не передаётся.
	e.write(e.path("task.fp"), "please fix the database\n")
	e.fails("задача не на языке", "llm", "generate", "--prompt", e.path("task.fp"))

	// Задача без предпосылки — тоже: перечень недостающего возвращает
	// ochan, модель её не получает, и генерации в журнале не прибавляется.
	e.write(e.path("lacking.fp"), formalNote+"\nrequires: incident_history\n")
	if _, err := e.ochan("llm", "generate", "--prompt", e.path("lacking.fp")); err == nil ||
		!strings.Contains(err.Error(), "не хватает предпосылок (⊥undef 1)") || !strings.Contains(err.Error(), "incident_history") {
		t.Fatalf("задача без предпосылки: %v", err)
	}
	if out := e.must("fp", "journal"); !strings.Contains(out, "генераций 1, с ответом 1") {
		t.Fatalf("задача без предпосылки дошла до модели:\n%s", out)
	}

	// Протокол движка строгий: не подтвердил /v1/info — генерации нет.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"protocol": 2, "engine": "future", "model": "x"}`))
	}))
	defer other.Close()
	viper.Set("llm.endpoint", other.URL)
	if _, err := e.ochan("llm", "generate"); err == nil || !strings.Contains(err.Error(), "говорит по протоколу v2") ||
		!strings.Contains(err.Error(), "генерации нет") {
		t.Fatalf("чужой протокол: %v", err)
	}
	other.Close()
	if _, err := e.ochan("llm", "generate"); err == nil || !strings.Contains(err.Error(), "недоступен") ||
		!strings.Contains(err.Error(), "генерации нет") {
		t.Fatalf("движок недоступен: %v", err)
	}
	if out := e.must("fp", "journal"); !strings.Contains(out, "генераций 1, с ответом 1") {
		t.Fatalf("без подтверждённого протокола генерация дошла до журнала:\n%s", out)
	}
}

// Копию задачи пишет ochan, выводимое — логика, модель выбирает только
// порядок двух решений; с llm.decoder.derive: false решения пишет модель
// сама. Прежний ключ llm.derive читается, пока нового нет в конфиге.
func TestLLMGenerateDerives(t *testing.T) {
	e := newScriptEnv(t)
	task := "goal: probe\n\nclasses:\n  File     : \"file\"   # источник\n  Approval : \"approve\"\n  Probe    : \"probe\"\n\n" +
		"relations:\n  File --defines--> Approval (database_quota)\n  Approval --strives_to--> Probe\n\n" +
		"cell c1:\n  set File.size = 8 GB\n  set File.owner = root\n"
	copied := "goal: probe\n\nclasses:\n  File : \"file\"\n  Approval : \"approve\"\n  Probe : \"probe\"\n\n" +
		"relations:\n  File --defines--> Approval (database_quota)\n  Approval --strives_to--> Probe\n\n" +
		"cell c1:\n  set File.size = 8 GB\n  set File.owner = root\n"
	answer := copied + "\ncell c2:\n  set Approval.owner = root\n  set Approval.size = 8 GB\n\nreport:\n" +
		"  q: owner\n    choice: root\n    basis: relation(database_quota)\n" +
		"  q: size\n    choice: 8 GB\n    basis: relation(database_quota)\n"
	srv := httptest.NewServer(embedded.Handler(embedded.Info{Engine: "ochan-demo", Model: "demo"}, &embedded.Demo{Answer: answer}))
	defer srv.Close()
	viper.Set("llm.endpoint", srv.URL)
	viper.Set("llm.mode", "embedded")
	t.Cleanup(func() {
		viper.Set("llm.mode", "")
		viper.Set("llm.endpoint", "http://127.0.0.1:8080")
		viper.Set("llm.decoder.derive", true)
	})
	e.write(e.path("task.fp"), task)

	out := e.must("llm", "generate", "--prompt", e.path("task.fp"), "--out", e.path("answer.fp"))
	if e.read("answer.fp") != answer || !strings.Contains(out, "Логика:      утверждений 11 — единственные допустимые; выбрано моделью среди допустимых: 1") {
		t.Fatalf("generate:\n%s\nответ:\n%s", out, e.read("answer.fp"))
	}

	// Без логики решения пишет модель; копию — всё равно ochan.
	viper.Set("llm.decoder.derive", false)
	out = e.must("llm", "generate", "--prompt", e.path("task.fp"), "--out", e.path("answer.fp"))
	if e.read("answer.fp") != answer || strings.Contains(out, "Логика:") {
		t.Fatalf("без логики:\n%s\nответ:\n%s", out, e.read("answer.fp"))
	}

	// Прежний ключ действует, и ochan llm говорит, как он называется теперь.
	viper.Set("llm.decoder.derive", true)
	setting(t, "llm.derive", false)
	out = e.must("llm", "generate", "--prompt", e.path("task.fp"), "--out", e.path("answer.fp"))
	if strings.Contains(out, "Логика:") {
		t.Fatalf("прежний llm.derive: false не подействовал:\n%s", out)
	}
	if out := e.must("llm"); !strings.Contains(out, "выводимое из посылок пишет модель (llm.decoder.derive: false)") ||
		!strings.Contains(out, "внимание: llm.derive — прежний ключ, теперь llm.decoder.derive") {
		t.Fatalf("llm с прежним ключом:\n%s", out)
	}
}

// archModel — архитектурная модель для теста: логика и кольца «в ней»,
// отвечает целиком сама.
type archModel struct{ answer embedded.Answer }

func (m *archModel) Next(context.Context, string, string, int) (embedded.Step, error) {
	return embedded.Step{End: 1}, nil
}

func (m *archModel) Generate(context.Context, string) (embedded.Answer, error) { return m.answer, nil }

// Архитектурная модель отвечает сама, ochan проверяет ответ снаружи и
// пишет его в журнал решений; исход оператора доходит до выгрузки, из
// которой модель пересчитывает свои кольца.
func TestLLMGenerateArchitectural(t *testing.T) {
	e := newScriptEnv(t)
	task := "goal: probe\n\nclasses:\n  File : \"file\"\n  Approval : \"approve\"\n  Probe : \"probe\"\n\n" +
		"relations:\n  File --defines--> Approval (database_quota)\n  Approval --strives_to--> Probe\n\n" +
		"cell c1:\n  set File.size = 8 GB\n  set File.owner = root\n"
	owner := "  set Approval.owner = root\n"
	size := "  set Approval.size = 8 GB\n"
	report := func(lines ...string) string { return strings.Join(lines, "") }
	full := task + "\ncell c2:\n" + owner + size + "\nreport:\n" +
		report("  q: owner\n    choice: root\n    basis: relation(database_quota)\n",
			"  q: size\n    choice: 8 GB\n    basis: relation(database_quota)\n")
	pkg := func(line, verdict string) embedded.ModelPackage {
		return embedded.ModelPackage{Statement: strings.TrimSpace(line), Rules: []string{"fp.defines"}, L: 0.66, U: 1, Verdict: verdict}
	}
	model := &archModel{}
	srv := httptest.NewServer(embedded.Handler(embedded.Info{Engine: "own", Model: "own-small"}, model))
	defer srv.Close()
	viper.Set("llm.endpoint", srv.URL)
	viper.Set("llm.mode", "embedded")
	t.Cleanup(func() { viper.Set("llm.mode", ""); viper.Set("llm.endpoint", "http://127.0.0.1:8080") })
	e.write(e.path("task.fp"), task)

	model.answer = embedded.Answer{Text: full, Forced: 9, Chosen: 1,
		Packages: []embedded.ModelPackage{pkg(owner, "well_true"), pkg(size, "well_true")}}
	out := e.must("llm", "generate", "--prompt", e.path("task.fp"), "--out", e.path("answer.fp"))
	if e.read("answer.fp") != full || !strings.Contains(out, "Архитектурная модель: ответ написала она сама") {
		t.Fatalf("generate:\n%s", out)
	}
	_, rest, _ := strings.Cut(out, "Журнал решений: запись ")
	id, _, _ := strings.Cut(rest, ";")

	// Режим у своей модели один — архитектурный: настройки декодера она не
	// читает, и ochan говорит об этом, а не молча их обходит.
	if out := e.must("llm"); !strings.Contains(out, "Подход:  архитектурный — своя модель ℒ") ||
		strings.Contains(out, "внимание:") {
		t.Fatalf("llm у своей модели:\n%s", out)
	}
	setting(t, "llm.rings.tau", 0.3)
	if out := e.must("llm"); !strings.Contains(out, "внимание: llm.rings.tau — настройки декодера ochan для чужого движка; своя модель их не читает") {
		t.Fatalf("настройка декодера у своей модели:\n%s", out)
	}
	if out := e.must("llm", "generate", "--prompt", e.path("task.fp")); !strings.Contains(out, "внимание: llm.rings.tau — настройки декодера") {
		t.Fatalf("generate с настройкой декодера:\n%s", out)
	}
	viper.Set("llm.rings.tau", nil)
	e.fails("своя модель отвечает на задачу: --prompt задача.fp", "llm", "generate")
	if out := e.must("fp", "journal", "export"); !strings.Contains(out, `"plans": []`) {
		t.Fatalf("исхода ещё нет, а выгрузка его знает:\n%s", out)
	}
	e.must("fp", "journal", "amend", id, "--right", "--reason", "ответ верен")
	if out := e.must("fp", "journal", "export"); !strings.Contains(out, `"fp.defines": [`) || !strings.Contains(out, `"correct": true`) {
		t.Fatalf("исход оператора не дошёл до выгрузки:\n%s", out)
	}

	// Решение, которого логика ochan не выводит, — ответа нет.
	model.answer.Text = strings.Replace(full, owner, "  set Probe.owner = root\n", 1)
	if _, err := e.ochan("llm", "generate", "--prompt", e.path("task.fp")); err == nil ||
		!strings.Contains(err.Error(), "ответ расходится с логикой ochan") {
		t.Fatalf("чужое решение принято: %v", err)
	}

	// Решение, которое кольцо 2 модели не приняло, можно пропустить — это
	// вопрос оператору.
	model.answer = embedded.Answer{Text: task + "\ncell c2:\n" + owner + "\nreport:\n" +
		"  q: owner\n    choice: root\n    basis: relation(database_quota)\n",
		Packages:  []embedded.ModelPackage{pkg(owner, "well_true"), pkg(size, "well_false")},
		Questions: []embedded.ModelQuestion{{Statement: strings.TrimSpace(size), Why: "правило проверено мало"}}}
	if out := e.must("llm", "generate", "--prompt", e.path("task.fp")); !strings.Contains(out, "Кольцо 2:    не принято 1") ||
		!strings.Contains(out, "Вопрос оператору: set Approval.size = 8 GB; правило проверено мало") {
		t.Fatalf("непринятое кольцом 2:\n%s", out)
	}

	// Задача повторяет кластер инцидентов, который модель решает плохо: её
	// кольцо 2 по исходам кластера не приняло решение, и вся задача —
	// вопрос оператору.
	clustered := pkg(size, "well_false")
	clustered.Cluster = "3fa2c1d4e5b6"
	model.answer.Packages[1] = clustered
	model.answer.Cluster = &embedded.ModelCluster{Key: "3fa2c1d4e5b6", Instances: 8, Solved: 1, Outcomes: 6, L: 0.22, U: 0.6}
	model.answer.Questions = append(model.answer.Questions,
		embedded.ModelQuestion{Cluster: "3fa2c1d4e5b6", Why: "инциденты этой структуры модель решила сама 1 из 6"})
	if out := e.must("llm", "generate", "--prompt", e.path("task.fp")); !strings.Contains(out,
		"Кластер:     3fa2c1d4e5b6 — задача повторяет структуру 8 инцидентов; модель решила сама 1.0 из 6.0, надёжность [0.22; 0.60]") ||
		!strings.Contains(out, "Вопрос оператору: задача целиком; инциденты этой структуры модель решила сама 1 из 6") {
		t.Fatalf("кластер инцидентов:\n%s", out)
	}
	journal, err := decisionStore()
	if err != nil {
		t.Fatal(err)
	}
	all, err := journal.All()
	if err != nil {
		t.Fatal(err)
	}
	if last := all[len(all)-1]; last.Packages[1].Cluster != "3fa2c1d4e5b6" || last.Packages[0].Cluster != "" {
		t.Fatalf("в журнале — на каком кластере решала модель: %+v", last.Packages)
	}

	model.answer = embedded.Answer{Refused: true, Missing: []string{"incident_history"}}
	if _, err := e.ochan("llm", "generate", "--prompt", e.path("task.fp")); err == nil ||
		!strings.Contains(err.Error(), "модель отказала: не хватает предпосылок — incident_history") {
		t.Fatalf("отказ модели: %v", err)
	}
}

// Почта: агенту — только формальный промпт; людям — на языке режима; своя
// модель — только формальный; модель адресов не выбирает.
func TestMail(t *testing.T) {
	e := newScriptEnv(t)
	viper.Set("mail.groups", map[string]any{"operators": []string{"ops@corp.example"}})
	t.Cleanup(func() {
		viper.Set("mail.groups", nil)
		viper.Set("mail.lang", "auto")
		viper.Set("llm.mode", "")
	})
	var sent []report.Mail
	var sentTo [][]string
	mailSender = func(cfg report.MailConfig, mail report.Mail) error {
		sent = append(sent, mail)
		sentTo = append(sentTo, cfg.To)
		return nil
	}
	t.Cleanup(func() { mailSender = report.Send })
	e.must("agents", "beat", "--name", "db8")
	e.write(e.path("note.fp"), formalNote)

	t.Setenv(EnvRole, "llm")
	t.Setenv(EnvAgent, "db7")
	e.fails("агентам пишут только формальным промптом", "mail", "send", "--to", "db8", "проверь реплику, пожалуйста")
	out := e.must("mail", "send", "--to", "db8", e.path("note.fp"))
	if !strings.Contains(out, "(формальный промпт)") || !strings.Contains(out, "в ящики: db8") {
		t.Fatalf("агенту:\n%s", out)
	}
	// По умолчанию режим protocol: людям — тоже формальным промптом.
	e.fails("людям пишут формальным промптом", "mail", "send", "--to", "operators", "всё починила")
	e.fails("--subject: у формального письма темы нет", "mail", "send", "--to", "operators", "--subject", "отчёт", e.path("note.fp"))
	e.must("mail", "send", "--to", "operators", e.path("note.fp"))
	if len(sent) != 1 || sentTo[0][0] != "ops@corp.example" || sent[0].Subject != "[ochan] goal: count — llm, агент db7" {
		t.Fatalf("письмо людям: %v %+v", sentTo, sent)
	}
	e.fails("модель пишет только агентам и группам", "mail", "send", "--to", "boss@corp.example", e.path("note.fp"))
	e.write(e.path("comment.fp"), strings.Replace(formalNote, "= 3", "= 3  # всё хорошо", 1))
	e.fails("комментарий в тексте модели", "mail", "send", "--to", "db8", e.path("comment.fp"))

	// natural — людям естественным языком, агентам по-прежнему формальным.
	viper.Set("llm.mode", "natural")
	e.must("mail", "send", "--to", "operators", "--subject", "реплика", "реплика db8 догнала, отставание 0")
	if sent[1].Subject != "реплика" || sent[1].Body != "реплика db8 догнала, отставание 0" {
		t.Fatalf("natural: %+v", sent[1])
	}
	e.fails("агентам пишут только формальным промптом", "mail", "send", "--to", "db8", "реплика догнала")

	// Своя модель — только формальный промпт, даже при mail.lang: natural.
	viper.Set("llm.mode", "embedded")
	viper.Set("mail.lang", "natural")
	e.fails("своя модель (llm.mode: embedded) пишет только формальным промптом", "mail", "send", "--to", "operators", "готово")
	t.Setenv(EnvRole, "master")
	e.must("mail", "send", "--to", "operators", "--subject", "от человека", "готово")

	// Записи инцидента своей модели — тоже только формальным промптом.
	t.Setenv(EnvRole, "llm")
	e.fails("что случилось — своя модель (llm.mode: embedded) пишет только формальным промптом",
		"incident", "open", "--level", "low", "диск db7 заполнен")
	if out := e.must("incident", "open", "--level", "low", formalNote); !strings.Contains(out, "(low) открыт") {
		t.Fatalf("формальная запись: %s", out)
	}
	t.Setenv(EnvRole, "master")

	// Модель читает только ящик своего агента.
	t.Setenv(EnvRole, "llm")
	e.fails("модель читает только ящик своего агента", "mail", "--agent", "db8")
	t.Setenv(EnvRole, "master")

	// Ящик агента.
	t.Setenv(EnvAgent, "db8")
	out = e.must("mail")
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "llm, агент db7") || !strings.Contains(out, "goal: count") {
		t.Fatalf("ящик db8:\n%s", out)
	}
	box, _ := mailbox()
	all, _ := box.List()
	inbox := workspace.MessagesFor(all, "db8")
	if out := e.must("mail", inbox[0].ID[:4]); !strings.Contains(out, "Язык:    формальный промпт") || !strings.Contains(out, "set Count.total = 3") {
		t.Fatalf("письмо:\n%s", out)
	}
}

// Своя модель не прячет естественный язык ни в имени, ни в строке: имя — из
// задачи, словаря объектов ochan или слов языка; в сценарии нет
// комментариев, а строка — имя, путь или значение из того же множества.
func TestModelNamesClosed(t *testing.T) {
	e := newScriptEnv(t)
	viper.Set("llm.mode", "embedded")
	viper.Set("mail.groups", map[string]any{"operators": []string{"ops@corp.example"}})
	t.Cleanup(func() { viper.Set("llm.mode", ""); viper.Set("mail.groups", nil) })
	mailSender = func(report.MailConfig, report.Mail) error { return nil }
	t.Cleanup(func() { mailSender = report.Send })
	e.must("agents", "beat", "--name", "db8")

	t.Setenv(EnvRole, "llm")
	note := "goal: count\n\nclasses:\n  Count : \"count\"\n\ncell c1:\n  set Count.total = 3\n  set Count.host = db8\n"
	e.must("mail", "send", "--to", "operators", note)
	e.fails("имя «please_restart» не из задачи и не из словаря объектов ochan",
		"mail", "send", "--to", "operators", strings.Replace(note, "db8", "please_restart", 1))

	// Сценарий своей модели.
	e.write(e.path("task.fp"), "goal: tuned\n\nclasses:\n  Config : \"config\"\n  State : \"tuned\"\n\n"+
		"relations:\n  Config --strives_to--> State\n\nhard forbid save(except = {prod_conf})\n")
	e.write(e.path("ok.ochan"), "c = поднять(config(\"prod.conf\"), 200)\nsave(c, \"prod.conf\")\n")
	e.write(e.path("chatty.ochan"), "c = поднять(config(\"prod.conf\"), 200)  # поднимаю лимит\n"+
		"print(\"please restart the server\")\nsave(c, \"prod.conf\")\n")
	out, err := e.ochan("plan", "--script", e.path("chatty.ochan"), "--prompt", e.path("task.fp"))
	if err == nil || !strings.Contains(err.Error(), "комментарий в сценарии своей модели") ||
		!strings.Contains(err.Error(), `строка "please restart the server"`) {
		t.Fatalf("болтливый сценарий своей модели: %v\n%s", err, out)
	}
	if _, err := e.ochan("plan", "--script", e.path("ok.ochan"), "--prompt", e.path("task.fp")); err != nil &&
		strings.Contains(err.Error(), "своя модель (llm.mode: embedded) пишет только на ℒ") {
		t.Fatalf("сценарий без естественного языка отвергнут: %v", err)
	}

	// Внешней модели (protocol) строки нужны: её сценарий так не проверяется.
	viper.Set("llm.mode", "protocol")
	if _, err := e.ochan("plan", "--script", e.path("chatty.ochan"), "--prompt", e.path("task.fp")); err != nil &&
		strings.Contains(err.Error(), "своя модель") {
		t.Fatalf("сценарий внешней модели проверен как своей: %v", err)
	}
}
