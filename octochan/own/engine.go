package own

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"ochan/arbiter"
	"ochan/embedded"
	"ochan/fp"
	"ochan/rings"
)

// Своя модель ℒ — архитектурная: сеть, словарь, логика и кольца — одна вещь.
//
// Модель не дообучается из чужой: её веса и словарь — только ℒ. Протокол
// она не читает — язык у неё в весах; из prompt ochan берётся только
// задача (после TaskMarker) в канонической записи, имена частных случаев —
// переменные.
//
// Логика и кольца — пакеты fp и rings, те же, которыми ochan судит ответ
// любой модели снаружи; второй их реализации нет:
//
//   - задачу без предпосылки (⊥undef) модель не решает: отказ — её
//     собственный ответ;
//   - копию задачи и то, что следует из посылок, пишет логика
//     (fp.PlanAnswer); кольцо 1 даёт поддержку выведенному по надёжности
//     правил из журнала, кольцо 2 не пропускает слабое — оно уходит
//     вопросом оператору;
//   - задачу, структура которой повторяет кластер инцидентов, модель узнаёт
//     сама (fp.Shape): надёжность правил на ней — по исходам его
//     инцидентов, а плохо решённая структура — вопрос оператору. Так же —
//     по обобщениям пакета знаний, который задача подключает;
//   - сеть выбирает только среди допустимых: правилом выбора с весами и
//     калибровкой из журнала (rings.json рядом с весами);
//   - её распределение переопределено: следующий токен вне допустимых
//     логикой утверждений имеет вероятность ноль, единственный допустимый —
//     единицу. Это видит и протокол движка v1 (/v1/next).

// TaskMarker — граница протокола и задачи в prompt ochan.
const TaskMarker = "\n\n# Task\n\n"

// Engine — имя движка в /v1/info.
const EngineName = "ochan"

// IsModel — каталог своей модели: в нём словарь ℒ.
func IsModel(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, TokenizerFile))
	return err == nil
}

// Model — своя модель, загруженная в процесс.
type Model struct {
	Dir     string
	Tok     *Tokenizer
	Net     *Net
	Params  *Params
	Library *Library
	// Options — проверка задачи: недостающее (⊥undef) и разбор. Без
	// библиотеки скиллов скилл даёт то, что объявлено в его provides:.
	Options fp.Options

	mu        sync.Mutex
	plans     map[string]*plan
	copies    map[string][]int
	prompt    string
	promptIDs []int
	names     *Names
	paths     map[string][]int
	proposed  map[string]map[string]int
}

// Load — модель каталога dir; modules — каталог модулей ochan с пакетами
// знаний (пусто — знаний нет).
func Load(dir, modules string) (*Model, error) {
	if !IsModel(dir) {
		return nil, fmt.Errorf("%s: не своя модель ℒ — нет %s", dir, TokenizerFile)
	}
	tok, err := LoadTokenizer(dir)
	if err != nil {
		return nil, err
	}
	net, err := LoadNet(dir)
	if err != nil {
		return nil, err
	}
	if net.VocabSize != tok.Len() {
		return nil, fmt.Errorf("словарь %d токенов, а выход сети — %d", tok.Len(), net.VocabSize)
	}
	params, err := LoadParams(dir)
	if err != nil {
		return nil, err
	}
	return New(dir, tok, net, params, NewLibrary(modules)), nil
}

// New — модель из готовых частей.
func New(dir string, tok *Tokenizer, net *Net, params *Params, lib *Library) *Model {
	if lib == nil {
		lib = NewLibrary("")
	}
	return &Model{Dir: dir, Tok: tok, Net: net, Params: params, Library: lib,
		Options: fp.Options{Builtins: arbiter.Builtins}, plans: map[string]*plan{}, copies: map[string][]int{}}
}

// Info — что модель говорит о себе в протоколе v1.
func (m *Model) Info() embedded.Info {
	return embedded.Info{Protocol: embedded.Protocol, Engine: EngineName, Model: m.Dir, Generate: true,
		Knowledge: m.Library.Listed()}
}

// TaskOf — задача из prompt ochan: протокол своей модели не нужен.
func TaskOf(prompt string) string {
	if _, task, ok := strings.Cut(prompt, TaskMarker); ok {
		return task
	}
	return prompt
}

// plan — план ответа после колец 1 и 2.
type plan struct {
	answer     *fp.AnswerPlan
	prog       *fp.Program
	order      []string // неточные предпосылки в порядке задачи
	missing    []string
	packages   []embedded.ModelPackage
	clusterKey string
	cluster    *Cluster
}

// inexactOrder — неточные предпосылки задачи в порядке, в котором их
// последний раз задали неточно: точная запись снимает неточную.
func inexactOrder(prog *fp.Program) []string {
	var order []string
	for _, c := range prog.Cells {
		for _, set := range c.Sets {
			at := -1
			for i, k := range order {
				if k == set.Target {
					at = i
				}
			}
			switch {
			case set.Value.Inexact() && at < 0:
				order = append(order, set.Target)
			case !set.Value.Inexact() && at >= 0:
				order = append(order[:at], order[at+1:]...)
			}
		}
	}
	return order
}

// narrowing — сужение неточной предпосылки по истории и его поддержка.
type narrowing struct {
	d    fp.Decision
	span rings.Interval
}

// narrowings — сужения неточных предпосылок по истории из журнала (раздел
// 5.2): прочтение — значение, которое оператор задавал атрибуту точно или
// одобрил в сужении; поддержка — доля прочтения среди всех. Лидер сужает,
// только если его нижняя граница выше верхних границ остальных; иначе
// варианты сопоставимы, и решает оператор.
func (m *Model) narrowings(a *fp.AnswerPlan, order []string) []narrowing {
	return narrowingsOf(m.Params, a, order)
}

func narrowingsOf(params *Params, a *fp.AnswerPlan, order []string) []narrowing {
	derived, decided := map[string]bool{}, map[string]string{}
	for _, d := range a.Decisions {
		derived[d.Target+"."+d.Attr] = true
		decided[d.Attr] = d.Value
	}
	var out []narrowing
	for _, key := range order {
		vague := a.Inexact[key]
		target, attr, _ := strings.Cut(key, ".")
		counts := params.History[attr]
		total := 0.0
		for _, n := range counts {
			total += n
		}
		if derived[key] || total == 0 {
			continue
		}
		candidates := vague.Variants
		if len(candidates) == 0 {
			candidates = nil
			for _, v := range sortedKeys(counts) {
				if vague.Admits(v) {
					candidates = append(candidates, v)
				}
			}
		}
		if len(candidates) == 0 {
			continue
		}
		spans := map[string]rings.Interval{}
		leader := ""
		for _, v := range candidates {
			spans[v] = rings.Share(counts[v], total)
			if leader == "" || spans[v].L > spans[leader].L {
				leader = v
			}
		}
		rest := -1.0
		for _, v := range candidates {
			if v != leader {
				rest = math.Max(rest, spans[v].U)
			}
		}
		if len(vague.Variants) == 0 {
			// Другие числа диапазона — всё, что не лидер.
			rest = math.Max(rest, rings.Share(total-counts[leader], total).U)
		}
		if rest >= 0 && spans[leader].L <= rest {
			continue
		}
		if v, ok := decided[attr]; ok && v != leader {
			continue
		}
		d := fp.Decision{Target: target, Attr: attr, Value: leader, Basis: "history(" + attr + ")", Rules: []string{"fp.history"}}
		if len(vague.Variants) > 0 && allNames(vague.Variants) {
			d.Alternatives = vague.Variants
		}
		out = append(out, narrowing{d, spans[leader]})
	}
	return out
}

// plan — план ответа на задачу после колец 1 и 2. Задача, структура которой
// повторяет кластер инцидентов или обобщение пакета знаний, который она
// подключает, берёт надёжность правил на этой структуре. Знаний той
// версии, которую задача просит, нет — это недостающая предпосылка.
func (m *Model) plan(task string) *plan {
	slot := fmt.Sprintf("%d\x00%s", m.Library.Generation(), task)
	if p, ok := m.plans[slot]; ok {
		return p
	}
	res := fp.CheckText("task", task, m.Options)
	prog := res.Program
	if prog == nil {
		prog, _ = fp.Parse("task", task)
	}
	p := &plan{prog: prog, order: inexactOrder(prog)}
	for _, d := range res.Diagnostics {
		if d.Code == fp.ErrUndef {
			p.missing = append(p.missing, d.Msg)
		}
	}
	extra, lacking := m.Library.ForTask(prog)
	p.missing = append(p.missing, lacking...)
	full := fp.PlanAnswer(task, prog)
	p.clusterKey, p.cluster = m.Params.ClusterOf(fp.Shape(prog, m.Tok.Closed), extra)

	narrowed := m.narrowings(full, p.order)
	spans := map[string]rings.Interval{}
	var all []fp.Decision
	for _, n := range narrowed {
		spans[n.d.Target+"."+n.d.Attr] = n.span
		all = append(all, n.d)
	}
	all = append(all, full.Decisions...)
	var accepted []fp.Decision
	for _, d := range all {
		span, ok := spans[d.Target+"."+d.Attr]
		if !ok {
			var parts []rings.Interval
			for _, r := range d.Rules {
				parts = append(parts, m.Params.Rule(r, p.cluster))
			}
			span = rings.Chain(parts...)
		}
		well := span.L >= m.Params.WellTrue
		pkg := embedded.ModelPackage{Statement: strings.TrimSpace(d.Line()), Rules: d.Rules, L: span.L, U: span.U, Verdict: "well_false"}
		if well {
			pkg.Verdict = "well_true"
			accepted = append(accepted, d)
		}
		if p.cluster != nil {
			for _, r := range d.Rules {
				if _, on := p.cluster.Rules[r]; on {
					pkg.Cluster = p.clusterKey
					break
				}
			}
		}
		p.packages = append(p.packages, pkg)
	}
	answer := *full
	answer.Decisions = accepted
	p.answer = &answer
	m.plans[slot] = p
	return p
}

// questions — вопросы оператору: что посылки не закрывают, что кольцо 2
// не приняло и какую неточную предпосылку сузить не по чему.
func (m *Model) questions(p *plan) []embedded.ModelQuestion {
	var out []embedded.ModelQuestion
	for _, q := range p.answer.Open {
		out = append(out, embedded.ModelQuestion{Attr: q.Attr, Targets: q.Targets, Values: q.Values, Why: "посылки не выбирают между значениями"})
	}
	narrowed := map[string]bool{}
	for _, d := range p.answer.Decisions {
		narrowed[d.Target+"."+d.Attr] = true
	}
	for _, key := range p.order {
		target, attr, _ := strings.Cut(key, ".")
		open := false
		for _, q := range p.answer.Open {
			if q.Attr == attr && contains(q.Targets, target) {
				open = true
			}
		}
		if !narrowed[key] && !open {
			out = append(out, embedded.ModelQuestion{Statement: "set " + key + " = " + p.answer.Inexact[key].Text,
				Why: "неточная предпосылка: сузить её нечем — ни посылки, ни истории"})
		}
	}
	wt := m.Params.WellTrue
	for _, pkg := range p.packages {
		if pkg.Verdict != "well_false" {
			continue
		}
		q := embedded.ModelQuestion{Statement: pkg.Statement, Rules: pkg.Rules,
			Why: fmt.Sprintf("правила %s проверены мало: нижняя граница поддержки %.2f ниже порога %.2f", strings.Join(pkg.Rules, ", "), pkg.L, wt)}
		if pkg.Cluster != "" {
			var counts []string
			for _, r := range pkg.Rules {
				if c, ok := p.cluster.Rules[r]; ok {
					counts = append(counts, fmt.Sprintf("%s верно %.1f из %.1f", r, c[0], c[1]))
				}
			}
			q.Cluster = pkg.Cluster
			q.Why = fmt.Sprintf("на инцидентах этой структуры (кластер %s) %s: нижняя граница поддержки %.2f ниже порога %.2f",
				pkg.Cluster, strings.Join(counts, "; "), pkg.L, wt)
		}
		out = append(out, q)
	}
	if c := p.cluster; c != nil && c.Outcomes[1] != 0 {
		if lower := m.Params.Outcomes(c).L; lower < wt {
			out = append(out, embedded.ModelQuestion{Cluster: p.clusterKey,
				Why: fmt.Sprintf("инциденты этой структуры (кластер %s) модель решила сама %.1f из %.1f: нижняя граница %.2f ниже порога %.2f — решает оператор",
					p.clusterKey, c.Outcomes[0], c.Outcomes[1], lower, wt)})
		}
	}
	return out
}

// clusterOf — кластер, структуру которого повторяет задача, для ответа.
func (m *Model) clusterOf(p *plan) *embedded.ModelCluster {
	if p.cluster == nil {
		return nil
	}
	r := m.Params.Outcomes(p.cluster)
	return &embedded.ModelCluster{Key: p.clusterKey, Instances: p.cluster.Instances.N(), Solved: p.cluster.Outcomes[0],
		Outcomes: p.cluster.Outcomes[1], L: r.L, U: r.U}
}

// Generate — ответ модели целиком: отказ без предпосылки, копия, пакеты
// логики после колец, выбор сети правилом выбора среди допустимых.
func (m *Model) Generate(ctx context.Context, prompt string) (embedded.Answer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.plan(TaskOf(prompt))
	if len(p.missing) > 0 {
		return embedded.Answer{Refused: true, Missing: p.missing}, nil
	}
	packages := append([]embedded.ModelPackage(nil), p.packages...)
	byLine := map[string]int{}
	for i, pkg := range packages {
		byLine[pkg.Statement] = i
	}
	output, forced, chosen := p.answer.Copy, 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return embedded.Answer{}, err
		}
		adm, ok := p.answer.Next(output)
		if !ok {
			return embedded.Answer{}, errors.New("логика модели не ведёт её же ответ")
		}
		if len(adm) == 0 {
			break
		}
		if len(adm) == 1 {
			output += adm[0]
			forced++
			continue
		}
		scores, err := m.score(prompt, output, adm)
		if err != nil {
			return embedded.Answer{}, err
		}
		type ranked struct {
			p, mean float64
			line    string
		}
		var all []ranked
		for i, line := range adm {
			pkg := packages[byLine[strings.TrimSpace(line)]]
			mean := scores[i].LogP / float64(max(1, scores[i].Tokens))
			// G = 1: выведенное логикой целиком привязано к посылкам.
			all = append(all, ranked{m.Params.Score(rings.Features{LogProb: mean, Lower: pkg.L, Width: pkg.U - pkg.L, Binding: 1}), mean, line})
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].p > all[j].p })
		best := all[0]
		pkg := &packages[byLine[strings.TrimSpace(best.line)]]
		pkg.Chosen, pkg.Score, pkg.LogProb, pkg.Options, pkg.Gap = true, best.p, best.mean, len(adm), best.p-all[1].p
		chosen++
		output += best.line
	}
	return embedded.Answer{Text: output, Packages: packages, Questions: m.questions(p), Forced: forced, Chosen: chosen,
		Cluster: m.clusterOf(p)}, nil
}

// promptIDs — токены prompt: <bos> задача <answer>.
func (m *Model) promptState(prompt string) ([]int, error) {
	if m.prompt != prompt || m.promptIDs == nil {
		ids, _, names, err := m.Tok.Pair(TaskOf(prompt), nil)
		if err != nil {
			return nil, err
		}
		m.prompt, m.promptIDs, m.names = prompt, ids, names
		m.paths, m.proposed = map[string][]int{"": nil}, map[string]map[string]int{}
	}
	return m.promptIDs, nil
}

// path — токены output: путь, который модель сама написала, иначе
// токенизация.
func (m *Model) path(output string) ([]int, error) {
	if p, ok := m.paths[output]; ok {
		return p, nil
	}
	for k := 1; k <= min(len(output), 256); k++ {
		base, piece := output[:len(output)-k], output[len(output)-k:]
		if bp, ok := m.paths[base]; ok {
			if tok, ok := m.proposed[base][piece]; ok {
				p := append(append([]int(nil), bp...), tok)
				m.paths[output] = p
				return p, nil
			}
		}
	}
	p, err := m.Tok.Encode(output, m.names.Copy())
	if err != nil {
		return nil, err
	}
	m.paths[output] = p
	return p, nil
}

func (m *Model) namesAfter(path []int) *Names {
	names := m.names.Copy()
	m.Tok.Decode(path, names)
	return names
}

func (m *Model) ids(prompt, output string) ([]int, error) {
	head, err := m.promptState(prompt)
	if err != nil {
		return nil, err
	}
	path, err := m.path(output)
	if err != nil {
		return nil, err
	}
	return append(append([]int(nil), head...), path...), nil
}

// score — оценки продолжений целиком: сумма log p их токенов после
// prompt+output.
func (m *Model) score(prompt, output string, continuations []string) ([]embedded.Scored, error) {
	base, err := m.ids(prompt, output)
	if err != nil {
		return nil, err
	}
	path, _ := m.path(output)
	names := m.namesAfter(path)
	out := make([]embedded.Scored, len(continuations))
	for i, text := range continuations {
		ids, err := m.Tok.Encode(text, names.Copy())
		if err != nil {
			return nil, err
		}
		seq, logp := append([]int(nil), base...), 0.0
		for _, tok := range ids {
			probs, err := m.Net.Probs(seq)
			if err != nil {
				return nil, err
			}
			logp += math.Log(math.Max(probs[tok], 1e-45))
			seq = append(seq, tok)
		}
		out[i] = embedded.Scored{LogP: logp, Tokens: len(ids)}
	}
	return out, nil
}

// Score — протокол v1: оценки продолжений.
func (m *Model) Score(_ context.Context, prompt, output string, continuations []string) ([]embedded.Scored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.score(prompt, output, continuations)
}

// allowed — распределение без служебных токенов, лишних переменных и байтов.
func (m *Model) allowed(probs []float64, names *Names, end bool) []float64 {
	rest := append([]float64(nil), probs...)
	for _, s := range []int{m.Tok.Pad, m.Tok.Bos, m.Tok.Answer} {
		rest[s] = 0
	}
	if !end {
		rest[m.Tok.Eos] = 0
	}
	for i := range rest {
		if k, ok := m.Tok.Variable(i); ok && k > names.Len() || m.Tok.IsByte(i) {
			rest[i] = 0
		}
	}
	return rest
}

// admissible — токены, которыми может продолжиться output по логике
// модели; end — ответ закончен; ok ложно — логика это место не ведёт.
func (m *Model) admissible(prompt, output string) (tokens map[int]bool, end, ok bool, err error) {
	p := m.plan(TaskOf(prompt))
	if len(p.missing) > 0 {
		return nil, true, true, nil
	}
	cp := p.answer.Copy
	var lines []string
	head, partial := "", output
	idsOf := map[string][]int{}
	if len(output) < len(cp) {
		if !strings.HasPrefix(cp, output) {
			return nil, false, false, nil
		}
		if _, have := m.copies[cp]; !have {
			ids, err := m.Tok.Encode(cp, m.names.Copy())
			if err != nil {
				return nil, false, false, err
			}
			m.copies[cp] = ids
		}
		lines, idsOf[cp] = []string{cp}, m.copies[cp]
	} else {
		cut := strings.LastIndex(output, "\n") + 1
		head, partial = output[:cut], output[cut:]
		adm, ok := p.answer.Next(head)
		if !ok {
			return nil, false, false, nil
		}
		if len(adm) == 0 {
			return nil, partial == "", partial == "", nil
		}
		path, err := m.path(head)
		if err != nil {
			return nil, false, false, err
		}
		names := m.namesAfter(path)
		for _, line := range adm {
			if strings.HasPrefix(line, partial) {
				ids, err := m.Tok.Encode(line, names.Copy())
				if err != nil {
					return nil, false, false, err
				}
				lines, idsOf[line] = append(lines, line), ids
			}
		}
	}
	names := m.names.Copy()
	if head != "" {
		path, err := m.path(head)
		if err != nil {
			return nil, false, false, err
		}
		names = m.namesAfter(path)
	}
	tokens = map[int]bool{}
	for _, line := range lines {
		ids, text, k := idsOf[line], "", 0
		for len(text) < len(partial) && k < len(ids) {
			text += m.Tok.Piece(ids[k], names.Copy())
			k++
		}
		if text == partial && k < len(ids) {
			tokens[ids[k]] = true
		}
	}
	return tokens, false, true, nil
}

// Next — протокол v1: распределение следующего токена, переопределённое
// логикой модели. Вне плана логики — распределение сети одной.
func (m *Model) Next(_ context.Context, prompt, output string, top int) (embedded.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.promptState(prompt); err != nil {
		return embedded.Step{}, err
	}
	allowed, end, ok, err := m.admissible(prompt, output)
	if err != nil {
		return embedded.Step{}, err
	}
	if !ok {
		return m.netNext(prompt, output, top)
	}
	if end || len(allowed) == 0 {
		return embedded.Step{End: 1}, nil
	}
	path, err := m.path(output)
	if err != nil {
		return embedded.Step{}, err
	}
	names := m.namesAfter(path)
	proposed := m.proposed[output]
	if proposed == nil {
		proposed = map[string]int{}
		m.proposed[output] = proposed
	}
	index := make([]int, 0, len(allowed))
	for t := range allowed {
		index = append(index, t)
	}
	sort.Ints(index)
	if len(index) == 1 {
		text := m.Tok.Piece(index[0], names.Copy())
		if _, seen := proposed[text]; !seen {
			proposed[text] = index[0]
		}
		return embedded.Step{Candidates: []embedded.Candidate{{Text: text, P: 1}}}, nil
	}
	ids, err := m.ids(prompt, output)
	if err != nil {
		return embedded.Step{}, err
	}
	probs, err := m.Net.Probs(ids)
	if err != nil {
		return embedded.Step{}, err
	}
	mass := make([]float64, len(index))
	sum := 0.0
	for i, t := range index {
		mass[i] = probs[t]
		sum += mass[i]
	}
	for i := range mass {
		if sum > 0 {
			mass[i] /= sum
		} else {
			mass[i] = 1 / float64(len(index))
		}
	}
	order := make([]int, len(index))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return mass[order[a]] > mass[order[b]] })
	var cands []embedded.Candidate
	for _, k := range order[:min(max(1, top), len(order))] {
		text := m.Tok.Piece(index[k], names.Copy())
		if _, seen := proposed[text]; !seen {
			proposed[text] = index[k]
		}
		cands = append(cands, embedded.Candidate{Text: text, P: mass[k]})
	}
	return embedded.Step{Candidates: cands}, nil
}

// netNext — распределение сети одной, без логики модели.
func (m *Model) netNext(prompt, output string, top int) (embedded.Step, error) {
	ids, err := m.ids(prompt, output)
	if err != nil {
		return embedded.Step{}, err
	}
	probs, err := m.Net.Probs(ids)
	if err != nil {
		return embedded.Step{}, err
	}
	path, _ := m.path(output)
	names := m.namesAfter(path)
	rest := m.allowed(probs, names, false)
	order := make([]int, len(rest))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rest[order[a]] > rest[order[b]] })
	proposed := m.proposed[output]
	if proposed == nil {
		proposed = map[string]int{}
		m.proposed[output] = proposed
	}
	var cands []embedded.Candidate
	for _, t := range order[:min(max(1, top), len(order))] {
		if rest[t] <= 0 {
			continue
		}
		text := m.Tok.Piece(t, names.Copy())
		if _, seen := proposed[text]; !seen {
			proposed[text] = t
		}
		cands = append(cands, embedded.Candidate{Text: text, P: rest[t]})
	}
	return embedded.Step{Candidates: cands, End: probs[m.Tok.Eos]}, nil
}

// Raw — сырой вывод сети без логики и колец: жадный, до <eos>. prefix —
// начало ответа, заданное заранее (каноническая запись): сеть продолжает
// после него. Это замер того, что даёт архитектура, а не режим работы.
func (m *Model) Raw(prompt string, maxTokens int, prefix string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	head, err := m.promptState(prompt)
	if err != nil {
		return "", err
	}
	ids := append([]int(nil), head...)
	names := m.names.Copy()
	if prefix != "" {
		more, err := m.Tok.Encode(prefix, names)
		if err != nil {
			return "", err
		}
		ids = append(ids, more...)
	}
	after := names.Copy()
	var out []int
	for i := 0; i < maxTokens; i++ {
		probs, err := m.Net.Probs(ids)
		if err != nil {
			return "", err
		}
		rest := m.allowed(probs, names, true)
		best := 0
		for t := range rest {
			if rest[t] > rest[best] {
				best = t
			}
		}
		if best == m.Tok.Eos {
			break
		}
		m.Tok.Piece(best, names)
		ids = append(ids, best)
		out = append(out, best)
	}
	return prefix + m.Tok.Decode(out, after), nil
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// allNames — все значения — имена (не числа): тогда они альтернативы отчёта.
func allNames(values []string) bool {
	for _, v := range values {
		if !isIdent(v) {
			return false
		}
	}
	return len(values) > 0
}
