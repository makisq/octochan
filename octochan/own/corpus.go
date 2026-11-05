package own

import (
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ochan/arbiter"
	"ochan/fp"
)

// Корпус ℒ для обучения своей модели.
//
// Пара — задача оператора и ответ модели, оба на формальном промпте:
//
//   - задача: цель, классы (смысл слова из словаря), отношения из словаря R
//     с именами из слов, иногда приоритет и жёсткое правило, ячейка c1,
//     обобщение — схема одного отношения — со стрелками-частными случаями,
//     неточные предпосылки, скилл с операциями;
//   - ответ: та же программа, закрытая там, где задача оставила открытым:
//     ячейка с решениями и отчёт. Ответ выводит логика ochan
//     (fp.PlanAnswer) — та же, что у модели при ответе: второй логики для
//     корпуса нет.
//
// Задача без предпосылки (⊥undef) — близнец решаемой: та же задача без
// одной предпосылки, часто с именем, похожим на настоящее (fault_error
// вместо fault_objective), — различающий пример (H3). Её ответ — задача
// без единого решения.
//
// Пары синтетические: они учат языку, форме ответа и выводу по
// отношениям, а не предметной области. Каждую проверяет разбор ochan:
// задача — без отказов, ответ — строже, как текст модели; у близнеца без
// предпосылки отказы — ровно ⊥undef на каждую недостающую. Непрошедшие
// отбрасываются.

var (
	corpusRelations = []string{"defines", "forces", "limits", "modifies", "precedes", "terminates"}
	corpusCarries   = map[string]bool{"defines": true, "limits": true, "modifies": true}
	corpusUnits     = []string{"MB", "GB", "s", "ms", "%", "m"}
	corpusBuiltins  = map[string]bool{"exec": true, "save": true, "write": true}
	corpusVersions  = []string{"v1", "v2", "v3", "v1.2", "v2.3"}
	corpusComment   = regexp.MustCompile(`\s*#.*$`)
	corpusBlank     = regexp.MustCompile(`\n{3,}`)
	corpusNumber    = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([A-Za-z%][\w/%]*)?$`)
)

// CorpusOptions — сколько пар и какие.
type CorpusOptions struct {
	Count int
	Seed  int64
	// Test — доля отложенных пар.
	Test float64
	// H3 — доля задач с решениями, у которых есть близнец без предпосылки
	// (у задач без решений — треть её).
	H3 float64
	// Lexicon — словарь W; nil — базовый.
	Lexicon *fp.Lexicon
}

// Pair — пара корпуса.
type Pair struct {
	Task   string `json:"task"`
	Answer string `json:"answer"`
	Split  string `json:"split"`
	// IDs и Start — токены пары словарём модели (<bos> задача <answer> ответ
	// <eos>) и где начинается ответ: так их читает обучение.
	IDs   []int `json:"ids,omitempty"`
	Start int   `json:"start,omitempty"`
}

// CorpusReport — что вышло.
type CorpusReport struct {
	Generated int
	Train     int
	Kinds     map[string]int
	Knowledge map[string]int
	Failures  map[string]int
}

type corpusLexicon struct {
	words   []string
	single  map[string][]string
	classes []string
	units   []string
}

func newCorpusLexicon(lex *fp.Lexicon) *corpusLexicon {
	if lex == nil {
		lex = fp.BaseLexicon()
	}
	c := &corpusLexicon{single: map[string][]string{}}
	c.words = append(c.words, lex.Words()...)
	sort.Strings(c.words)
	for _, w := range c.words {
		if e, ok := lex.Lookup(w); ok && len(e.Senses) == 1 {
			c.single[e.Senses[0]] = append(c.single[e.Senses[0]], w)
		}
	}
	for sense, words := range c.single {
		if len(words) > 0 {
			c.classes = append(c.classes, sense)
		}
	}
	sort.Strings(c.classes)
	for _, u := range lex.Units() {
		for _, want := range corpusUnits {
			if u == want {
				c.units = append(c.units, u)
			}
		}
	}
	if len(c.units) == 0 {
		c.units = corpusUnits
	}
	return c
}

type corpusGen struct {
	rng *rand.Rand
	lex *corpusLexicon
}

func (g *corpusGen) choice(list []string) string { return list[g.rng.Intn(len(list))] }

func (g *corpusGen) sample(list []string, k int) []string {
	cp := append([]string(nil), list...)
	g.rng.Shuffle(len(cp), func(i, j int) { cp[i], cp[j] = cp[j], cp[i] })
	return cp[:min(k, len(cp))]
}

func (g *corpusGen) between(lo, hi int) int { return lo + g.rng.Intn(hi-lo+1) }

// name — имя из слов словаря через _, ещё не занятое.
func (g *corpusGen) name(used map[string]bool, parts int) string {
	for i := 0; i < 100; i++ {
		candidate := strings.Join(g.sample(g.lex.words, parts), "_")
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
	panic("не нашлось свободного имени")
}

// particular — имя частного случая: не слово языка и не составное из слов
// (plodu2222_ru). Определения у него нет — смысл даёт стрелка.
func (g *corpusGen) particular(used map[string]bool) string {
	for {
		var stem strings.Builder
		for i := g.between(2, 3); i > 0; i-- {
			stem.WriteByte("bdfgklmnprstvz"[g.rng.Intn(14)])
			stem.WriteByte("aeiou"[g.rng.Intn(5)])
		}
		candidate := stem.String() + strconv.Itoa(g.between(1, 9999))
		if g.rng.Float64() < 0.4 {
			candidate += "_" + g.choice([]string{"ru", "io", "loc", "lan"})
		}
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// arrow — имя стрелки — непрозрачная ссылка (h121151).
func (g *corpusGen) arrow(used map[string]bool) string {
	for {
		candidate := string("hkmprtx"[g.rng.Intn(7)]) + strconv.Itoa(g.between(1000, 999999))
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

func (g *corpusGen) value() string {
	if g.rng.Float64() < 0.25 {
		return g.choice(g.lex.words)
	}
	n := []int{1, 2, 3, 4, 5, 8, 10, 16, 20, 30, 32, 50, 64, 100, 128, 200, 256, 300, 400, 500, 1000}[g.rng.Intn(21)]
	if g.rng.Float64() < 0.6 {
		return fmt.Sprintf("%d %s", n, g.choice(g.lex.units))
	}
	return strconv.Itoa(n)
}

// vague — неточное значение: варианты-слова или диапазон. v — значение
// источника: inside — оно лежит внутри, иначе — вне; пусто — источника нет.
func (g *corpusGen) vague(v string, inside bool) string {
	m := corpusNumber.FindStringSubmatch(v)
	if m != nil || v == "" && g.rng.Float64() < 0.4 {
		var n int
		var unit string
		if m != nil {
			f, _ := strconv.ParseFloat(m[1], 64)
			n, unit = int(f), m[2]
		} else {
			n = []int{10, 50, 100, 200}[g.rng.Intn(4)]
			unit = []string{"", g.choice(g.lex.units)}[g.rng.Intn(2)]
		}
		lo, hi := max(0, n/2), n*2+1
		if !inside {
			lo, hi = n+1, n*3+2
		}
		out := fmt.Sprintf("%d..%d", lo, hi)
		if unit != "" {
			out += " " + unit
		}
		return out
	}
	var others []string
	for _, w := range g.sample(g.lex.words, 4) {
		if w != v {
			others = append(others, w)
		}
	}
	others = others[:min(len(others), g.between(1, 3))]
	var variants []string
	if inside {
		variants = append(variants, v)
	}
	variants = append(variants, others...)
	if len(variants) < 2 {
		for _, w := range g.lex.words {
			if !containsString(variants, w) && w != v {
				variants = append(variants, w)
				break
			}
		}
	}
	g.rng.Shuffle(len(variants), func(i, j int) { variants[i], variants[j] = variants[j], variants[i] })
	return "{" + strings.Join(variants, ", ") + "}"
}

type corpusRelation struct{ from, kind, to, label string }
type corpusSet struct{ target, attr, value string }
type corpusInstance struct {
	name, of string
	values   [][2]string // слот → значение по порядку
}
type corpusGeneralization struct {
	name, source, result string
	slots                [2]string
	words                []string // домен слота: слова
	lo, hi               int      // или диапазон
	op                   string
	instances            []*corpusInstance
}
type corpusSkill struct {
	name, version string
	classes, ops  []string
	needs         []string
}
type corpusTask struct {
	classes   []string
	words     map[string]string
	goal      string
	relations []corpusRelation
	priority  []string
	gen       *corpusGeneralization
	skill     *corpusSkill
	requires  []string
	rule      *[2]string
	sets      []corpusSet
}

// task — случайная задача на ℒ.
func (g *corpusGen) task() *corpusTask {
	used, attrs := map[string]bool{}, map[string]bool{}
	t := &corpusTask{words: map[string]string{}}
	t.classes = g.sample(g.lex.classes, g.between(3, 5))
	for _, c := range t.classes {
		t.words[c] = g.choice(g.lex.single[c])
	}
	t.goal = t.classes[len(t.classes)-1]
	type rel struct{ from, kind, to string }
	var rels []rel
	for i := 0; i+1 < len(t.classes); i++ {
		kind := g.choice(corpusRelations)
		if t.classes[i+1] == t.goal {
			kind = "strives_to"
		}
		rels = append(rels, rel{t.classes[i], kind, t.classes[i+1]})
	}
	// Второй источник у той же цели: из него бывает конфликт.
	var conflict []string
	if len(t.classes) >= 4 && g.rng.Float64() < 0.5 {
		target := g.choice(t.classes[2 : len(t.classes)-1])
		at := indexOf(t.classes, target)
		source := g.choice(t.classes[:at-1])
		carriers := []string{"defines", "limits", "modifies"}
		rels = append(rels, rel{source, g.choice(carriers), target})
		var before, beforeKind string
		for _, r := range rels {
			if r.to == target {
				before, beforeKind = r.from, r.kind
				break
			}
		}
		if g.rng.Float64() < 0.7 && corpusCarries[beforeKind] {
			conflict = []string{before, source}
		}
	}
	for _, r := range rels {
		label := ""
		if r.kind != "strives_to" {
			label = g.name(used, 2)
		}
		t.relations = append(t.relations, corpusRelation{r.from, r.kind, r.to, label})
	}
	switch {
	case conflict != nil && g.rng.Float64() < 0.85:
		t.priority = append([]string(nil), conflict...)
		g.rng.Shuffle(2, func(i, j int) { t.priority[i], t.priority[j] = t.priority[j], t.priority[i] })
	case g.rng.Float64() < 0.4:
		t.priority = g.sample(t.classes[:len(t.classes)-1], 2)
	}
	if conflict != nil {
		// Оба источника задают один атрибут, разными значениями.
		attr := g.name(attrs, 1)
		two := g.sample(g.lex.words, 2)
		t.sets = append(t.sets, corpusSet{conflict[0], attr, two[0]}, corpusSet{conflict[1], attr, two[1]})
	}
	for n := g.between(1, 2); n > 0; n-- {
		// Чаще — источнику отношения, которое переносит атрибут.
		var carriers []string
		for _, r := range t.relations {
			if corpusCarries[r.kind] {
				carriers = append(carriers, r.from)
			}
		}
		target := g.choice(t.classes[:len(t.classes)-1])
		if len(carriers) > 0 && g.rng.Float64() < 0.7 {
			target = g.choice(carriers)
		}
		t.sets = append(t.sets, corpusSet{target, g.name(attrs, 1), g.value()})
	}
	// Неточная предпосылка (раздел 5.2) — чаще на цели переноса: значение
	// источника внутри неё её сужает, вне — вопрос оператору; без источника
	// её сужает только история.
	if g.rng.Float64() < 0.3 {
		given := map[[2]string]bool{}
		for _, s := range t.sets {
			given[[2]string{s.target, s.attr}] = true
		}
		var carried []corpusSet
		for _, r := range t.relations {
			if !corpusCarries[r.kind] || r.label == "" {
				continue
			}
			for _, s := range t.sets {
				if s.target == r.from && !given[[2]string{r.to, s.attr}] {
					carried = append(carried, corpusSet{r.to, s.attr, s.value})
				}
			}
		}
		if len(carried) > 0 && g.rng.Float64() < 0.8 {
			c := carried[g.rng.Intn(len(carried))]
			t.sets = append(t.sets, corpusSet{c.target, c.attr, g.vague(c.value, g.rng.Float64() < 0.7)})
		} else {
			t.sets = append(t.sets, corpusSet{g.choice(t.classes[:len(t.classes)-1]), g.name(attrs, 1), g.vague("", false)})
		}
	}
	// Обобщение — схема одного из отношений, частные случаи — её стрелки.
	var labeled []corpusRelation
	for _, r := range t.relations {
		if r.label != "" {
			labeled = append(labeled, r)
		}
	}
	if len(labeled) > 0 && g.rng.Float64() < 0.5 {
		r := labeled[g.rng.Intn(len(labeled))]
		slots := map[string]bool{}
		gen := &corpusGeneralization{name: r.label, source: r.from, result: r.to, slots: [2]string{g.name(slots, 1), g.name(slots, 1)}}
		if g.rng.Float64() < 0.5 {
			gen.words = g.sample(g.lex.words, g.between(2, 4))
			sort.Strings(gen.words)
		} else {
			gen.lo = []int{1, 2, 5, 10, 20, 50, 100}[g.rng.Intn(7)]
			gen.hi = gen.lo * []int{2, 4, 5, 10}[g.rng.Intn(4)]
		}
		gen.op = g.name(used, 2)
		for n := []int{1, 1, 2, 2, 3}[g.rng.Intn(5)]; n > 0; n-- {
			v := strconv.Itoa(g.between(gen.lo, gen.hi))
			if gen.words != nil {
				v = g.choice(gen.words)
			}
			gen.instances = append(gen.instances, &corpusInstance{name: g.arrow(used), of: r.label,
				values: [][2]string{{gen.slots[0], g.particular(used)}, {gen.slots[1], v}}})
		}
		var arrows []string
		for _, i := range gen.instances {
			arrows = append(arrows, i.name)
		}
		if g.rng.Float64() < 0.75 {
			attr := g.name(attrs, 1)
			t.sets = append(t.sets, corpusSet{r.label, attr, g.value()})
			if g.rng.Float64() < 0.3 {
				// Своё значение у стрелки: точная посылка о частном случае.
				t.sets = append(t.sets, corpusSet{g.choice(arrows), attr, g.value()})
			}
		}
		if g.rng.Float64() < 0.3 {
			// Атрибут стрелки не переходит ни на обобщение, ни на соседей.
			t.sets = append(t.sets, corpusSet{g.choice(arrows), g.name(attrs, 1), g.value()})
		}
		t.gen = gen
	}
	// Знание задачи: скилл, требования, правило о его операции. Операцию
	// обобщения даёт скилл (П5): с обобщением скилл есть всегда.
	ownSkill := g.rng.Float64() < 0.45
	if ownSkill || t.gen != nil {
		s := &corpusSkill{name: g.name(used, 1), version: g.choice(corpusVersions), classes: g.sample(t.classes, g.between(1, 2))}
		if ownSkill {
			for n := g.between(1, 2); n > 0; n-- {
				s.ops = append(s.ops, g.name(used, 2))
			}
		}
		if g.rng.Float64() < 0.5 {
			s.needs = []string{g.choice(t.classes)}
		}
		if t.gen != nil {
			s.ops = append(s.ops, t.gen.op)
		}
		t.skill = s
	}
	known := t.known()
	if g.rng.Float64() < 0.45 && len(known) > 0 {
		t.requires = g.sample(known, min(len(known), g.between(1, 2)))
	}
	if g.rng.Float64() < 0.4 {
		action := g.choice([]string{"write", "save"})
		if t.skill != nil && len(t.skill.ops) > 0 && g.rng.Float64() < 0.6 {
			action = g.choice(t.skill.ops)
		}
		t.rule = &[2]string{action, g.name(used, 2)}
	}
	return t
}

// known — знание задачи K: имена отношений, операции скилла, стрелки.
func (t *corpusTask) known() []string {
	var k []string
	for _, r := range t.relations {
		if r.label != "" {
			k = append(k, r.label)
		}
	}
	if t.skill != nil {
		k = append(k, t.skill.ops...)
	}
	if t.gen != nil {
		for _, i := range t.gen.instances {
			k = append(k, i.name)
		}
	}
	return k
}

// render — текст задачи.
func (t *corpusTask) render() string {
	width := 0
	for _, c := range t.classes {
		width = max(width, len(c))
	}
	pad := func(s string) string { return s + strings.Repeat(" ", width-len(s)) }
	lines := []string{"goal: " + t.words[t.goal], "", "classes:"}
	for _, c := range t.classes {
		lines = append(lines, fmt.Sprintf("  %s : %q", pad(c), t.words[c]))
	}
	lines = append(lines, "", "relations:")
	for _, r := range t.relations {
		if r.label == "" {
			lines = append(lines, fmt.Sprintf("  %s --%s--> %s", pad(r.from), r.kind, r.to))
		} else {
			lines = append(lines, fmt.Sprintf("  %s --%s--> %s (%s)", pad(r.from), r.kind, pad(r.to), r.label))
		}
	}
	if t.priority != nil {
		lines = append(lines, "", "priority:", fmt.Sprintf("  %s < %s", t.priority[0], t.priority[1]))
	}
	if gen := t.gen; gen != nil {
		spec := fmt.Sprintf("%d..%d", gen.lo, gen.hi)
		if gen.words != nil {
			spec = "{" + strings.Join(gen.words, ", ") + "}"
		}
		lines = append(lines, "", fmt.Sprintf("generalization %s(%s : %s, %s : %s) :", gen.name, gen.slots[0], gen.source, gen.slots[1], spec),
			fmt.Sprintf("  %s = %s(%s, %s)", gen.result, gen.op, gen.slots[0], gen.slots[1]), "")
		for _, i := range gen.instances {
			var bound []string
			for _, v := range i.values {
				bound = append(bound, v[0]+" := "+v[1])
			}
			lines = append(lines, fmt.Sprintf("instance %s = %s(%s)", i.name, i.of, strings.Join(bound, ", ")))
		}
	}
	if s := t.skill; s != nil {
		lines = append(lines, "", "import @"+s.name+" "+s.version, "  provides: "+strings.Join(append(append([]string(nil), s.classes...), s.ops...), ", "))
		if len(s.needs) > 0 {
			lines = append(lines, "  requires_class: "+strings.Join(s.needs, ", "))
		}
	}
	if len(t.requires) > 0 {
		lines = append(lines, "", "requires: "+strings.Join(t.requires, ", "))
	}
	if t.rule != nil {
		lines = append(lines, "", fmt.Sprintf("hard forbid %s(except = {%s})", t.rule[0], t.rule[1]))
	}
	lines = append(lines, "", "cell c1:")
	for _, s := range t.sets {
		lines = append(lines, fmt.Sprintf("  set %s.%s = %s", s.target, s.attr, s.value))
	}
	return strings.Join(lines, "\n") + "\n"
}

func (t *corpusTask) clone() *corpusTask {
	c := *t
	c.classes = append([]string(nil), t.classes...)
	c.relations = append([]corpusRelation(nil), t.relations...)
	c.requires = append([]string(nil), t.requires...)
	c.sets = append([]corpusSet(nil), t.sets...)
	if t.rule != nil {
		r := *t.rule
		c.rule = &r
	}
	if t.skill != nil {
		s := *t.skill
		s.classes = append([]string(nil), t.skill.classes...)
		s.ops = append([]string(nil), t.skill.ops...)
		s.needs = append([]string(nil), t.skill.needs...)
		c.skill = &s
	}
	if t.gen != nil {
		gen := *t.gen
		gen.instances = nil
		for _, i := range t.gen.instances {
			ci := *i
			ci.values = append([][2]string(nil), i.values...)
			gen.instances = append(gen.instances, &ci)
		}
		c.gen = &gen
	}
	return &c
}

// absent — имя, которого нет в K: чаще — настоящее (like, иначе любое
// составное из K) с одним словом не тем, иначе — новое.
func (g *corpusGen) absent(known map[string]bool, like string) string {
	var compounds []string
	if like != "" {
		compounds = []string{like}
	} else {
		for k := range known {
			if strings.Contains(k, "_") {
				compounds = append(compounds, k)
			}
		}
		sort.Strings(compounds)
	}
	for i := 0; i < 100; i++ {
		var candidate string
		if len(compounds) > 0 && g.rng.Float64() < 0.6 {
			parts := strings.Split(g.choice(compounds), "_")
			parts[g.rng.Intn(len(parts))] = g.choice(g.lex.words)
			candidate = strings.Join(parts, "_")
		} else {
			candidate = strings.Join(g.sample(g.lex.words, 2), "_")
		}
		if !known[candidate] && !corpusBuiltins[candidate] {
			return candidate
		}
	}
	panic("не нашлось имени вне K")
}

// lacking — близнец задачи без одной предпосылки (H3): требование или
// действие правила вне K, класс скилла не из classes:, стрелка к
// необъявленному обобщению или с незаполненным слотом, операция обобщения
// не из скилла.
func (g *corpusGen) lacking(base *corpusTask) *corpusTask {
	t := base.clone()
	known := map[string]bool{}
	for _, k := range t.known() {
		known[k] = true
	}
	kinds := []string{"requires", "rule"}
	if t.skill != nil {
		kinds = append(kinds, "class")
	}
	if t.gen != nil {
		kinds = append(kinds, "gen", "slot", "op")
	}
	switch g.choice(kinds) {
	case "gen":
		t.gen.instances[g.rng.Intn(len(t.gen.instances))].of = g.absent(known, t.gen.name)
	case "slot":
		i := t.gen.instances[g.rng.Intn(len(t.gen.instances))]
		k := g.rng.Intn(len(i.values))
		i.values = append(i.values[:k:k], i.values[k+1:]...)
	case "op":
		known[t.gen.op] = true
		t.gen.op = g.absent(known, "")
	case "class":
		var other []string
		for _, c := range g.lex.classes {
			if !containsString(t.classes, c) {
				other = append(other, c)
			}
		}
		pick := g.choice(other)
		if g.rng.Float64() < 0.5 {
			t.skill.needs = []string{pick}
		} else {
			t.skill.classes = append(t.skill.classes, pick)
		}
	case "rule":
		except := ""
		if t.rule != nil {
			except = t.rule[1]
		} else {
			except = g.name(known, 2)
		}
		t.rule = &[2]string{g.absent(known, ""), except}
	default:
		if len(t.requires) == 0 {
			t.requires = []string{""}
		}
		t.requires[g.rng.Intn(len(t.requires))] = g.absent(known, "")
	}
	return t
}

// copyOf — задача, как её повторяет ответ: без комментариев и лишних
// пустых строк.
func copyOf(text string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		lines = append(lines, strings.TrimRight(corpusComment.ReplaceAllString(line, ""), " \t"))
	}
	return corpusBlank.ReplaceAllString(strings.Trim(strings.Join(lines, "\n"), "\n")+"\n", "\n\n")
}

// corpusOptions — проверка, которой ochan судит задачу: встроенные действия
// арбитра известны без скиллов.
func corpusOptions(lex *fp.Lexicon) fp.Options {
	return fp.Options{Lexicon: lex, Builtins: arbiter.Builtins}
}

// undef — недостающие предпосылки задачи (⊥undef).
func undef(r *fp.Result) int {
	n := 0
	for _, d := range r.Diagnostics {
		if d.Code == fp.ErrUndef {
			n++
		}
	}
	return n
}

// Solve — ответ на задачу: задача без комментариев, решения и отчёт — их
// выводит логика ochan. Без предпосылки решений нет: модель не решает за
// оператора. Два значения одного вопроса отчёта одной записью не сказать —
// такой задаче ответа нет (ok ложно).
func Solve(text string, opts fp.Options) (answer string, ok bool) {
	cp := copyOf(text)
	r := fp.CheckText("task", text, opts)
	if r.Program == nil || len(r.Syntax()) > 0 {
		return "", false
	}
	if undef(r) > 0 {
		return cp, true
	}
	plan := fp.PlanAnswer(text, r.Program)
	for _, q := range plan.Open {
		if len(q.Targets) > 1 {
			return "", false
		}
	}
	return strings.TrimRight(cp, "\n") + plan.Text()[len(plan.Copy)-1:], true
}

// pairKind — вид пары для сводки.
func pairKind(task, answer string, opts fp.Options) string {
	r := fp.CheckText("task", task, opts)
	if undef(r) > 0 {
		return "без предпосылки"
	}
	made := answer[min(len(copyOf(task)), len(answer)):]
	plan := fp.PlanAnswer(task, r.Program)
	if len(plan.Inexact) > 0 {
		for key := range plan.Inexact {
			if strings.Contains(made, "set "+key+" ") {
				return "неточное: сужено"
			}
		}
		return "неточное: вопрос"
	}
	if strings.Contains(answer, "priority(") {
		return "конфликт"
	}
	arrows := map[string]bool{}
	for _, i := range r.Program.Instances {
		arrows[i.Name] = true
	}
	onArrow, other := false, false
	for _, m := range regexp.MustCompile(`set (\w+)\.`).FindAllStringSubmatch(made, -1) {
		if arrows[m[1]] {
			onArrow = true
		} else {
			other = true
		}
	}
	switch {
	case onArrow && !other:
		return "по стрелке"
	case onArrow:
		return "перенос и по стрелке"
	case other:
		return "перенос"
	}
	return "без решений"
}

// checkPair — пара принята разбором ochan: без предпосылки — ровно ⊥undef
// на каждую недостающую у задачи и у ответа; иначе — без отказов, ответ
// строже, как текст модели. Причина отказа — первая находка.
func checkPair(task, answer string, opts fp.Options) (bool, string) {
	rt := fp.CheckText("task", task, opts)
	ra := fp.CheckModelText("answer", answer, opts)
	lack := undef(rt)
	for _, r := range []*fp.Result{rt, ra} {
		bad := 0
		first := ""
		for _, d := range r.Diagnostics {
			if !d.IsError() {
				continue
			}
			if lack > 0 && d.Code == fp.ErrUndef {
				continue
			}
			bad++
			if first == "" {
				first = d.Msg
			}
		}
		if lack > 0 && (len(r.Syntax()) > 0 || undef(r) != lack) {
			if first == "" {
				first = fmt.Sprintf("ожидался ⊥undef %d, а их %d", lack, undef(r))
			}
			return false, first
		}
		if bad > 0 {
			return false, first
		}
	}
	return true, ""
}

// Corpus — пары корпуса. Группа — задача и её близнец без предпосылки:
// группа целиком либо в обучении, либо в отложенных.
func Corpus(o CorpusOptions) ([]Pair, CorpusReport) {
	if o.Count <= 0 {
		o.Count = 5000
	}
	if o.Lexicon == nil {
		o.Lexicon = fp.BaseLexicon()
	}
	g := &corpusGen{rng: rand.New(rand.NewSource(o.Seed)), lex: newCorpusLexicon(o.Lexicon)}
	opts := corpusOptions(o.Lexicon)
	rep := CorpusReport{Kinds: map[string]int{}, Knowledge: map[string]int{}, Failures: map[string]int{}}
	seen := map[string]bool{}
	var groups [][]Pair
	kept := 0
	for kept < o.Count && rep.Generated < o.Count*20 {
		base := g.task()
		texts := []string{base.render()}
		first, ok := Solve(texts[0], opts)
		decided := ok && first != copyOf(texts[0])
		share := o.H3
		if !decided {
			share /= 3
		}
		if g.rng.Float64() < share {
			texts = append(texts, g.lacking(base).render())
		}
		var group []Pair
		fresh := true
		for _, t := range texts {
			if seen[t] {
				fresh = false
			}
		}
		if !fresh {
			continue
		}
		for _, t := range texts {
			rep.Generated++
			seen[t] = true
			answer, ok := Solve(t, opts)
			if !ok {
				rep.Failures["два значения одного вопроса отчёта — ответа нет"]++
				continue
			}
			if good, why := checkPair(t, answer, opts); !good {
				rep.Failures[why]++
				continue
			}
			group = append(group, Pair{Task: t, Answer: answer})
		}
		if len(group) > 0 {
			groups = append(groups, group)
			kept += len(group)
		}
	}
	split := int(float64(kept) * (1 - o.Test))
	var out []Pair
	for _, group := range groups {
		part := "train"
		if len(out) >= split {
			part = "test"
		}
		for _, p := range group {
			p.Split = part
			if part == "train" {
				rep.Train++
			}
			out = append(out, p)
			rep.Kinds[pairKind(p.Task, p.Answer, opts)]++
			for kind, mark := range map[string]string{"import": "\nimport @", "requires:": "\nrequires:",
				"правило": "\nhard ", "обобщение": "\ngeneralization "} {
				if strings.Contains(p.Task, mark) {
					rep.Knowledge[kind]++
				}
			}
		}
	}
	return out, rep
}

// Tokenize добавляет паре токены словарём модели.
func (p *Pair) Tokenize(tok *Tokenizer) error {
	ids, start, _, err := tok.Pair(p.Task, &p.Answer)
	if err != nil {
		return err
	}
	p.IDs, p.Start = ids, start
	return nil
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func containsString(list []string, s string) bool { return indexOf(list, s) >= 0 }
