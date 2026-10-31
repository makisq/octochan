package embedded

import (
	"context"
	"regexp"
	"strings"
)

// Demo — демонстрационная модель: проверить декодер, протокол и настройки
// без настоящей модели. Знаний у неё нет. Она ведёт к заданному ответу
// Answer и по дороге ведёт себя как слабая модель:
//
//   - в начале утверждений из Chatter пробует заговорить — «Sure, here is
//     the report» или по-русски — и уверена в этом больше, чем в ответе;
//   - на каждом шаге у неё есть второй кандидат ниже порога.
//
// Ответ, к которому она ведёт, — её «знание»: с настоящей моделью его нет.
type Demo struct {
	Answer string
	// Chatter — номер строки ответа (с нуля) → фраза на естественном языке,
	// которую модель пробует сказать в начале этой строки.
	Chatter map[int]string
}

var demoToken = regexp.MustCompile(`\n| *[A-Za-z0-9_]+| *[^\sA-Za-z0-9_]| +`)

func demoTokens(text string) []string { return demoToken.FindAllString(text, -1) }

// Next — распределение следующего токена.
func (m *Demo) Next(_ context.Context, _, output string, top int) (Step, error) {
	answer := demoTokens(m.Answer)
	done := ""
	for i := 0; i <= len(answer); i++ {
		if output == done {
			return m.onAnswer(answer, i, output), nil
		}
		// Внутри фразы вместо утверждения: модель договаривает её.
		if chatter, ok := m.Chatter[strings.Count(done, "\n")]; ok && strings.HasPrefix(output, done) &&
			(done == "" || strings.HasSuffix(done, "\n")) {
			said := strings.TrimPrefix(output, done)
			if rest, ok := strings.CutPrefix(chatter+"\n", said); ok && rest != "" {
				return Step{Candidates: []Candidate{{Text: demoTokens(rest)[0], P: 0.9}}, End: 0.01}, nil
			}
		}
		if i == len(answer) {
			break
		}
		done += answer[i]
	}
	// Вне сценария модель ни в чём не уверена.
	return Step{Candidates: []Candidate{{Text: "\n", P: 0.2}, {Text: " ok", P: 0.1}}, End: 0.1}, nil
}

func (m *Demo) onAnswer(answer []string, i int, output string) Step {
	if i == len(answer) {
		return Step{Candidates: []Candidate{{Text: "\n", P: 0.03}}, End: 0.95}
	}
	next := answer[i]
	if chatter, ok := m.Chatter[strings.Count(output, "\n")]; ok && (output == "" || strings.HasSuffix(output, "\n")) {
		return Step{Candidates: []Candidate{{Text: demoTokens(chatter)[0], P: 0.52}, {Text: next, P: 0.44}}, End: 0.01}
	}
	return Step{Candidates: []Candidate{{Text: next, P: 0.86}, {Text: " restart", P: 0.09}}, End: 0.01}
}
