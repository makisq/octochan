package fp

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Замкнутость языка: слово со смыслом — только из словаря W.
//
// Слова со смыслом стоят там, где грамматика ждёт понятие: слова классов,
// имена отношений, обобщений и слотов, значения доменов, вопросы и выборы
// отчёта. Имя со смыслом — слово словаря или составное имя из слов
// словаря через «_». Имена частных случаев — ячеек, атрибутов, целей
// правил, подставленных значений — не слова языка (П3): это имена
// файлов и узлов, и они открыты, но только там, где грамматика ждёт
// значение. Отчёт пишет модель, и новых имён в нём нет: только слова
// словаря и имена, которые уже есть в промпте.

// buildLexicon собирает словарь промпта: базовый и словари оператора
// из настроек, а к ним — словари подключённых скиллов.
func (c *checker) buildLexicon() {
	if c.opts.Lexicon != nil {
		c.lex = c.opts.Lexicon.Clone()
	} else {
		c.lex = BaseLexicon()
	}
	for _, imp := range c.prog.Imports {
		if c.opts.Library == nil {
			c.lexGaps = append(c.lexGaps, fmt.Sprintf("словарь @%s без библиотеки скиллов не подключается", imp.Skill))
			continue
		}
		skill, ok := c.opts.Library(imp.Skill)
		if !ok {
			// Отсутствие скилла — отказ checkImports; слова, которые он
			// мог бы дать, — следствие, а не причина.
			c.lexGaps = append(c.lexGaps, fmt.Sprintf("скилла @%s нет в библиотеке", imp.Skill))
			continue
		}
		if skill.LexiconErr != nil {
			c.report(imp.Pos, ErrUndef, "словарь скилла @%s негоден: %v", imp.Skill, skill.LexiconErr)
			c.lexGaps = append(c.lexGaps, fmt.Sprintf("словарь @%s негоден", imp.Skill))
			continue
		}
		if skill.Lexicon == nil {
			continue
		}
		if err := c.lex.Merge(skill.Lexicon); err != nil {
			c.report(imp.Pos, ErrConflict, "словарь скилла @%s расходится с уже подключёнными: %v", imp.Skill, err)
		}
	}
}

// checkWordSense — слово класса есть в словаре, входит в язык при
// потолке k и означает этот класс.
func (c *checker) checkWordSense(word Name, class string) {
	if c.lex.IsFunction(word.Text) {
		c.report(word.Pos, ErrSyn, "служебное слово %q класса не означает: в classes: — только слова со смыслом", word.Text)
		return
	}
	entry, ok := c.lex.Lookup(word.Text)
	if !ok {
		c.unknownWord(word.Pos, word.Text, fmt.Sprintf("слова %q нет в словаре языка", word.Text))
		return
	}
	if entry.N() > c.opts.MaxMeanings {
		c.report(word.Pos, ErrSyn, "у слова %q смыслов %d (%s), а потолок fp.max_meanings = %d: при нём слова в языке нет",
			word.Text, entry.N(), strings.Join(entry.Senses, ", "), c.opts.MaxMeanings)
		return
	}
	if !contains(entry.Senses, class) {
		c.report(word.Pos, ErrSyn, "в языке слово %q означает %s, а не %s: смыслы слова задаёт словарь, а не промпт",
			word.Text, strings.Join(entry.Senses, " или "), class)
		c.hint("класс слова — один из его смыслов: %s : %q (словарь: %s)",
			entry.Senses[0], word.Text, strings.Join(entry.Sources, ", "))
	}
}

// unknownWord — слова нет в словаре. Если словарь неполон, это
// замечание: слово мог бы дать недоступный словарь скилла.
func (c *checker) unknownWord(pos Pos, word, msg string) {
	if len(c.lexGaps) > 0 {
		c.report(pos, Note, "%s; проверить нельзя: %s", msg, strings.Join(c.lexGaps, ", "))
		return
	}
	c.report(pos, ErrSyn, "%s", msg)
	c.hint("%s", c.wordHint(word))
}

// wordHint — где взять слово: словарь скилла библиотеки, в котором оно
// есть, или похожее слово словаря.
func (c *checker) wordHint(word string) string {
	if skills := c.skillsWithWord(word); len(skills) > 0 {
		imports := make([]string, len(skills))
		for i, s := range skills {
			imports[i] = "import " + s
		}
		return fmt.Sprintf("слово %s есть в словаре %s — подключите: %s (протокол, «Vocabulary»)",
			word, strings.Join(skills, ", "), strings.Join(imports, " или "))
	}
	known := append(c.lex.WordsWithin(c.opts.MaxMeanings), c.lex.FunctionWords()...)
	if near := nearest(word, known); near != "" {
		return fmt.Sprintf("возможно, имелось в виду %s", near)
	}
	return "слова области приносят словари скиллов (lexicon.yaml пакета) и словари оператора " +
		"(fp.lexicons); список — ochan fp words (протокол, «Vocabulary»)"
}

// skillsWithWord — скиллы библиотеки, в словаре которых есть слово.
func (c *checker) skillsWithWord(word string) []string {
	if c.opts.Skills == nil || c.opts.Library == nil {
		return nil
	}
	var found []string
	for _, name := range c.opts.Skills() {
		skill, ok := c.opts.Library(name)
		if !ok || skill.Lexicon == nil {
			continue
		}
		if _, ok := skill.Lexicon.Lookup(word); !ok {
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

// foreignParts — части составного имени, которых нет в языке: ни слова
// словаря в пределах потолка, ни служебного, ни ключевого, ни числа.
func (c *checker) foreignParts(name string) []string {
	var bad []string
	for _, part := range strings.Split(name, "_") {
		switch {
		case part == "":
			bad = append(bad, "«_»")
		case isNumber(part), IsKeyword(part), c.lex.IsFunction(part):
		default:
			entry, ok := c.lex.Lookup(part)
			if !ok || entry.N() > c.opts.MaxMeanings {
				bad = append(bad, part)
			}
		}
	}
	return bad
}

func isNumber(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// checkMeaningful — имя со смыслом: слово словаря или составное из них.
func (c *checker) checkMeaningful(name Name, what string) {
	bad := c.foreignParts(name.Text)
	if len(bad) == 0 {
		return
	}
	msg := fmt.Sprintf("%s %s: %s нет в словаре языка — имя со смыслом составляется из слов словаря через _",
		what, name.Text, describeParts(bad))
	c.unknownWord(name.Pos, bad[0], msg)
}

func describeParts(bad []string) string {
	if len(bad) == 1 {
		return "слова " + bad[0]
	}
	return "слов " + strings.Join(bad, ", ")
}

// checkVocabulary проверяет имена со смыслом, единицы измерения и отчёт.
func (c *checker) checkVocabulary() {
	for _, rel := range c.prog.Relations {
		if rel.Label != "" {
			c.checkMeaningful(Name{Pos: rel.Pos, Text: rel.Label}, "имя отношения")
		}
	}
	for _, g := range c.prog.Generalizations {
		// Обобщение с именем отношения проверено вместе с отношением.
		if _, linked := c.relations[g.Name]; !linked {
			c.checkMeaningful(Name{Pos: g.Pos, Text: g.Name}, "имя обобщения")
		}
		for _, pr := range g.Params {
			c.checkMeaningful(Name{Pos: pr.Pos, Text: pr.Name}, "слот")
			for _, v := range pr.Domain {
				if _, _, number := ParseNumber(v); !number {
					c.checkMeaningful(Name{Pos: pr.Pos, Text: v}, "значение домена")
				} else {
					c.checkUnit(v, pr.Pos)
				}
			}
			if pr.Range != nil && pr.Range.Unit != "" {
				c.checkUnit("1 "+pr.Range.Unit, pr.Pos)
			}
		}
	}
	for _, inst := range c.prog.Instances {
		for _, b := range inst.Bindings {
			c.checkUnit(b.Value, b.Pos)
		}
	}
	for _, cell := range c.prog.Cells {
		for _, set := range cell.Sets {
			c.checkUnit(set.Value.Text, set.Value.Pos)
		}
	}
	c.checkReportNames()
}

// checkUnit — единица после числа есть в словаре.
func (c *checker) checkUnit(text string, pos Pos) {
	number, unit, ok := strings.Cut(text, " ")
	if !ok || number == "" || !unicode.IsDigit([]rune(number)[0]) || unit == "%" {
		return
	}
	for _, part := range strings.Split(unit, "/") {
		if !c.lex.IsUnit(part) {
			c.report(pos, ErrSyn, "единицы %s нет в словаре языка", part)
			c.hint("единицы словаря: %s, а ещё %%", strings.Join(c.lex.Units(), ", "))
			return
		}
	}
}

// checkReportNames — в отчёте только слова словаря и имена из промпта:
// модель не вводит новых имён.
func (c *checker) checkReportNames() {
	if len(c.prog.Report) == 0 {
		return
	}
	known := c.promptNames()
	check := func(name string, pos Pos, what string) {
		for _, part := range strings.Split(name, ".") {
			if known[part] || len(c.foreignParts(part)) == 0 {
				continue
			}
			msg := fmt.Sprintf("%s %s: %s нет ни в словаре языка, ни в промпте — отчёт не вводит новых имён",
				what, name, describeParts(c.foreignParts(part)))
			c.unknownWord(pos, part, msg)
			return
		}
	}
	for _, entry := range c.prog.Report {
		check(entry.Question, entry.Pos, "вопрос")
		if entry.Choice != nil {
			if isNumber(firstField(entry.Choice.Text)) {
				c.checkUnit(entry.Choice.Text, entry.Choice.Pos)
			} else {
				check(entry.Choice.Text, entry.Choice.Pos, "выбор")
			}
		}
		for _, alt := range entry.Alternatives {
			check(alt, entry.Pos, "альтернатива")
		}
	}
}

func firstField(text string) string {
	first, _, _ := strings.Cut(text, " ")
	if i := strings.IndexByte(first, '.'); i >= 0 && isNumber(first[:i]) {
		return first[:i]
	}
	return first
}

// promptNames — все имена промпта без отчёта: на них отчёт может
// ссылаться, даже если в словаре их нет.
func (c *checker) promptNames() map[string]bool {
	names := map[string]bool{}
	add := func(texts ...string) {
		for _, t := range texts {
			for _, part := range strings.Split(t, ".") {
				if part != "" {
					names[part] = true
				}
			}
		}
	}
	p := c.prog
	if p.Goal != nil {
		add(p.Goal.Word)
	}
	for _, class := range p.Classes {
		add(class.Name)
		for _, w := range class.Words {
			add(w.Text)
		}
	}
	for _, rel := range p.Relations {
		add(rel.From, rel.To, rel.Label)
	}
	for _, g := range p.Generalizations {
		add(g.Name, g.Result, g.Op.Text)
		for _, pr := range g.Params {
			add(pr.Name, pr.Class)
			add(pr.Domain...)
		}
	}
	for _, inst := range p.Instances {
		add(inst.Name, inst.Of.Text)
		for _, b := range inst.Bindings {
			add(b.Slot, b.Value)
		}
	}
	for _, r := range p.Requires {
		add(r.Text)
	}
	for _, imp := range p.Imports {
		for _, item := range imp.Provides {
			add(item.Text)
		}
	}
	for _, rule := range p.Rules {
		add(rule.Action)
		for _, arg := range rule.Args {
			add(arg.Values...)
		}
	}
	for _, cell := range p.Cells {
		add(cell.Name)
		for _, set := range cell.Sets {
			add(set.Target)
			if !set.Value.Ref && isNumber(firstField(set.Value.Text)) {
				continue
			}
			add(set.Value.Text)
		}
	}
	return names
}
