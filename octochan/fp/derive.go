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
//   - перенос — цепочкой: выведенное значение — посылка для следующего
//     шага (A → B → C). Цель решается на первом шаге, на котором до неё
//     дошёл перенос; другой путь — длиннее или по кругу — с другим
//     значением, которое приоритет не снимает, делает её вопросом
//     оператору, и от вопроса дальше ничего не переносится;
//   - два источника дают цели разные значения — решает приоритет (A < B:
//     побеждает A); без приоритета это открытый вопрос оператору (П5);
//   - атрибут, заданный обобщению — схеме отношения, переходит на каждую
//     его стрелку-частный случай, у которой его нет; атрибут, совпадающий
//     со слотом, — нет: значение слота задаёт сама стрелка. Обратно не
//     переходит ничего;
//   - значение, заданное промптом, не переписывается;
//   - неточная предпосылка (set T.a = {x, y} или 100..500, раздел 5.2)
//     сама ничего не переносит. Её сужает точная посылка через отношение:
//     перенос на T.a значения, которое лежит внутри неточного, — решение с
//     альтернативами-вариантами; значение вне — вопрос оператору. Сузить
//     её по истории может только модель, у которой есть журнал
//     (AnswerPlan.Narrowings проверяет такое сужение).

// carrying — типы отношений, которые переносят атрибут источника на цель.
var carrying = map[string]bool{"defines": true, "limits": true, "modifies": true}

// Decision — решение, выведенное из посылок: set Target.Attr = Value.
type Decision struct {
	Target, Attr, Value string
	// Alternatives — значения источников конфликта, решённого приоритетом.
	Alternatives []string
	// Basis — основание в отчёте: relation(имя) или priority(A < B).
	Basis string
	// Rules — правила вывода всего пути: перенос цепочкой — fp.<тип> на
	// каждом шаге, по стрелке — fp.arrow. Кольцо 1 перемножает их
	// надёжность.
	Rules []string
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
	given := map[key]Value{}
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
			given[k] = set.Value
		}
	}
	// exact — точная посылка: только она переносится и только её не
	// переписывают; неточную можно сузить.
	exact := func(k key) bool {
		v, ok := given[k]
		return ok && !v.Inexact()
	}

	// narrow — решение о неточной цели: значение внутри неё, альтернативы —
	// её варианты. Вне — нет решения.
	narrow := func(to key, d Decision) (Decision, bool) {
		premise, ok := given[to]
		if !ok {
			return d, true
		}
		if !premise.Admits(d.Value) {
			return d, false
		}
		if len(premise.Variants) > 0 && allNames(premise.Variants) {
			d.Alternatives = premise.Variants
		}
		return d, true
	}

	type source struct {
		from, value, label, kind string
		rules                    []string
	}
	// path — правила пути решения: путь источника и шаг переноса.
	path := func(s source) []string { return append(append([]string(nil), s.rules...), "fp."+s.kind) }
	distinct := func(sources []source) []string {
		var values []string
		for _, s := range sources {
			if !contains(values, s.value) {
				values = append(values, s.value)
			}
		}
		return values
	}
	// resolve — решение цели по её источникам или вопрос оператору.
	resolve := func(to key, sources []source) (Decision, *OpenQuestion) {
		values := distinct(sources)
		outside := &OpenQuestion{Targets: []string{to.target}, Attr: to.attr,
			Values: append(append([]string(nil), values...), given[to].Text)}
		if len(values) == 1 {
			d, ok := narrow(to, Decision{Target: to.target, Attr: to.attr, Value: values[0], Basis: "relation(" + sources[0].label + ")",
				Rules: path(sources[0])})
			if !ok {
				return Decision{}, outside
			}
			return d, nil
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
			return Decision{}, &OpenQuestion{Targets: []string{to.target}, Attr: to.attr, Values: values}
		}
		lose := ""
		for _, o := range sources {
			if o.value != win.value {
				lose = o.from
				break
			}
		}
		// Приоритет — посылка оператора, точная: правило шага одно — перенос.
		d := Decision{Target: to.target, Attr: to.attr, Value: win.value, Basis: fmt.Sprintf("priority(%s < %s)", win.from, lose),
			Rules: path(*win)}
		if allNames(values) {
			d.Alternatives = values
		}
		if d, ok := narrow(to, d); ok {
			return d, nil
		}
		return Decision{}, outside
	}

	// Перенос по шагам: шаг — посылки и решения прошлого шага. blocked —
	// цели, которым разные пути дают разные значения: вопросы оператору, от
	// них ничего не переносится. С каждым кругом blocked растёт, поэтому
	// вывод кончается и на отношениях по кругу.
	blocked := map[key][]string{}
	var out Derivation
	for {
		decided := map[key]Decision{}
		valueOf := func(k key) (string, []string, bool) {
			if exact(k) {
				return given[k].Text, nil, true
			}
			d, ok := decided[k]
			return d.Value, d.Rules, ok
		}
		var decisions []Decision
		var open []OpenQuestion
		visited := map[key]bool{}
		var frontier []key
		for _, k := range order {
			if exact(k) {
				frontier = append(frontier, k)
			}
		}
		for len(frontier) > 0 {
			carried := map[key][]source{}
			var targets []key
			for _, k := range frontier {
				value, rules, _ := valueOf(k)
				for _, rel := range prog.Relations {
					if rel.From != k.target || !carrying[rel.Type] || rel.Label == "" {
						continue
					}
					to := key{rel.To, k.attr}
					if exact(to) || visited[to] {
						continue
					}
					if _, seen := carried[to]; !seen {
						targets = append(targets, to)
					}
					carried[to] = append(carried[to], source{k.target, value, rel.Label, rel.Type, rules})
				}
			}
			frontier = nil
			for _, to := range targets {
				visited[to] = true
				if values, ok := blocked[to]; ok {
					open = append(open, OpenQuestion{Targets: []string{to.target}, Attr: to.attr, Values: values})
					continue
				}
				d, q := resolve(to, carried[to])
				if q != nil {
					open = append(open, *q)
					continue
				}
				decided[to] = d
				decisions = append(decisions, d)
				frontier = append(frontier, to)
			}
		}
		// Другие пути: со всеми источниками — посылками и решениями всех
		// шагов — цель решилась бы иначе или не решилась бы вовсе.
		again := false
		for _, d := range decisions {
			to := key{d.Target, d.Attr}
			var all []source
			for _, rel := range prog.Relations {
				if rel.To != to.target || !carrying[rel.Type] || rel.Label == "" {
					continue
				}
				if value, rules, ok := valueOf(key{rel.From, to.attr}); ok {
					all = append(all, source{rel.From, value, rel.Label, rel.Type, rules})
				}
			}
			other, q := resolve(to, all)
			switch {
			case q != nil:
				blocked[to], again = q.Values, true
			case other.Value != d.Value:
				blocked[to], again = distinct(all), true
			}
		}
		if !again {
			out.Decisions, out.Open = decisions, open
			break
		}
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
		if !ok || !labels[k.target] || isSlot(g, k.attr) || !exact(k) {
			continue
		}
		for _, inst := range prog.Instances {
			to := key{inst.Name, k.attr}
			if inst.Of.Text != k.target || exact(to) {
				continue
			}
			if d, ok := narrow(to, Decision{Target: inst.Name, Attr: k.attr, Value: given[k].Text,
				Basis: "relation(" + k.target + ")", Rules: []string{"fp.arrow"}}); ok {
				out.Decisions = append(out.Decisions, d)
			}
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
	// Inexact — неточные предпосылки задачи: Цель.атрибут → значение.
	Inexact map[string]Value
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
	inexact := map[string]Value{}
	for _, c := range prog.Cells {
		for _, set := range c.Sets {
			if set.Value.Inexact() {
				inexact[set.Target] = set.Value
			} else {
				delete(inexact, set.Target)
			}
		}
	}
	return &AnswerPlan{Copy: Canonical(src), Cell: cell, Derivation: Derive(prog), Inexact: inexact}
}

var setLine = regexp.MustCompile(`^set (\w+)\.(\w+) = (.+)$`)

// Narrowings — сужения неточных предпосылок, которые архитектурная модель
// сделала по своему источнику — истории из журнала (basis: history):
// ochan проверяет только допустимость — цель задана неточно, значение
// внутри, логика ochan её не сузила. Поддержку сужения считает модель.
func (a *AnswerPlan) Narrowings(statements []string) ([]Decision, error) {
	var out []Decision
	for _, st := range statements {
		m := setLine.FindStringSubmatch(strings.TrimSpace(st))
		if m == nil {
			return nil, fmt.Errorf("сужение %q — не утверждение set", st)
		}
		premise, ok := a.Inexact[m[1]+"."+m[2]]
		switch {
		case !ok:
			return nil, fmt.Errorf("сужение %q: %s.%s в задаче не задан неточно", st, m[1], m[2])
		case !premise.Admits(m[3]):
			return nil, fmt.Errorf("сужение %q вне %s", st, premise.Text)
		}
		for _, d := range a.Decisions {
			switch {
			case d.Target == m[1] && d.Attr == m[2]:
				return nil, fmt.Errorf("сужение %q: %s.%s уже сужен посылками — %s", st, m[1], m[2], d.Basis)
			case d.Attr == m[2] && d.Value != m[3]:
				return nil, fmt.Errorf("сужение %q: вопрос отчёта %s уже решён иначе — %s.%s = %s", st, m[2], d.Target, d.Attr, d.Value)
			}
		}
		d := Decision{Target: m[1], Attr: m[2], Value: m[3], Basis: "history(" + m[2] + ")", Rules: []string{"fp.history"}}
		if len(premise.Variants) > 0 && allNames(premise.Variants) {
			d.Alternatives = premise.Variants
		}
		out = append(out, d)
	}
	return out, nil
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

// Verify — ответ архитектурной модели допустим логикой ochan: начинается
// копией задачи, каждое следующее утверждение — среди допустимых, ответ
// кончается там, где кончается план. omitted — решения (утверждения set),
// которые модель не записала, потому что её кольцо 2 их не приняло: их
// план не ждёт. narrowed — её сужения по истории (Narrowings). Проверка
// внешняя — логика модели своя, в её коде.
func (a *AnswerPlan) Verify(answer string, omitted []string, narrowed []Decision) error {
	plan := *a
	plan.Decisions = append([]Decision(nil), narrowed...)
	for _, d := range a.Decisions {
		if !contains(omitted, strings.TrimSpace(d.Line())) {
			plan.Decisions = append(plan.Decisions, d)
		}
	}
	if !strings.HasPrefix(answer, plan.Copy) {
		return fmt.Errorf("ответ не начинается копией задачи")
	}
	text := plan.Copy
	for _, line := range strings.SplitAfter(answer[len(plan.Copy):], "\n") {
		if line == "" {
			continue
		}
		adm, ok := plan.Next(text)
		if !ok || !contains(adm, line) {
			return fmt.Errorf("утверждения %q логика не допускает", strings.TrimRight(line, "\n"))
		}
		text += line
	}
	if adm, ok := plan.Next(text); !ok || len(adm) > 0 {
		return fmt.Errorf("ответ кончился раньше плана: дальше допустимо %q", adm)
	}
	return nil
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
