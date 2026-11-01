package fp

import (
	"fmt"
	"regexp"
	"strings"
)

// Вывод из промпта — то, что следует из его точных посылок без модели:
// правила вывода кольца 1, у которых надёжность — единица. Здесь модель
// не нужна, и её здесь нет: она выбирает только там, где посылки оставляют
// выбор (П6).
//
//   - отношение defines, limits или modifies переносит атрибут, который
//     ячейка задаёт источнику, на цель, если цели он не задан;
//   - два источника дают цели разные значения — решает приоритет (A < B:
//     побеждает A); без приоритета это открытый вопрос оператору (П5);
//   - атрибут, заданный обобщению — схеме отношения, переходит на каждую
//     его стрелку-частный случай, у которой его нет; атрибут, совпадающий
//     со слотом, — нет: значение слота задаёт сама стрелка. Обратно не
//     переходит ничего;
//   - значение, заданное промптом, не переписывается.

// carrying — типы отношений, которые переносят атрибут источника на цель.
var carrying = map[string]bool{"defines": true, "limits": true, "modifies": true}

// Decision — решение, выведенное из посылок: set Target.Attr = Value.
type Decision struct {
	Target, Attr, Value string
	// Alternatives — значения источников конфликта, решённого приоритетом.
	Alternatives []string
	// Basis — основание в отчёте: relation(имя) или priority(A < B).
	Basis string
}

// Line — утверждение ячейки с решением.
func (d Decision) Line() string { return fmt.Sprintf("  set %s.%s = %s\n", d.Target, d.Attr, d.Value) }

// OpenQuestion — вопрос, который посылки не закрывают: разные значения
// одного атрибута без приоритета между источниками. Решает оператор.
type OpenQuestion struct {
	Targets []string
	Attr    string
	Values  []string
}

// Derivation — решения и открытые вопросы промпта.
type Derivation struct {
	Decisions []Decision
	Open      []OpenQuestion
}

// Derive выводит решения из разобранного промпта. Порядок решений — по
// первому заданию атрибута в ячейках, затем по отношениям и стрелкам.
func Derive(prog *Program) Derivation {
	type key struct{ target, attr string }
	given := map[key]string{}
	var order []key
	for _, cell := range prog.Cells {
		for _, set := range cell.Sets {
			target, attr, ok := strings.Cut(set.Target, ".")
			if !ok || strings.Contains(attr, ".") {
				continue
			}
			k := key{target, attr}
			if _, seen := given[k]; !seen {
				order = append(order, k)
			}
			// Действует последняя запись — как у ячеек.
			given[k] = set.Value.Text
		}
	}

	type source struct{ from, value, label string }
	carried := map[key][]source{}
	var targets []key
	for _, k := range order {
		for _, rel := range prog.Relations {
			if rel.From != k.target || !carrying[rel.Type] || rel.Label == "" {
				continue
			}
			to := key{rel.To, k.attr}
			if _, set := given[to]; set {
				continue
			}
			if _, seen := carried[to]; !seen {
				targets = append(targets, to)
			}
			carried[to] = append(carried[to], source{k.target, given[k], rel.Label})
		}
	}

	var out Derivation
	for _, to := range targets {
		sources := carried[to]
		var values []string
		for _, s := range sources {
			if !contains(values, s.value) {
				values = append(values, s.value)
			}
		}
		if len(values) == 1 {
			out.Decisions = append(out.Decisions, Decision{Target: to.target, Attr: to.attr, Value: values[0],
				Basis: "relation(" + sources[0].label + ")"})
			continue
		}
		var win *source
		for i := range sources {
			wins := true
			for _, o := range sources {
				if o.value != sources[i].value && !precedes(prog, sources[i].from, o.from) {
					wins = false
					break
				}
			}
			if wins {
				win = &sources[i]
				break
			}
		}
		if win == nil {
			out.Open = append(out.Open, OpenQuestion{Targets: []string{to.target}, Attr: to.attr, Values: values})
			continue
		}
		lose := ""
		for _, o := range sources {
			if o.value != win.value {
				lose = o.from
				break
			}
		}
		d := Decision{Target: to.target, Attr: to.attr, Value: win.value, Basis: fmt.Sprintf("priority(%s < %s)", win.from, lose)}
		if allNames(values) {
			d.Alternatives = values
		}
		out.Decisions = append(out.Decisions, d)
	}

	// По стрелке: обобщение — схема отношения, частный случай — стрелка.
	labels := map[string]bool{}
	for _, rel := range prog.Relations {
		if rel.Label != "" {
			labels[rel.Label] = true
		}
	}
	gens := map[string]*Generalization{}
	for _, g := range prog.Generalizations {
		gens[g.Name] = g
	}
	for _, k := range order {
		g, ok := gens[k.target]
		if !ok || !labels[k.target] || isSlot(g, k.attr) {
			continue
		}
		for _, inst := range prog.Instances {
			if inst.Of.Text != k.target {
				continue
			}
			if _, set := given[key{inst.Name, k.attr}]; set {
				continue
			}
			out.Decisions = append(out.Decisions, Decision{Target: inst.Name, Attr: k.attr, Value: given[k],
				Basis: "relation(" + k.target + ")"})
		}
	}

	// Вопрос отчёта — атрибут: одна запись на все цели с тем же значением.
	// Разные значения одного атрибута одной записью не сказать — такой
	// вопрос уходит оператору.
	byAttr := map[string][]string{}
	for _, d := range out.Decisions {
		if !contains(byAttr[d.Attr], d.Value) {
			byAttr[d.Attr] = append(byAttr[d.Attr], d.Value)
		}
	}
	var kept []Decision
	split := map[string]*OpenQuestion{}
	for _, d := range out.Decisions {
		if len(byAttr[d.Attr]) == 1 {
			kept = append(kept, d)
			continue
		}
		q, ok := split[d.Attr]
		if !ok {
			out.Open = append(out.Open, OpenQuestion{Attr: d.Attr, Values: byAttr[d.Attr]})
			q = &out.Open[len(out.Open)-1]
			split[d.Attr] = q
		}
		q.Targets = append(q.Targets, d.Target)
	}
	out.Decisions = kept
	return out
}

func precedes(prog *Program, a, b string) bool {
	edges := map[string][]string{}
	for _, pr := range prog.Priorities {
		edges[pr.Higher] = append(edges[pr.Higher], pr.Lower)
	}
	seen := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range edges[node] {
			if next == b {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

func isSlot(g *Generalization, name string) bool {
	for _, p := range g.Params {
		if p.Name == name {
			return true
		}
	}
	return false
}

var nameLike = regexp.MustCompile(`^[A-Za-z_]\w*$`)

func allNames(values []string) bool {
	for _, v := range values {
		if !nameLike.MatchString(v) {
			return false
		}
	}
	return true
}

var (
	innerSpaces = regexp.MustCompile(` {2,}`)
	blankRuns   = regexp.MustCompile(`\n{3,}`)
)

// Canonical — каноническая запись текста на ℒ: без комментариев и
// пробелов в конце строк, одиночные пробелы внутри строки, не больше одной
// пустой строки подряд. Так ответ повторяет задачу.
func Canonical(src string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r", ""), "\n") {
		line = strings.TrimRight(stripComment(line), " \t")
		body := strings.TrimLeft(line, " ")
		if body == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, line[:len(line)-len(body)]+innerSpaces.ReplaceAllString(body, " "))
	}
	text := blankRuns.ReplaceAllString(strings.Trim(strings.Join(lines, "\n"), "\n"), "\n\n")
	if text == "" {
		return ""
	}
	return text + "\n"
}

// stripComment снимает комментарий: # вне кавычек.
func stripComment(line string) string {
	quoted := false
	for i, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
		case r == '#' && !quoted:
			return line[:i]
		}
	}
	return line
}

// AnswerPlan — ответ, который строит логика: каноническая копия задачи,
// затем ячейка с выведенными решениями и отчёт. Порядок решений в ячейке
// посылки не задают — его выбирает модель среди допустимых; остальное
// следует из уже написанного.
type AnswerPlan struct {
	Copy string
	Cell string
	Derivation
}

// PlanAnswer — план ответа на промпт src, разобранный в prog.
func PlanAnswer(src string, prog *Program) *AnswerPlan {
	taken := map[string]bool{}
	for _, c := range prog.Cells {
		taken[c.Name] = true
	}
	for _, i := range prog.Instances {
		taken[i.Name] = true
	}
	for _, g := range prog.Generalizations {
		taken[g.Name] = true
	}
	for _, r := range prog.Relations {
		taken[r.Label] = true
	}
	cell := ""
	for k := 1; cell == ""; k++ {
		if name := fmt.Sprintf("c%d", k); !taken[name] {
			cell = name
		}
	}
	return &AnswerPlan{Copy: Canonical(src), Cell: cell, Derivation: Derive(prog)}
}

// Next — допустимые следующие утверждения (с переводом строки) после
// output; пусто — ответ закончен. output начинается с Copy: копию пишет
// сам ochan. ok ложно — output ушёл от плана, и логика это место не
// ведёт.
func (a *AnswerPlan) Next(output string) (adm []string, ok bool) {
	rest, ok := strings.CutPrefix(output, a.Copy)
	if !ok {
		return nil, false
	}
	if len(a.Decisions) == 0 {
		return nil, rest == ""
	}
	lines := strings.SplitAfter(rest, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	switch len(lines) {
	case 0:
		return []string{"\n"}, true
	case 1:
		return []string{"cell " + a.Cell + ":\n"}, true
	}
	i := 2
	var written []Decision
	for ; i < len(lines) && strings.HasPrefix(lines[i], "  set "); i++ {
		d, found := a.decision(lines[i])
		if !found {
			return nil, false
		}
		written = append(written, d)
	}
	if i == len(lines) {
		for _, d := range a.Decisions {
			if !containsDecision(written, d) {
				adm = append(adm, d.Line())
			}
		}
		if len(adm) == 0 {
			adm = []string{"\n"}
		}
		return adm, true
	}
	report := append([]string{"report:\n"}, reportLines(written)...)
	done := len(lines) - i - 1
	if lines[i] != "\n" || done > len(report) {
		return nil, false
	}
	if done == len(report) {
		return nil, true
	}
	return []string{report[done]}, true
}

// Text — ответ целиком, решения — в порядке вывода.
func (a *AnswerPlan) Text() string {
	text := a.Copy
	for {
		adm, ok := a.Next(text)
		if !ok || len(adm) == 0 {
			return text
		}
		text += adm[0]
	}
}

func (a *AnswerPlan) decision(line string) (Decision, bool) {
	for _, d := range a.Decisions {
		if d.Line() == line {
			return d, true
		}
	}
	return Decision{}, false
}

func containsDecision(list []Decision, d Decision) bool {
	for _, x := range list {
		if x.Target == d.Target && x.Attr == d.Attr {
			return true
		}
	}
	return false
}

// reportLines — записи report: в порядке первого решения по атрибуту.
func reportLines(decisions []Decision) []string {
	var lines []string
	seen := map[string]bool{}
	for _, d := range decisions {
		if seen[d.Attr] {
			continue
		}
		seen[d.Attr] = true
		lines = append(lines, "  q: "+d.Attr+"\n", "    choice: "+d.Value+"\n")
		if len(d.Alternatives) > 0 {
			lines = append(lines, "    alternatives: {"+strings.Join(d.Alternatives, ", ")+"}\n")
		}
		lines = append(lines, "    basis: "+d.Basis+"\n")
	}
	return lines
}
