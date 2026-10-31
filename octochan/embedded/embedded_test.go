package embedded

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

const answer = "goal: count\n\nclasses:\n  Count : \"count\"\n  Period : \"period\"\n\ncell incidents:\n" +
	"  set Count.total = 3\n  set Period.length = 7 d\n"

// Модель уверена в болтовне больше, чем в ответе, — а в ответ попадает
// только язык: фраза по-английски собрана и отвергнута целиком, по-русски
// — на первом же токене.
func TestNaturalLanguageNeverLeaves(t *testing.T) {
	m := &Demo{Answer: answer, Chatter: map[int]string{0: "Sure, here is the report", 6: "Готово, вот отчёт"}}
	out, err := Generate(context.Background(), m, "", Config{Tau: 0.42})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != answer {
		t.Fatalf("ответ:\n%s", out.Text)
	}
	if strings.Contains(out.Text, "Sure") || out.OutOfLanguage != 1 || out.Masked != 1 || out.Rollbacks != 1 {
		t.Fatalf("вне языка %d, маска %d, откатов %d:\n%s", out.OutOfLanguage, out.Masked, out.Rollbacks, out.Text)
	}
	// Второй кандидат ниже порога на каждом шаге ответа — кольцо 0.
	if out.Ring0 < out.Tokens-2 || len(out.Statements) != 7 {
		t.Fatalf("кольцо 0: %d при %d токенах, утверждений %d", out.Ring0, out.Tokens, len(out.Statements))
	}
	if s := out.Statements[0]; s.Text != "goal: count" || s.MeanLogProb >= 0 {
		t.Fatalf("первое утверждение: %+v", s)
	}
}

// Модель, которая знает только естественный язык, ответа не даёт вовсе.
func TestChatterOnlyHasNoAnswer(t *testing.T) {
	m := &Demo{Answer: "Sure, here is the report\n", Chatter: map[int]string{}}
	_, err := Generate(context.Background(), m, "", Config{Tau: 0.42, Retries: 4})
	var f *Failure
	if !errors.As(err, &f) || f.Output.Text != "" {
		t.Fatalf("ожидался отказ без текста: %v", err)
	}
}

// table — модель-таблица: префикс ответа → распределение.
type table map[string]Step

func (t table) Next(_ context.Context, _, output string, _ int) (Step, error) {
	if s, ok := t[output]; ok {
		return s, nil
	}
	return Step{End: 0.9}, nil
}

// Утверждение ушло из языка в середине — запрещён токен, на который
// указала находка («лишнее good»), а не начало утверждения. Без него
// продолжения нет, и откат снимает ещё один токен: модель пишет число.
func TestRollbackBansTheCulprit(t *testing.T) {
	head := "goal: count\n\nclasses:\n  Count : \"count\"\n\ncell c:\n"
	m := table{
		"":                          {Candidates: []Candidate{{Text: head, P: 0.9}}},
		"goal: count\n":             {Candidates: []Candidate{{Text: "\n", P: 0.9}}},
		"goal: count\n\n":           {Candidates: []Candidate{{Text: "classes:\n", P: 0.9}}},
		"goal: count\n\nclasses:\n": {Candidates: []Candidate{{Text: "  Count : \"count\"\n", P: 0.9}}},
		"goal: count\n\nclasses:\n  Count : \"count\"\n":   {Candidates: []Candidate{{Text: "\n", P: 0.9}}},
		"goal: count\n\nclasses:\n  Count : \"count\"\n\n": {Candidates: []Candidate{{Text: "cell c:\n", P: 0.9}}},
		head:                                  {Candidates: []Candidate{{Text: "  set", P: 0.9}}},
		head + "  set":                        {Candidates: []Candidate{{Text: " Count.total =", P: 0.9}}},
		head + "  set Count.total =":          {Candidates: []Candidate{{Text: " all", P: 0.6}, {Text: " 3", P: 0.45}}},
		head + "  set Count.total = all":      {Candidates: []Candidate{{Text: " good", P: 0.9}}},
		head + "  set Count.total = all good": {Candidates: []Candidate{{Text: "\n", P: 0.9}}},
		head + "  set Count.total = 3":        {Candidates: []Candidate{{Text: "\n", P: 0.9}}},
	}
	out, err := Generate(context.Background(), m, "", Config{Tau: 0.42})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.Text, "  set Count.total = 3\n") || out.OutOfLanguage != 1 || out.Dead != 1 || out.Rollbacks != 2 {
		t.Fatalf("откат %d, вне языка %d:\n%s", out.Rollbacks, out.OutOfLanguage, out.Text)
	}
}

// Порог не прошёл никто в начале утверждения — ответа нет; кольцо 0 не
// даёт модели напечатать то, в чём она не уверена.
func TestRingZeroDeadEnd(t *testing.T) {
	m := table{"": {Candidates: []Candidate{{Text: "goal", P: 0.3}, {Text: "Sure", P: 0.3}}, End: 0.1}}
	_, err := Generate(context.Background(), m, "", Config{Tau: 0.42})
	if err == nil || !strings.Contains(err.Error(), "кольцо 0") {
		t.Fatalf("ожидался отказ кольца 0: %v", err)
	}
}

// Протокол v1 по HTTP: тот же ответ, что без сети; распределение, которое
// не распределение, — отказ.
func TestHTTPProtocol(t *testing.T) {
	srv := httptest.NewServer(Handler(Info{Engine: "demo", Model: "demo"}, &Demo{Answer: answer}))
	defer srv.Close()
	h := NewHTTP(srv.URL, 0)
	info, err := h.Info(context.Background())
	if err != nil || info.Protocol != Protocol || info.Engine != "demo" {
		t.Fatalf("info: %+v %v", info, err)
	}
	out, err := Generate(context.Background(), h, "prompt", Config{Tau: 0.42})
	if err != nil || out.Text != answer {
		t.Fatalf("по HTTP: %v\n%v", err, out)
	}

	bad := httptest.NewServer(Handler(Info{}, table{"": {Candidates: []Candidate{{Text: "goal", P: 0.9}, {Text: "x", P: 0.5}}}}))
	defer bad.Close()
	if _, err := NewHTTP(bad.URL, 0).Next(context.Background(), "", "", 2); err == nil || !strings.Contains(err.Error(), "не распределение") {
		t.Fatalf("сумма 1,4: %v", err)
	}
}
