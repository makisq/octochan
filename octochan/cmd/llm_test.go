package cmd

import (
	"net/http/httptest"
	"strings"
	"testing"

	"octochan/embedded"
	"octochan/report"
	"octochan/workspace"

	"github.com/spf13/viper"
)

const formalNote = "goal: count\n\nclasses:\n  Count : \"count\"\n  Period : \"period\"\n\ncell incidents:\n" +
	"  set Count.total = 3\n  set Period.length = 7 d\n"

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
	if out := e.must("llm"); !strings.Contains(out, "embedded (кольцо 0 активно, τ=0.42: на позицию проходит не больше 2 токенов)") ||
		!strings.Contains(out, "протокол v1, ochan-demo, модель demo") {
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
	if out := e.must("fp", "check", e.path("answer.fp")); !strings.Contains(out, "Режим:   embedded (кольцо 0 активно, τ=0.42") {
		t.Fatalf("fp check:\n%s", out)
	}

	// Задача не на языке модели не передаётся.
	e.write(e.path("task.fp"), "please fix the database\n")
	e.fails("задача не на языке", "llm", "generate", "--prompt", e.path("task.fp"))
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
