package fp

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// RelationTypes — словарь типов отношений R. Он конечен: тип вне словаря —
// не отношение языка, а ⊥syn.
var RelationTypes = []string{
	"defines", "forces", "limits", "modifies", "precedes", "requires", "strives_to", "terminates",
}

// imperative — глаголы приращения. Ячейка задаёт значение, а не изменение:
// правка «увеличить на 5», применённая дважды, даёт другой результат
// (утверждение 5).
var imperative = map[string]bool{
	"increase": true, "decrease": true, "add": true, "subtract": true,
	"raise": true, "lower": true, "increment": true, "decrement": true,
	"append": true, "remove": true,
}

var (
	importPattern = regexp.MustCompile(`^import\s+@([\p{L}\p{N}_][\p{L}\p{N}_.\-]*)(?:\s+(v\d+(?:\.\d+)*))?$`)
	bindsPattern  = regexp.MustCompile(`^binds_to\s*:\s*(\S+):(\d+)-(\d+)$`)
	wordPattern   = regexp.MustCompile(`^[\p{L}][\p{L}\p{N}_]*$`)
)

// Parse разбирает формальный промпт.
//
// Разбор не останавливается на первой ошибке: строка с ошибкой
// пропускается, остальные разбираются. Оператору и модели нужно исправить
// всё за один заход.
func Parse(name, src string) (*Program, []Diagnostic) {
	p := &parser{prog: &Program{Name: name}, lines: splitLines(src)}
	p.parse()
	if p.prog.Goal == nil && !p.hasGoalError() {
		p.report(Pos{1, 1}, ErrSyn, "программа начинается с заголовка goal: слово")
	}
	sortDiagnostics(p.diags)
	return p.prog, p.diags
}

type parser struct {
	prog  *Program
	lines []srcLine
	i     int
	diags []Diagnostic
}

// hasGoalError сообщает, что строка с goal: была, но не разобралась:
// тогда второе сообщение «нет цели» только запутало бы.
func (p *parser) hasGoalError() bool {
	for _, ln := range p.lines {
		if ln.indent == 0 && len(ln.toks) > 0 && ln.toks[0].text == "goal" {
			return true
		}
	}
	return false
}

func (p *parser) report(pos Pos, code Code, format string, args ...any) {
	p.diags = append(p.diags, Diagnostic{Pos: pos, Code: code, Msg: fmt.Sprintf(format, args...)})
}

// body забирает строки с отступом, идущие за заголовком раздела.
func (p *parser) body() []srcLine {
	var lines []srcLine
	for p.i < len(p.lines) && p.lines[p.i].indent > 0 {
		lines = append(lines, p.lines[p.i])
		p.i++
	}
	return lines
}

func (p *parser) parse() {
	for p.i < len(p.lines) {
		ln := p.lines[p.i]
		p.i++

		if ln.err != nil {
			p.diags = append(p.diags, *ln.err)
			p.body()
			continue
		}
		if ln.indent > 0 {
			p.report(Pos{ln.no, ln.indent + 1}, ErrSyn, "строка с отступом вне раздела: отступом пишется только тело раздела")
			continue
		}
		p.item(ln)
	}
}

const expectedItems = "goal:, classes:, relations:, priority:, generalization, instance, requires:, " +
	"import, hard forbid|require, ??n_|??a_, cell, report:"

func (p *parser) item(ln srcLine) {
	c := &cursor{p: p, ln: ln}
	first := c.peek()

	if first.kind == tIdent && imperative[strings.ToLower(first.text)] {
		p.report(Pos{ln.no, first.col}, ErrSyn, "%q — приращение, а не значение: правка, применённая дважды, "+
			"дала бы другой результат (утверждение 5); пишите set Цель.атрибут = значение", first.text)
		p.body()
		return
	}

	switch {
	case first.kind == tIdent && first.text == "goal":
		p.goal(c)
	case first.kind == tIdent && first.text == "classes":
		p.block(c, "classes", p.classLine)
	case first.kind == tIdent && first.text == "relations":
		p.block(c, "relations", p.relationLine)
	case first.kind == tIdent && first.text == "priority":
		p.block(c, "priority", p.priorityLine)
	case first.kind == tIdent && first.text == "generalization":
		p.generalization(c)
	case first.kind == tIdent && first.text == "instance":
		p.instance(c)
	case first.kind == tIdent && first.text == "requires":
		p.requires(c)
	case first.kind == tIdent && first.text == "import":
		p.importSkill(ln)
	case first.kind == tIdent && first.text == "hard":
		p.hardRule(c)
	case first.kind == tPunct && first.text == "??":
		p.signalRule(c)
	case first.kind == tIdent && first.text == "cell":
		p.cell(c)
	case first.kind == tIdent && first.text == "report":
		p.reportBlock(c)
	default:
		p.report(Pos{ln.no, first.col}, ErrSyn, "такой последовательности нет в языке; раздел начинается с одного из: %s",
			expectedItems)
		p.body()
	}
}

// ---------------------------------------------------------------------------
// Лексемы строки
// ---------------------------------------------------------------------------

// cursor читает лексемы одной строки. Первая ошибка в строке прекращает
// её разбор: дальше она объяснила бы следствие, а не причину.
type cursor struct {
	p   *parser
	ln  srcLine
	i   int
	bad bool
}

func (c *cursor) peek() token {
	if c.i < len(c.ln.toks) {
		return c.ln.toks[c.i]
	}
	col := c.ln.indent + len([]rune(c.ln.raw)) + 1
	return token{kind: tEnd, col: col}
}

func (c *cursor) next() token {
	t := c.peek()
	if c.i < len(c.ln.toks) {
		c.i++
	}
	return t
}

func (c *cursor) pos() Pos { return Pos{c.ln.no, c.peek().col} }

func (c *cursor) fail(code Code, format string, args ...any) {
	if c.bad {
		return
	}
	c.bad = true
	c.p.report(c.pos(), code, format, args...)
}

func (c *cursor) is(kind tokKind, text string) bool {
	t := c.peek()
	return t.kind == kind && (text == "" || t.text == text)
}

func (c *cursor) punct(text, where string) bool {
	if c.bad {
		return false
	}
	if c.is(tPunct, text) {
		c.next()
		return true
	}
	c.fail(ErrSyn, "ожидалось «%s» %s, получено %s", text, where, c.peek().describe())
	return false
}

// name читает имя со строчной буквы.
func (c *cursor) name(what string) (Name, bool) {
	if c.bad {
		return Name{}, false
	}
	t := c.peek()
	if t.kind != tIdent {
		c.fail(ErrSyn, "ожидалось %s, получено %s", what, t.describe())
		return Name{}, false
	}
	if !isName(t.text) {
		c.fail(ErrSyn, "%s пишется со строчной буквы: с заглавной пишутся только классы, получено «%s»", what, t.text)
		return Name{}, false
	}
	c.next()
	return Name{Pos: Pos{c.ln.no, t.col}, Text: t.text}, true
}

// class читает имя класса.
func (c *cursor) class(what string) (Name, bool) {
	if c.bad {
		return Name{}, false
	}
	t := c.peek()
	if t.kind != tIdent || !isClassName(t.text) {
		c.fail(ErrSyn, "ожидался %s — класс с заглавной латинской буквы, получено %s", what, t.describe())
		return Name{}, false
	}
	c.next()
	return Name{Pos: Pos{c.ln.no, t.col}, Text: t.text}, true
}

func (c *cursor) end(where string) bool {
	if c.bad {
		return false
	}
	if c.peek().kind == tEnd {
		return true
	}
	c.fail(ErrSyn, "лишнее %s %s", c.peek().describe(), where)
	return false
}

// nameList читает имена через запятую.
func (c *cursor) nameList(what string) ([]Name, bool) {
	var names []Name
	for {
		n, ok := c.name(what)
		if !ok {
			return nil, false
		}
		names = append(names, n)
		if !c.is(tPunct, ",") {
			return names, true
		}
		c.next()
	}
}

// set читает множество имён {a, b}.
func (c *cursor) set(what string) ([]string, bool) {
	if !c.punct("{", "— начало множества") {
		return nil, false
	}
	names, ok := c.nameList(what)
	if !ok || !c.punct("}", "— конец множества") {
		return nil, false
	}
	values := make([]string, len(names))
	for i, n := range names {
		values[i] = n.Text
	}
	return values, true
}

// value читает значение: имя, ссылку через точку или число с единицей.
func (c *cursor) value(what string) (Value, bool) {
	if c.bad {
		return Value{}, false
	}
	t := c.peek()
	pos := Pos{c.ln.no, t.col}

	switch t.kind {
	case tNumber:
		c.next()
		text := t.text
		if unit := c.unit(); unit != "" {
			text += " " + unit
		}
		return Value{Pos: pos, Text: text}, true

	case tIdent:
		ref, ok := c.ref(what)
		if !ok {
			return Value{}, false
		}
		return Value{Pos: pos, Text: ref, Ref: strings.Contains(ref, ".")}, true

	case tString:
		c.fail(ErrSyn, "свободный текст в значении запрещён: значения — имена из языка или числа")
	default:
		c.fail(ErrSyn, "ожидалось %s, получено %s", what, t.describe())
	}
	return Value{}, false
}

// unit читает единицу измерения после числа: km/h, MB, %.
func (c *cursor) unit() string {
	if c.is(tPunct, "%") {
		c.next()
		return "%"
	}
	if !c.is(tIdent, "") {
		return ""
	}
	parts := []string{c.next().text}
	for c.is(tPunct, "/") {
		c.next()
		if !c.is(tIdent, "") {
			c.fail(ErrSyn, "после «/» ожидалась единица измерения")
			return ""
		}
		parts = append(parts, c.next().text)
	}
	return strings.Join(parts, "/")
}

// ref читает ссылку вида Класс.атрибут.атрибут или имя.атрибут.
func (c *cursor) ref(what string) (string, bool) {
	t := c.peek()
	if t.kind != tIdent {
		c.fail(ErrSyn, "ожидалось %s, получено %s", what, t.describe())
		return "", false
	}
	parts := []string{c.next().text}
	for c.is(tPunct, ".") {
		c.next()
		n, ok := c.name("атрибут после точки")
		if !ok {
			return "", false
		}
		parts = append(parts, n.Text)
	}
	return strings.Join(parts, "."), true
}

// ---------------------------------------------------------------------------
// Разделы
// ---------------------------------------------------------------------------

func (p *parser) goal(c *cursor) {
	pos := c.pos()
	c.next()
	c.punct(":", "после goal")
	word, ok := c.name("слово цели")
	if !ok || !c.end("после цели") {
		return
	}
	if p.prog.Goal != nil {
		p.report(pos, ErrConflict, "цель уже объявлена в строке %d: у программы одна цель γ", p.prog.Goal.Pos.Line)
		return
	}
	if !p.empty() {
		p.report(pos, ErrSyn, "goal: — заголовок программы и пишется первым")
	}
	p.prog.Goal = &Goal{Pos: pos, Word: word.Text}
}

// empty сообщает, что до текущего места не объявлено ничего, кроме цели.
func (p *parser) empty() bool {
	g := p.prog
	return len(g.Classes)+len(g.Relations)+len(g.Priorities)+len(g.Generalizations)+len(g.Instances)+
		len(g.Requires)+len(g.Imports)+len(g.Rules)+len(g.Cells)+len(g.Report) == 0
}

// block разбирает раздел вида «имя:» с телом из строк.
func (p *parser) block(c *cursor, title string, line func(*cursor)) {
	c.next()
	c.punct(":", "после "+title)
	c.end("после " + title + ":")
	lines := p.body()
	if !c.bad && len(lines) == 0 {
		p.report(Pos{c.ln.no, 1}, ErrSyn, "раздел %s: пуст", title)
	}
	for _, ln := range lines {
		if ln.err != nil {
			p.diags = append(p.diags, *ln.err)
			continue
		}
		line(&cursor{p: p, ln: ln})
	}
}

func (p *parser) classLine(c *cursor) {
	class, ok := c.class("класс")
	if !ok || !c.punct(":", "после имени класса") {
		return
	}

	decl := &Class{Pos: class.Pos, Name: class.Text}
	for {
		t := c.peek()
		if t.kind != tString {
			c.fail(ErrSyn, "ожидалось слово словаря в кавычках, получено %s", t.describe())
			return
		}
		if !wordPattern.MatchString(t.text) {
			c.fail(ErrSyn, "слово словаря %q: одно слово из букв, цифр и _", t.text)
			return
		}
		c.next()
		decl.Words = append(decl.Words, Name{Pos: Pos{c.ln.no, t.col}, Text: t.text})
		if !c.is(tPunct, ",") {
			break
		}
		c.next()
	}
	if c.end("после слов класса") {
		p.prog.Classes = append(p.prog.Classes, decl)
	}
}

func (p *parser) relationLine(c *cursor) {
	from, _ := c.class("источник отношения")
	c.punct("--", "перед типом отношения")
	typ, _ := c.name("тип отношения")
	c.punct("-->", "после типа отношения")
	to, _ := c.class("цель отношения")

	rel := &Relation{Pos: from.Pos, From: from.Text, Type: typ.Text, To: to.Text}
	if c.is(tPunct, "(") {
		c.next()
		label, ok := c.name("имя отношения")
		if ok {
			rel.Label = label.Text
		}
		c.punct(")", "после имени отношения")
	}
	if !c.end("после отношения") {
		return
	}
	if !contains(RelationTypes, rel.Type) {
		p.report(typ.Pos, ErrSyn, "тип отношения %q не входит в словарь R: %s", rel.Type, strings.Join(RelationTypes, ", "))
		return
	}
	p.prog.Relations = append(p.prog.Relations, rel)
}

// priorityLine разбирает A < B и цепочки A < B < C.
func (p *parser) priorityLine(c *cursor) {
	chain := []Name{}
	first, ok := c.class("класс")
	if !ok {
		return
	}
	chain = append(chain, first)
	for c.is(tPunct, "<") {
		c.next()
		next, ok := c.class("класс после «<»")
		if !ok {
			return
		}
		chain = append(chain, next)
	}
	if len(chain) < 2 {
		c.fail(ErrSyn, "ожидалось «<»: приоритет записывается как A < B")
		return
	}
	if !c.end("после приоритета") {
		return
	}
	for i := 0; i+1 < len(chain); i++ {
		p.prog.Priorities = append(p.prog.Priorities,
			&Priority{Pos: chain[i].Pos, Higher: chain[i].Text, Lower: chain[i+1].Text})
	}
}

func (p *parser) generalization(c *cursor) {
	c.next()
	name, _ := c.name("имя обобщения")
	c.punct("(", "— начало слотов")

	g := &Generalization{Pos: name.Pos, Name: name.Text}
	for !c.bad {
		param, ok := c.name("слот")
		if !ok || !c.punct(":", "после имени слота") {
			break
		}
		pr := &Param{Pos: param.Pos, Name: param.Text}
		if c.is(tPunct, "{") {
			pr.Domain, ok = c.set("значение домена")
		} else {
			var class Name
			class, ok = c.class("тип слота")
			pr.Class = class.Text
		}
		if !ok {
			break
		}
		g.Params = append(g.Params, pr)
		if !c.is(tPunct, ",") {
			break
		}
		c.next()
	}
	c.punct(")", "— конец слотов")
	c.punct(":", "после слотов обобщения")
	c.end("после заголовка обобщения")

	lines := p.body()
	if c.bad {
		return
	}
	if len(lines) != 1 {
		p.report(name.Pos, ErrSyn, "у обобщения одна строка тела: Класс = операция(слоты), найдено строк: %d", len(lines))
		return
	}
	if lines[0].err != nil {
		p.diags = append(p.diags, *lines[0].err)
		return
	}

	b := &cursor{p: p, ln: lines[0]}
	result, _ := b.class("класс результата")
	b.punct("=", "после класса результата")
	op, _ := b.name("операция")
	b.punct("(", "— начало аргументов")
	if !b.is(tPunct, ")") {
		g.Args, _ = b.nameList("аргумент")
	}
	b.punct(")", "— конец аргументов")
	if !b.end("после тела обобщения") {
		return
	}
	g.Result, g.Op = result.Text, op
	p.prog.Generalizations = append(p.prog.Generalizations, g)
}

func (p *parser) instance(c *cursor) {
	c.next()
	name, _ := c.name("имя частного случая")
	c.punct("=", "после имени частного случая")
	of, _ := c.name("обобщение")
	c.punct("(", "— начало подстановки")

	inst := &Instance{Pos: name.Pos, Name: name.Text, Of: of}
	for !c.bad && !c.is(tPunct, ")") {
		slot, ok := c.name("слот")
		if !ok || !c.punct(":=", "после имени слота") {
			break
		}
		value, ok := c.value("значение слота")
		if !ok {
			break
		}
		inst.Bindings = append(inst.Bindings, &Binding{Pos: slot.Pos, Slot: slot.Text, Value: value.Text})
		if !c.is(tPunct, ",") {
			break
		}
		c.next()
	}
	c.punct(")", "— конец подстановки")
	if c.end("после частного случая") {
		p.prog.Instances = append(p.prog.Instances, inst)
	}
}

func (p *parser) requires(c *cursor) {
	c.next()
	c.punct(":", "после requires")
	names, ok := c.nameList("требуемое отношение")
	if ok && c.end("после списка требований") {
		p.prog.Requires = append(p.prog.Requires, names...)
	}
}

func (p *parser) importSkill(ln srcLine) {
	lines := p.body()
	m := importPattern.FindStringSubmatch(ln.raw)
	if m == nil {
		p.report(Pos{ln.no, 1}, ErrSyn, "ожидалось import @скилл [vN.N], получено %q", ln.raw)
		return
	}
	imp := &Import{Pos: Pos{ln.no, 1}, Skill: m[1], Version: m[2]}

	for _, body := range lines {
		if body.err != nil {
			p.diags = append(p.diags, *body.err)
			continue
		}
		c := &cursor{p: p, ln: body}
		switch {
		case c.is(tIdent, "provides"):
			c.next()
			c.punct(":", "после provides")
			for !c.bad {
				t := c.peek()
				if t.kind != tIdent {
					c.fail(ErrSyn, "ожидался класс или операция, получено %s", t.describe())
					break
				}
				c.next()
				imp.Provides = append(imp.Provides, Name{Pos: Pos{body.no, t.col}, Text: t.text})
				if !c.is(tPunct, ",") {
					break
				}
				c.next()
			}
			c.end("после provides")
		case c.is(tIdent, "requires_class"):
			c.next()
			c.punct(":", "после requires_class")
			for !c.bad {
				class, ok := c.class("требуемый класс")
				if !ok {
					break
				}
				imp.RequiresClass = append(imp.RequiresClass, class)
				if !c.is(tPunct, ",") {
					break
				}
				c.next()
			}
			c.end("после requires_class")
		default:
			c.fail(ErrSyn, "в import допустимы provides: и requires_class:, получено %s", c.peek().describe())
		}
	}
	p.prog.Imports = append(p.prog.Imports, imp)
}

// hardRule разбирает `hard forbid X(...)` и `hard require X(...)`.
func (p *parser) hardRule(c *cursor) {
	pos := c.pos()
	c.next()
	var kind RuleKind
	switch {
	case c.is(tIdent, "forbid"):
		kind = Forbid
	case c.is(tIdent, "require"):
		kind = Require
	default:
		c.fail(ErrSyn, "после hard ожидалось forbid или require, получено %s", c.peek().describe())
		p.body()
		return
	}
	c.next()
	action, _ := c.name("действие")
	p.ruleArgs(c, &Rule{Pos: pos, Kind: kind, Action: action.Text})
}

// signalRule разбирает сигналы ??n_X(...) и ??a_X(...).
func (p *parser) signalRule(c *cursor) {
	pos := c.pos()
	c.next()
	t := c.peek()
	var kind RuleKind
	switch {
	case t.kind == tIdent && strings.HasPrefix(t.text, "n_") && len(t.text) > 2:
		kind = Forbid
	case t.kind == tIdent && strings.HasPrefix(t.text, "a_") && len(t.text) > 2:
		kind = Require
	default:
		c.fail(ErrSyn, "после ?? пишется n_действие (никогда) или a_действие (всегда), слитно, получено %s", t.describe())
		p.body()
		return
	}
	c.next()
	action := t.text[2:]
	if !isName(action) {
		c.fail(ErrSyn, "действие пишется со строчной буквы: %q", action)
		return
	}
	p.ruleArgs(c, &Rule{Pos: pos, Kind: kind, Action: action})
}

// ruleArgs разбирает параметры правила. Значения — только имена языка
// (Ж2): иначе модель сама решала бы, когда исключение сработало, и правило
// стало бы мягким через исключение.
func (p *parser) ruleArgs(c *cursor, rule *Rule) {
	defer p.body()
	c.punct("(", "— начало параметров правила")
	for !c.bad && !c.is(tPunct, ")") {
		key, ok := c.name("параметр правила")
		if !ok {
			return
		}
		if key.Text != "except" && key.Text != "scope" {
			c.p.report(key.Pos, ErrSyn, "у правила параметры except и scope, получено %q", key.Text)
			c.bad = true
			return
		}
		if c.is(tPunct, "=") || c.is(tPunct, ":") {
			c.next()
		} else {
			c.fail(ErrSyn, "после %s ожидалось «=» или «:»", key.Text)
			return
		}

		arg := &RuleArg{Pos: key.Pos, Name: key.Text}
		switch t := c.peek(); {
		case t.kind == tPunct && t.text == "{":
			arg.Values, ok = c.set("значение " + key.Text)
		case t.kind == tIdent:
			var n Name
			n, ok = c.name("значение " + key.Text)
			arg.Values = []string{n.Text}
		default:
			c.fail(ErrSyn, "значение %s — имя или множество имён на том же строгом языке (Ж2), получено %s",
				key.Text, t.describe())
			return
		}
		if !ok {
			return
		}
		rule.Args = append(rule.Args, arg)
		if !c.is(tPunct, ",") {
			break
		}
		c.next()
	}
	c.punct(")", "— конец параметров правила")
	if c.end("после правила") {
		p.prog.Rules = append(p.prog.Rules, rule)
	}
}

func (p *parser) cell(c *cursor) {
	c.next()
	name, _ := c.name("имя ячейки")
	c.punct(":", "после имени ячейки")
	c.end("после заголовка ячейки")
	lines := p.body()
	if c.bad {
		return
	}

	cell := &Cell{Pos: name.Pos, Name: name.Text}
	for _, ln := range lines {
		if ln.err != nil {
			p.diags = append(p.diags, *ln.err)
			continue
		}
		b := &cursor{p: p, ln: ln}
		t := b.peek()
		switch {
		case t.kind == tIdent && t.text == "set":
			b.next()
			target, ok := b.ref("цель правки")
			if !ok || !b.punct("=", "после цели правки") {
				continue
			}
			value, ok := b.value("значение")
			if ok && b.end("после значения") {
				cell.Sets = append(cell.Sets, &Set{Pos: Pos{ln.no, t.col}, Target: target, Value: value})
			}

		case t.kind == tIdent && t.text == "binds_to":
			m := bindsPattern.FindStringSubmatch(ln.raw)
			if m == nil {
				p.report(Pos{ln.no, t.col}, ErrSyn, "ожидалось binds_to: путь:строка-строка, получено %q", ln.raw)
				continue
			}
			from, _ := strconv.Atoi(m[2])
			to, _ := strconv.Atoi(m[3])
			switch {
			case cell.Binds != nil:
				p.report(Pos{ln.no, t.col}, ErrConflict, "у ячейки одна привязка к коду, первая — в строке %d", cell.Binds.Pos.Line)
			case from < 1 || from > to:
				p.report(Pos{ln.no, t.col}, ErrSyn, "диапазон строк %d-%d: начало от 1 и не больше конца", from, to)
			default:
				cell.Binds = &CodeRange{Pos: Pos{ln.no, t.col}, Path: m[1], From: from, To: to}
			}

		case t.kind == tIdent && imperative[strings.ToLower(t.text)]:
			p.report(Pos{ln.no, t.col}, ErrSyn, "%q — приращение, а не значение: ячейки декларативны (утверждение 5); "+
				"пишите set Цель.атрибут = значение", t.text)

		default:
			b.fail(ErrSyn, "в ячейке допустимы set и binds_to:, получено %s", t.describe())
		}
	}
	if len(cell.Sets) == 0 && cell.Binds == nil {
		p.report(name.Pos, ErrSyn, "ячейка %s пуста", cell.Name)
		return
	}
	p.prog.Cells = append(p.prog.Cells, cell)
}

func (p *parser) reportBlock(c *cursor) {
	c.next()
	c.punct(":", "после report")
	c.end("после report:")
	lines := p.body()
	if c.bad {
		return
	}

	var entry *ReportEntry
	seen := map[string]bool{}
	// skipping — запись с испорченным q: её поля не разбираются, иначе
	// каждое дало бы ещё одну ошибку о том же.
	skipping := false
	for _, ln := range lines {
		if ln.err != nil {
			p.diags = append(p.diags, *ln.err)
			continue
		}
		b := &cursor{p: p, ln: ln}
		key, ok := b.name("поле записи отчёта")
		if !ok || !b.punct(":", "после поля") {
			continue
		}

		if key.Text == "q" {
			entry, skipping = nil, true
			q, ok := b.name("вопрос")
			if !ok || !b.end("после вопроса") {
				continue
			}
			entry, skipping = &ReportEntry{Pos: q.Pos, Question: q.Text}, false
			p.prog.Report = append(p.prog.Report, entry)
			seen = map[string]bool{}
			continue
		}
		if skipping {
			continue
		}
		if entry == nil {
			p.report(key.Pos, ErrSyn, "запись отчёта начинается с q: вопрос")
			continue
		}
		if seen[key.Text] {
			p.report(key.Pos, ErrConflict, "поле %s в записи %s повторяется", key.Text, entry.Question)
			continue
		}
		seen[key.Text] = true

		switch key.Text {
		case "choice":
			value, ok := b.value("выбор")
			if ok && b.end("после выбора") {
				entry.Choice = &value
			}
		case "alternatives":
			values, ok := b.set("альтернатива")
			if ok && b.end("после альтернатив") {
				entry.Alternatives = values
			}
		case "basis":
			if basis, ok := p.basis(b); ok {
				entry.Basis = basis
			}
		default:
			p.report(key.Pos, ErrSyn, "в записи отчёта поля q, choice, alternatives, basis, получено %q", key.Text)
		}
	}
	if len(p.prog.Report) == 0 {
		p.report(Pos{c.ln.no, 1}, ErrSyn, "раздел report: пуст")
	}
}

func (p *parser) basis(b *cursor) (*Basis, bool) {
	kind, ok := b.name("основание")
	if !ok {
		return nil, false
	}
	basis := &Basis{Pos: kind.Pos, Kind: kind.Text}

	switch kind.Text {
	case "not_in_prompt":
	case "priority":
		b.punct("(", "после priority")
		higher, _ := b.class("класс")
		b.punct("<", "в приоритете")
		lower, _ := b.class("класс")
		b.punct(")", "после приоритета")
		basis.Args = []string{higher.Text, lower.Text}
	case "skill":
		b.punct("(", "после skill")
		b.punct("@", "перед именем скилла")
		t := b.peek()
		if t.kind != tIdent {
			b.fail(ErrSyn, "ожидалось имя скилла, получено %s", t.describe())
			return nil, false
		}
		b.next()
		b.punct(")", "после имени скилла")
		basis.Args = []string{t.text}
	case "relation", "rule":
		b.punct("(", "после "+kind.Text)
		n, _ := b.name("имя")
		b.punct(")", "после имени")
		basis.Args = []string{n.Text}
	default:
		p.report(kind.Pos, ErrSyn, "основание — not_in_prompt, priority(A < B), relation(имя), skill(@имя) или rule(действие), получено %q", kind.Text)
		return nil, false
	}
	if !b.end("после основания") {
		return nil, false
	}
	return basis, true
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Pos.Line != diags[j].Pos.Line {
			return diags[i].Pos.Line < diags[j].Pos.Line
		}
		return diags[i].Pos.Col < diags[j].Pos.Col
	})
}
