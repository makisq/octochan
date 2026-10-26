package fp

import (
	"fmt"
	"sort"
	"strings"
)

// Consistency — уровень согласованности ячеек (таблица 5 манифеста).
type Consistency string

const (
	// Strict — λ1: ссылка на то, чего нет, — ошибка со списком; решает
	// оператор. Несогласованное состояние невозможно.
	Strict Consistency = "strict"
	// Reconcile — λ0: система пытается согласовать сама, и каждое
	// согласование — замечание, а не отказ.
	Reconcile Consistency = "reconcile"
)

// Options — то, что задаётся в конфигурации, а не в промпте.
type Options struct {
	// MaxMeanings — k: сколько классов может означать одно слово (П2).
	// Ноль — 3.
	MaxMeanings int
	// Consistency — уровень согласованности; пусто — Strict.
	Consistency Consistency
	// Quiet — Q_off: вопросы, о которых оператор просил не сообщать.
	Quiet []string
	// Library — библиотека скиллов: есть ли скилл, его операции и версия.
	// nil — библиотеки нет, и скилл даёт ровно то, что объявлено в import.
	Library func(name string) (*Skill, bool)
	// Skills — имена всех скиллов библиотеки: по ним подсказка находит
	// скилл, в котором есть недостающая операция.
	Skills func() []string
	// Builtins — операции самой среды выполнения (в ochan — write и save):
	// они известны арбитру без скиллов, и правило о них проверяется по плану.
	Builtins []string
}

// Skill — скилл в библиотеке.
type Skill struct {
	Ops []string
	// Version — версия скилла; пусто — у скилла нет версии.
	Version string
}

// Result — итог разбора и проверки.
type Result struct {
	Program     *Program
	Diagnostics []Diagnostic
	// Goal — класс цели γ.
	Goal string
	// Knowledge — K: отношения, обобщения, частные случаи и операции скиллов.
	Knowledge []string
	// Required — Req(P): то, без чего задачу не решить.
	Required []string
	// Dependents — D(c): ячейки, зависящие от c напрямую или по цепочке.
	Dependents map[string][]string
	// Skills — подключённые скиллы и их версии: из библиотеки, а без неё —
	// как объявлено в import.
	Skills map[string]string
}

// Errors возвращает число отказов (без замечаний).
func (r *Result) Errors() int {
	n := 0
	for _, d := range r.Diagnostics {
		if d.IsError() {
			n++
		}
	}
	return n
}

// CheckText разбирает и проверяет промпт.
//
// Порядок — как в алгоритме 1: если текст не принадлежит языку, дальше
// он не проверяется. Ошибки знания в программе, которую не удалось
// разобрать, объясняли бы следствие, а не причину.
func CheckText(name, src string, opts Options) *Result {
	prog, diags := Parse(name, src)
	if len(diags) > 0 {
		return &Result{Program: prog, Diagnostics: diags}
	}
	return Check(prog, opts)
}

// Check выполняет проверки разбора (разделы 4 и 6.1 манифеста):
// однозначность слов, замкнутость знания Req(P) ⊆ K, совместность жёстких
// правил, а заодно целостность графа, обобщений, ячеек и отчёта. Все
// проверки — поиск по таблицам и сравнение множеств; вероятностных
// решений в них нет.
func Check(prog *Program, opts Options) *Result {
	if opts.MaxMeanings <= 0 {
		opts.MaxMeanings = 3
	}
	if opts.Consistency == "" {
		opts.Consistency = Strict
	}

	c := &checker{
		prog:      prog,
		opts:      opts,
		classes:   make(map[string]*Class),
		meanings:  make(map[string][]string),
		names:     make(map[string]string),
		relations: make(map[string]*Relation),
		gens:      make(map[string]*Generalization),
		knowledge: make(map[string]bool),
		skills:    make(map[string]*Import),
		rules:     make(map[string]*Rule),

		skillVersions: make(map[string]string),
	}

	c.declareClasses()
	c.checkGoal()
	c.declareNames()
	c.checkRelations()
	c.checkPriorities()
	c.checkGeneralizations()
	c.checkInstances()
	c.checkImports()
	c.checkRules()
	for _, r := range prog.Requires {
		c.require(r, "requires")
	}
	c.checkKnowledge()
	deps := c.checkCells()
	c.checkReport()

	sortDiagnostics(c.diags)
	return &Result{
		Program:     prog,
		Diagnostics: c.diags,
		Goal:        c.goal,
		Knowledge:   sortedSet(c.knowledge),
		Required:    c.requiredNames(),
		Dependents:  deps,
		Skills:      c.skillVersions,
	}
}

type requirement struct {
	name Name
	// from — откуда требование: requires, обобщение, правило.
	from string
}

type checker struct {
	prog  *Program
	opts  Options
	diags []Diagnostic

	goal      string
	classes   map[string]*Class
	meanings  map[string][]string // слово → классы в порядке объявления
	names     map[string]string   // имя → что это
	relations map[string]*Relation
	gens      map[string]*Generalization
	knowledge map[string]bool
	required  []requirement
	skills    map[string]*Import
	rules     map[string]*Rule

	skillVersions map[string]string
}

func (c *checker) report(pos Pos, code Code, format string, args ...any) {
	c.diags = append(c.diags, Diagnostic{Pos: pos, Code: code, Msg: fmt.Sprintf(format, args...)})
}

// hint дописывает подсказку к последней находке.
func (c *checker) hint(format string, args ...any) {
	if n := len(c.diags); n > 0 {
		c.diags[n-1].Hint = fmt.Sprintf(format, args...)
	}
}

// skillsWith — скиллы библиотеки, в которых есть операция, с версиями.
func (c *checker) skillsWith(op string) []string {
	if c.opts.Skills == nil || c.opts.Library == nil {
		return nil
	}
	var found []string
	for _, name := range c.opts.Skills() {
		skill, ok := c.opts.Library(name)
		if !ok || !contains(skill.Ops, op) {
			continue
		}
		if skill.Version != "" {
			found = append(found, "@"+name+" v"+skill.Version)
		} else {
			found = append(found, "@"+name)
		}
	}
	sort.Strings(found)
	return found
}

// missingOpHint — где взять недостающую операцию: скилл библиотеки, в
// котором она есть, или похожее имя из того, что уже известно.
func (c *checker) missingOpHint(op string) string {
	if skills := c.skillsWith(op); len(skills) > 0 {
		imports := make([]string, len(skills))
		for i, s := range skills {
			imports[i] = "import " + s
		}
		return fmt.Sprintf("%s есть в %s — подключите: %s (протокол, «Knowledge: requirements and skills»)",
			op, strings.Join(skills, ", "), strings.Join(imports, " или "))
	}
	known := append(sortedSet(c.knowledge), c.opts.Builtins...)
	if near := nearest(op, known); near != "" {
		return fmt.Sprintf("возможно, имелось в виду %s", near)
	}
	return "операцию даёт скилл: import @скилл { provides: " + op + " } (протокол, «Knowledge: requirements and skills»)"
}

// nearest — самое похожее имя на расстоянии не больше двух правок.
func nearest(name string, candidates []string) string {
	best, bestDist := "", 3
	sorted := append([]string(nil), candidates...)
	sort.Strings(sorted)
	for _, cand := range sorted {
		if cand == name {
			continue
		}
		if d := levenshtein(name, cand); d < bestDist {
			best, bestDist = cand, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func (c *checker) require(name Name, from string) {
	c.required = append(c.required, requirement{name: name, from: from})
}

func (c *checker) declaredClass(name string, pos Pos, role string) bool {
	if _, ok := c.classes[name]; ok {
		return true
	}
	c.report(pos, ErrUndef, "%s %s не объявлен в classes:", role, name)
	return false
}

// ---------------------------------------------------------------------------
// Классы, слова, цель
// ---------------------------------------------------------------------------

func (c *checker) declareClasses() {
	for _, class := range c.prog.Classes {
		if prev, ok := c.classes[class.Name]; ok {
			c.report(class.Pos, ErrConflict, "класс %s уже объявлен в строке %d", class.Name, prev.Pos.Line)
			continue
		}
		c.classes[class.Name] = class

		seen := map[string]bool{}
		for _, word := range class.Words {
			if seen[word.Text] {
				c.report(word.Pos, Note, "слово %q повторяется в классе %s", word.Text, class.Name)
				continue
			}
			seen[word.Text] = true
			c.meanings[word.Text] = append(c.meanings[word.Text], class.Name)

			// Одно слово в нескольких классах законно (П4), но смыслов
			// у слова не больше k: иначе язык перестаёт быть ограниченным.
			if n := len(c.meanings[word.Text]); n == c.opts.MaxMeanings+1 {
				c.report(word.Pos, ErrAmbig, "слово %q означает уже %d класса (%s), допустимо не больше %d (П2: |μ(w)| ≤ k)",
					word.Text, n, strings.Join(c.meanings[word.Text], ", "), c.opts.MaxMeanings)
			}
		}
	}
}

// resolve выбирает смысл слова. allowed — классы, которые допускает место
// употребления; пусто — место класс не ограничивает. Возвращает класс,
// если он единственный (|A| = 1).
func (c *checker) resolve(word string, allowed []string, pos Pos, where string) (string, bool) {
	meanings := c.meanings[word]
	candidates := meanings
	if len(allowed) > 0 {
		candidates = nil
		for _, m := range meanings {
			if contains(allowed, m) {
				candidates = append(candidates, m)
			}
		}
	}

	switch len(candidates) {
	case 1:
		return candidates[0], true
	case 0:
		c.report(pos, ErrConflict, "%s: слово %q означает %s, а здесь допустим %s",
			where, word, strings.Join(meanings, ", "), strings.Join(allowed, ", "))
	default:
		c.report(pos, ErrAmbig, "%s: слово %q может означать %s — контекст не выбирает один смысл (|A(P)| > 1)",
			where, word, strings.Join(candidates, " или "))
	}
	return "", false
}

func (c *checker) checkGoal() {
	g := c.prog.Goal
	if g == nil {
		c.report(Pos{1, 1}, ErrSyn, "программа начинается с заголовка goal: слово")
		return
	}
	if _, ok := c.meanings[g.Word]; !ok {
		c.report(g.Pos, ErrSyn, "слова %q нет в словаре: объявите его в classes:", g.Word)
		return
	}
	if class, ok := c.resolve(g.Word, nil, g.Pos, "goal"); ok {
		c.goal = class
	}
}

// declareNames регистрирует имена со строчной буквы: отношения, обобщения,
// частные случаи, ячейки. Одно имя — один элемент, иначе ссылка на него
// стала бы неоднозначной. Исключение — обобщение с именем отношения: это
// схема этого отношения.
func (c *checker) declareNames() {
	claim := func(name string, pos Pos, kind string) bool {
		if prev, ok := c.names[name]; ok {
			c.report(pos, ErrConflict, "имя %s уже занято: %s", name, prev)
			return false
		}
		c.names[name] = fmt.Sprintf("%s, строка %d", kind, pos.Line)
		return true
	}

	for _, rel := range c.prog.Relations {
		if rel.Label != "" && claim(rel.Label, rel.Pos, "отношение") {
			c.relations[rel.Label] = rel
		}
	}
	for _, g := range c.prog.Generalizations {
		if _, linked := c.relations[g.Name]; linked {
			if prev, dup := c.gens[g.Name]; dup {
				c.report(g.Pos, ErrConflict, "обобщение %s уже объявлено в строке %d", g.Name, prev.Pos.Line)
				continue
			}
			c.gens[g.Name] = g
			continue
		}
		if claim(g.Name, g.Pos, "обобщение") {
			c.gens[g.Name] = g
		}
	}
	for _, inst := range c.prog.Instances {
		claim(inst.Name, inst.Pos, "частный случай")
	}
	for _, cell := range c.prog.Cells {
		claim(cell.Name, cell.Pos, "ячейка")
	}
}

// ---------------------------------------------------------------------------
// Граф программы
// ---------------------------------------------------------------------------

func (c *checker) checkRelations() {
	seen := map[string]int{}
	for _, rel := range c.prog.Relations {
		c.declaredClass(rel.From, rel.Pos, "класс")
		c.declaredClass(rel.To, rel.Pos, "класс")
		if rel.Label != "" {
			c.knowledge[rel.Label] = true
		}

		key := rel.From + " " + rel.Type + " " + rel.To
		if line, dup := seen[key]; dup {
			c.report(rel.Pos, Note, "отношение %s --%s--> %s уже есть в строке %d: правка избыточна (утверждение 6, б)",
				rel.From, rel.Type, rel.To, line)
		}
		seen[key] = rel.Pos.Line
	}
}

// checkPriorities проверяет, что приоритеты — частичный порядок: без
// циклов. Конфликт отношений решает порядок, а не сумма, и цикл сделал бы
// его неразрешимым.
func (c *checker) checkPriorities() {
	edges := map[string][]string{}
	for _, pr := range c.prog.Priorities {
		okHigh := c.declaredClass(pr.Higher, pr.Pos, "класс")
		okLow := c.declaredClass(pr.Lower, pr.Pos, "класс")
		if pr.Higher == pr.Lower {
			c.report(pr.Pos, ErrConflict, "%s < %s: класс не может быть важнее самого себя", pr.Higher, pr.Lower)
			continue
		}
		if okHigh && okLow {
			edges[pr.Higher] = append(edges[pr.Higher], pr.Lower)
		}
	}

	if cycle := findCycle(edges); len(cycle) > 0 {
		pos := c.prog.Priorities[0].Pos
		for _, pr := range c.prog.Priorities {
			if pr.Higher == cycle[0] {
				pos = pr.Pos
				break
			}
		}
		c.report(pos, ErrConflict, "приоритеты образуют цикл %s: это не частичный порядок, и конфликт им не разрешить",
			strings.Join(cycle, " < "))
	}
}

// findCycle возвращает цикл в графе, если он есть.
func findCycle(edges map[string][]string) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var cycle []string

	var visit func(node string) bool
	visit = func(node string) bool {
		color[node] = grey
		stack = append(stack, node)
		for _, next := range edges[node] {
			switch color[next] {
			case grey:
				for i, n := range stack {
					if n == next {
						cycle = append(append([]string{}, stack[i:]...), next)
						return true
					}
				}
			case white:
				if visit(next) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[node] = black
		return false
	}

	nodes := make([]string, 0, len(edges))
	for node := range edges {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if color[node] == white && visit(node) {
			return cycle
		}
	}
	return nil
}

// precedes сообщает, что a < b объявлено напрямую или следует по цепочке.
func (c *checker) precedes(a, b string) bool {
	edges := map[string][]string{}
	for _, pr := range c.prog.Priorities {
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

// ---------------------------------------------------------------------------
// Обобщения и частные случаи
// ---------------------------------------------------------------------------

func (c *checker) checkGeneralizations() {
	for _, g := range c.prog.Generalizations {
		c.knowledge[g.Name] = true

		params := map[string]*Param{}
		for _, pr := range g.Params {
			if _, dup := params[pr.Name]; dup {
				c.report(pr.Pos, ErrConflict, "слот %s обобщения %s объявлен дважды", pr.Name, g.Name)
				continue
			}
			params[pr.Name] = pr
			if pr.Class != "" {
				c.declaredClass(pr.Class, pr.Pos, "тип слота")
			}
			seen := map[string]bool{}
			for _, v := range pr.Domain {
				if seen[v] {
					c.report(pr.Pos, ErrConflict, "значение %s повторяется в домене слота %s", v, pr.Name)
				}
				seen[v] = true
			}
		}

		c.declaredClass(g.Result, g.Op.Pos, "класс результата")
		used := map[string]bool{}
		for _, arg := range g.Args {
			if _, ok := params[arg.Text]; !ok {
				c.report(arg.Pos, ErrUndef, "аргумент %s — не слот обобщения %s; слоты: %s", arg.Text, g.Name, paramNames(g))
				continue
			}
			used[arg.Text] = true
		}
		for _, pr := range g.Params {
			if !used[pr.Name] {
				c.report(pr.Pos, Note, "слот %s обобщения %s не передаётся операции %s", pr.Name, g.Name, g.Op.Text)
			}
		}

		// Операция — «как»: её знание приходит из скилла, а не из головы
		// модели (П5).
		c.require(g.Op, "обобщение "+g.Name)

		// Обобщение с именем отношения — схема этого отношения. Её
		// результат — цель отношения, а среди слотов есть его источник.
		if rel, linked := c.relations[g.Name]; linked {
			if g.Result != rel.To {
				c.report(g.Pos, ErrConflict, "обобщение %s — схема отношения %s --%s--> %s, но его результат %s, а не %s",
					g.Name, rel.From, rel.Type, rel.To, g.Result, rel.To)
			}
			hasSource := false
			for _, pr := range g.Params {
				if pr.Class == rel.From {
					hasSource = true
				}
			}
			if !hasSource {
				c.report(g.Pos, ErrConflict, "обобщение %s — схема отношения из %s, но ни один слот не имеет тип %s",
					g.Name, rel.From, rel.From)
			}
		}
	}
}

func paramNames(g *Generalization) string {
	names := make([]string, len(g.Params))
	for i, pr := range g.Params {
		names[i] = pr.Name
	}
	return strings.Join(names, ", ")
}

func (c *checker) checkInstances() {
	for _, inst := range c.prog.Instances {
		c.knowledge[inst.Name] = true

		g, ok := c.gens[inst.Of.Text]
		if !ok {
			c.report(inst.Of.Pos, ErrUndef, "обобщения %s нет: частный случай подставляет значения в объявленную схему", inst.Of.Text)
			continue
		}

		bound := map[string]bool{}
		for _, b := range inst.Bindings {
			var param *Param
			for _, pr := range g.Params {
				if pr.Name == b.Slot {
					param = pr
				}
			}
			if param == nil {
				c.report(b.Pos, ErrSyn, "у обобщения %s нет слота %s; слоты: %s", g.Name, b.Slot, paramNames(g))
				continue
			}
			if bound[b.Slot] {
				c.report(b.Pos, ErrConflict, "слот %s заполнен дважды", b.Slot)
				continue
			}
			bound[b.Slot] = true

			switch {
			case len(param.Domain) > 0:
				if !contains(param.Domain, b.Value) {
					c.report(b.Pos, ErrSyn, "значение %s вне домена слота %s: {%s}", b.Value, b.Slot, strings.Join(param.Domain, ", "))
				}
			case param.Class != "":
				// Имя-значение (o_17, db-main) классом не проверить — оно
				// не слово языка (П3). А слово словаря проверяется: место
				// употребления выбирает его смысл.
				if _, isWord := c.meanings[b.Value]; isWord {
					c.resolve(b.Value, []string{param.Class}, b.Pos, "слот "+b.Slot)
				}
			}
		}

		var missing []string
		for _, pr := range g.Params {
			if !bound[pr.Name] {
				missing = append(missing, pr.Name)
			}
		}
		if len(missing) > 0 {
			c.report(inst.Pos, ErrUndef, "в частном случае %s не заполнены слоты: %s — подстановка задаёт все слоты (определение 3)",
				inst.Name, strings.Join(missing, ", "))
		}
	}
}

// ---------------------------------------------------------------------------
// Скиллы
// ---------------------------------------------------------------------------

// checkImports — арбитр скиллов A2: скилл существует, его классы
// допустимы для задачи, требуемые им классы объявлены в промпте.
// Совместимость считается по классам промпта, а не по «типу задачи»
// (определение 12).
func (c *checker) checkImports() {
	for _, imp := range c.prog.Imports {
		if prev, dup := c.skills[imp.Skill]; dup {
			c.report(imp.Pos, Note, "скилл @%s уже подключён в строке %d", imp.Skill, prev.Pos.Line)
			continue
		}
		c.skills[imp.Skill] = imp

		var libraryOps []string
		c.skillVersions[imp.Skill] = strings.TrimPrefix(imp.Version, "v")
		if c.opts.Library != nil {
			skill, ok := c.opts.Library(imp.Skill)
			if !ok {
				c.report(imp.Pos, ErrUndef, "скилла @%s нет в библиотеке", imp.Skill)
				if c.opts.Skills != nil {
					if near := nearest(imp.Skill, c.opts.Skills()); near != "" {
						c.hint("возможно, имелось в виду @%s", near)
					} else if names := c.opts.Skills(); len(names) > 0 {
						sorted := append([]string(nil), names...)
						sort.Strings(sorted)
						c.hint("в библиотеке: @%s", strings.Join(sorted, ", @"))
					}
				}
				continue
			}
			libraryOps = skill.Ops
			c.skillVersions[imp.Skill] = skill.Version

			// Версия — часть знания: операция с тем же именем в другой
			// версии скилла может делать другое.
			switch {
			case imp.Version == "":
			case skill.Version == "":
				c.report(imp.Pos, ErrUndef, "промпт требует @%s %s, а у скилла в библиотеке версии нет", imp.Skill, imp.Version)
				continue
			case !VersionMatches(imp.Version, skill.Version):
				c.report(imp.Pos, ErrUndef, "промпт требует @%s %s, а в библиотеке версия %s", imp.Skill, imp.Version, skill.Version)
				c.hint("версия — решение оператора: промпт с import @%s v%s или скилл нужной версии в библиотеке", imp.Skill, skill.Version)
				continue
			}
		}

		declaredOps := false
		for _, item := range imp.Provides {
			if isClassName(item.Text) {
				if _, ok := c.classes[item.Text]; !ok {
					c.report(item.Pos, ErrUndef, "скилл @%s даёт класс %s, которого нет в промпте: класс скилла должен быть "+
						"допустим для задачи (определение 12)", imp.Skill, item.Text)
				}
				continue
			}
			declaredOps = true
			if c.opts.Library != nil && !contains(libraryOps, item.Text) {
				c.report(item.Pos, ErrUndef, "в скилле @%s нет операции %s", imp.Skill, item.Text)
				if near := nearest(item.Text, libraryOps); near != "" {
					c.hint("в @%s есть %s", imp.Skill, near)
				} else if skills := c.skillsWith(item.Text); len(skills) > 0 {
					c.hint("%s есть в %s", item.Text, strings.Join(skills, ", "))
				}
				continue
			}
			c.knowledge[item.Text] = true
		}
		// Без перечня операций скилл даёт всё, что в нём есть.
		if !declaredOps {
			for _, op := range libraryOps {
				c.knowledge[op] = true
			}
		}

		for _, class := range imp.RequiresClass {
			if _, ok := c.classes[class.Text]; !ok {
				c.report(class.Pos, ErrUndef, "скилл @%s требует класс %s, которого нет в промпте: для этой задачи скилл недопустим",
					imp.Skill, class.Text)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Жёсткие правила
// ---------------------------------------------------------------------------

func (c *checker) checkRules() {
	seen := map[string]*Rule{}
	for _, rule := range c.prog.Rules {
		keys := map[string]bool{}
		for _, arg := range rule.Args {
			if keys[arg.Name] {
				c.report(arg.Pos, ErrConflict, "параметр %s правила %s повторяется", arg.Name, rule.Action)
			}
			keys[arg.Name] = true
		}

		// Ж1: «!» — только для того, что проверяется по плану. По плану
		// проверяется действие, которое знает скилл или отношение;
		// остального арбитр в плане не найдёт.
		c.require(Name{Pos: rule.Pos, Text: rule.Action}, "правило")

		key := string(rule.Kind) + " " + rule.Action
		if prev, dup := seen[key]; dup {
			c.report(rule.Pos, Note, "правило %s %s уже есть в строке %d: правка избыточна (утверждение 6, б)",
				rule.Kind, rule.Action, prev.Pos.Line)
			continue
		}
		seen[key] = rule

		// Ж3: приоритета между абсолютными правилами нет, поэтому
		// «никогда X» и «всегда X» вместе — ошибка до исполнения. Кроме
		// одного случая: требование действует только там, где запрет
		// сделал исключение, — тогда оба выполнимы сразу.
		opposite := Forbid
		if rule.Kind == Forbid {
			opposite = Require
		}
		if prev, clash := seen[string(opposite)+" "+rule.Action]; clash {
			forbid, require := rule, prev
			if rule.Kind == Require {
				forbid, require = prev, rule
			}
			if !withinExceptions(require, forbid) {
				c.report(rule.Pos, ErrConflict, "правило %s %s несовместно с правилом %s %s в строке %d: "+
					"между жёсткими правилами нет приоритета (Ж3)", rule.Kind, rule.Action, prev.Kind, prev.Action, prev.Pos.Line)
				c.hint("совместны, если требование ограничено исключениями запрета: "+
					"hard forbid %s(except = {окно}) и hard require %s(scope = окно) (протокол, «Hard rules»)",
					rule.Action, rule.Action)
			}
		}
		c.rules[rule.Action] = rule
	}
}

// withinExceptions — область требования лежит внутри исключений запрета:
// тогда «никогда, кроме E» и «всегда на S ⊆ E» выполнимы вместе.
func withinExceptions(require, forbid *Rule) bool {
	var except, scope []string
	for _, arg := range forbid.Args {
		if arg.Name == "except" {
			except = append(except, arg.Values...)
		}
	}
	for _, arg := range require.Args {
		if arg.Name == "scope" {
			scope = append(scope, arg.Values...)
		}
	}
	if len(scope) == 0 || len(except) == 0 {
		return false
	}
	for _, name := range scope {
		if !contains(except, name) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Замкнутость знания
// ---------------------------------------------------------------------------

// checkKnowledge — Req(P) ⊆ K. Недостающее — ⊥undef с перечнем, а не
// догадка: модель не пополняет K сама (определение 6).
func (c *checker) checkKnowledge() {
	for _, r := range c.required {
		if c.knowledge[r.name.Text] || contains(c.opts.Builtins, r.name.Text) {
			continue
		}
		switch r.from {
		case "requires":
			c.report(r.name.Pos, ErrUndef, "%s: такого отношения нет ни в промпте, ни в скиллах — "+
				"система не достраивает его сама (П5)", r.name.Text)
		case "правило":
			c.report(r.name.Pos, ErrUndef, "действие %s неизвестно ни скиллам, ни отношениям: арбитру нечем "+
				"проверить правило по плану (Ж1)", r.name.Text)
		default:
			c.report(r.name.Pos, ErrUndef, "операция %s (%s) не дана ни одним скиллом: «как» приходит из скилла (П5)",
				r.name.Text, r.from)
		}
		c.hint("%s", c.missingOpHint(r.name.Text))
	}
}

func (c *checker) requiredNames() []string {
	set := map[string]bool{}
	for _, r := range c.required {
		set[r.name.Text] = true
	}
	return sortedSet(set)
}

// ---------------------------------------------------------------------------
// Ячейки
// ---------------------------------------------------------------------------

// checkCells вычисляет Rd и Wr ячеек и зависимости по ним (определение 14).
func (c *checker) checkCells() map[string][]string {
	cells := c.prog.Cells
	writes := make([]map[string]bool, len(cells))
	reads := make([]map[string]bool, len(cells))
	writer := map[string]int{} // цель → последняя ячейка, которая её задала

	// Кто задаёт цель — нужно знать заранее, чтобы отличить ссылку
	// «вперёд» от ссылки в никуда.
	everWritten := map[string]int{}
	for i := len(cells) - 1; i >= 0; i-- {
		for _, set := range cells[i].Sets {
			everWritten[set.Target] = i
		}
	}

	for i, cell := range cells {
		writes[i] = map[string]bool{}
		reads[i] = map[string]bool{}

		for _, set := range cell.Sets {
			parts := strings.Split(set.Target, ".")
			if len(parts) < 2 {
				c.report(set.Pos, ErrSyn, "set задаёт атрибут: Класс.атрибут или имя.атрибут, получено %s", set.Target)
				continue
			}
			c.checkRoot(parts[0], set.Pos)

			if writes[i][set.Target] {
				c.report(set.Pos, ErrConflict, "ячейка %s задаёт %s дважды", cell.Name, set.Target)
				continue
			}
			if prev, ok := writer[set.Target]; ok {
				c.report(set.Pos, Note, "ячейка %s перезаписывает %s из ячейки %s: действует последняя запись",
					cell.Name, set.Target, cells[prev].Name)
			}
			writes[i][set.Target] = true

			for k := 2; k < len(parts); k++ {
				reads[i][strings.Join(parts[:k], ".")] = true
			}
			if set.Value.Ref {
				c.checkValueRef(cell, i, set.Value, everWritten, reads[i])
			}
		}
		for target := range writes[i] {
			writer[target] = i
		}
	}

	// Прямые зависимости: c_j зависит от c_i, если читает то, что c_i
	// записала (i < j).
	direct := make([][]int, len(cells))
	for j := range cells {
		for i := 0; i < j; i++ {
			for target := range reads[j] {
				if writes[i][target] {
					direct[i] = append(direct[i], j)
					break
				}
			}
		}
	}

	deps := map[string][]string{}
	for i, cell := range cells {
		seen := map[int]bool{}
		queue := append([]int{}, direct[i]...)
		for len(queue) > 0 {
			j := queue[0]
			queue = queue[1:]
			if seen[j] {
				continue
			}
			seen[j] = true
			queue = append(queue, direct[j]...)
		}
		var names []string
		for j := range cells {
			if seen[j] {
				names = append(names, cells[j].Name)
			}
		}
		deps[cell.Name] = names
	}
	return deps
}

// checkRoot проверяет, что корень ссылки — объявленный класс или имя.
func (c *checker) checkRoot(root string, pos Pos) bool {
	if isClassName(root) {
		return c.declaredClass(root, pos, "класс")
	}
	if _, ok := c.names[root]; ok {
		return true
	}
	c.report(pos, ErrUndef, "%s не объявлено: ячейка правит класс, отношение, обобщение или частный случай", root)
	return false
}

func (c *checker) checkValueRef(cell *Cell, index int, value Value, everWritten map[string]int, reads map[string]bool) {
	parts := strings.Split(value.Text, ".")
	if !c.checkRoot(parts[0], value.Pos) {
		return
	}
	for k := 2; k <= len(parts); k++ {
		reads[strings.Join(parts[:k], ".")] = true
	}

	at, ok := everWritten[value.Text]
	code := ErrUndef
	if c.opts.Consistency == Reconcile {
		code = Note
	}
	switch {
	case !ok:
		c.report(value.Pos, code, "ячейка %s ссылается на %s, которое не задаёт ни одна ячейка", cell.Name, value.Text)
	case at >= index:
		c.report(value.Pos, code, "ячейка %s ссылается на %s, которое задаётся позже, в ячейке %s",
			cell.Name, value.Text, c.prog.Cells[at].Name)
	}
}

// ---------------------------------------------------------------------------
// Отчёт
// ---------------------------------------------------------------------------

// checkReport проверяет записи отчёта: выбор из допустимого, основание
// существует, и модель не переопределяет то, что задал промпт (П5).
func (c *checker) checkReport() {
	fixed := c.fixedByPrompt()
	seen := map[string]int{}

	for _, entry := range c.prog.Report {
		if line, dup := seen[entry.Question]; dup {
			c.report(entry.Pos, ErrConflict, "вопрос %s уже есть в отчёте в строке %d", entry.Question, line)
			continue
		}
		seen[entry.Question] = entry.Pos.Line

		if entry.Choice == nil {
			c.report(entry.Pos, ErrSyn, "в записи %s нет choice", entry.Question)
			continue
		}
		if entry.Basis == nil {
			c.report(entry.Pos, ErrSyn, "в записи %s нет basis: решение без основания не принимается", entry.Question)
			continue
		}
		if len(entry.Alternatives) > 0 && !contains(entry.Alternatives, entry.Choice.Text) {
			c.report(entry.Choice.Pos, ErrConflict, "выбор %s не входит в альтернативы {%s}",
				entry.Choice.Text, strings.Join(entry.Alternatives, ", "))
		}
		if contains(c.opts.Quiet, entry.Question) {
			c.report(entry.Pos, Note, "вопрос %s выключен в конфигурации (Q_off): в отчёт не входит", entry.Question)
		}

		if set, ok := fixed[entry.Question]; ok {
			if set.Value.Text != entry.Choice.Text {
				c.report(entry.Choice.Pos, ErrConflict, "промпт задаёт %s = %s, а отчёт выбрал %s: модель не переопределяет "+
					"заданное (П5)", set.Target, set.Value.Text, entry.Choice.Text)
			} else {
				c.report(entry.Pos, Note, "%s задан промптом: в отчёт входят только решения, которые промпт оставил "+
					"открытыми (определение 8)", set.Target)
			}
		}

		c.checkBasis(entry.Basis)
	}
}

// fixedByPrompt — вопросы, на которые ответили ячейки: по полному имени
// цели и по последнему атрибуту, если он однозначен.
func (c *checker) fixedByPrompt() map[string]*Set {
	fixed := map[string]*Set{}
	byAttr := map[string][]*Set{}
	for _, cell := range c.prog.Cells {
		for _, set := range cell.Sets {
			fixed[set.Target] = set
			attr := set.Target[strings.LastIndex(set.Target, ".")+1:]
			byAttr[attr] = append(byAttr[attr], set)
		}
	}
	for attr, sets := range byAttr {
		if len(sets) == 1 {
			if _, taken := fixed[attr]; !taken {
				fixed[attr] = sets[0]
			}
		}
	}
	return fixed
}

func (c *checker) checkBasis(b *Basis) {
	switch b.Kind {
	case "priority":
		if !c.precedes(b.Args[0], b.Args[1]) {
			c.report(b.Pos, ErrUndef, "основание %s: такого приоритета в промпте нет", b)
		}
	case "relation":
		if _, ok := c.relations[b.Args[0]]; !ok {
			c.report(b.Pos, ErrUndef, "основание %s: отношения %s в промпте нет", b, b.Args[0])
		}
	case "skill":
		if _, ok := c.skills[b.Args[0]]; !ok {
			c.report(b.Pos, ErrUndef, "основание %s: скилл не подключён", b)
		}
	case "rule":
		if _, ok := c.rules[b.Args[0]]; !ok {
			c.report(b.Pos, ErrUndef, "основание %s: такого правила нет", b)
		}
	}
}

func sortedSet(set map[string]bool) []string {
	items := make([]string, 0, len(set))
	for item := range set {
		items = append(items, item)
	}
	sort.Strings(items)
	return items
}

// VersionMatches сообщает, подходит ли версия have под требование want.
//
// Требование совпадает с началом версии по целым числам: v2 подходит
// под 2.0 и 2.7.1, v2.3 — под 2.3 и 2.3.9, v2.3.1 — только под 2.3.1.
// Так промпт пишет ровно ту точность, которая ему важна.
func VersionMatches(want, have string) bool {
	want = strings.TrimPrefix(want, "v")
	have = strings.TrimPrefix(have, "v")
	if want == "" {
		return true
	}
	if have == "" {
		return false
	}
	w, h := strings.Split(want, "."), strings.Split(have, ".")
	if len(w) > len(h) {
		// 2.3 удовлетворяет требованию 2.3.0, но не 2.3.1.
		for len(h) < len(w) {
			h = append(h, "0")
		}
	}
	for i := range w {
		if strings.TrimLeft(w[i], "0") != strings.TrimLeft(h[i], "0") {
			return false
		}
	}
	return true
}
