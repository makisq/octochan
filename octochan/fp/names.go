package fp

import (
	"fmt"
	"regexp"
	"strings"
)

// Имена объектов в тексте модели.
//
// Имена частных случаев в языке открыты (o_17, prod_conf): их смысл задаёт
// место в структуре, а не словарь. В тексте оператора так и должно быть.
// Модель же через открытое имя сказала бы что угодно: please_restart — тоже
// имя. Поэтому в тексте модели имя объекта допустимо, только если его можно
// проверить:
//
//   - оно есть в задаче, на которую модель отвечает;
//   - его знает ochan (Names.Known): хост инвентаря, агент, номер заявки
//     или инцидента;
//   - это имя со смыслом: слово словаря W или составное из слов через «_»
//     (служебные и ключевые слова и числа — тоже части);
//   - это новая ячейка cK: так модель называет ячейку своих решений.
//
// Строки binds_to и import пишутся по шаблону целиком (путь, версия) — их
// модель берёт из задачи как есть. Путь из binds_to задачи — и тот файл,
// который модель может назвать в своём сценарии (Names.Path).

// Names — имена, которые модель может писать, кроме имён со смыслом.
type Names struct {
	task  map[string]bool
	lines map[string]bool
	paths map[string]bool
	// Known — объекты, которые знает ochan; nil — никаких.
	Known func(name string) bool
}

// NamesOf — имена задачи task: каждое имя её текста и её строки по
// шаблону. Пустая задача — имён нет: модель пишет письмо без задачи.
func NamesOf(task string) *Names {
	n := &Names{task: map[string]bool{}, lines: map[string]bool{}, paths: map[string]bool{}}
	for _, ln := range splitLines(task) {
		if ln.err != nil {
			continue
		}
		if word := firstWord(ln.raw); word == "binds_to" || word == "import" {
			n.lines[ln.raw] = true
			if m := bindsPattern.FindStringSubmatch(ln.raw); m != nil {
				n.paths[m[1]] = true
			}
			continue
		}
		for _, t := range ln.toks {
			if t.kind == tIdent {
				n.task[t.text] = true
			}
		}
	}
	return n
}

// Path — путь, к которому задача привязала ячейку (binds_to): его модель
// может назвать в сценарии строкой, как оператор назвал его в задаче.
func (n *Names) Path(p string) bool { return n != nil && n.paths[p] }

// newCell — имя ячейки решений модели.
var newCell = regexp.MustCompile(`^c[0-9]+$`)

func (n *Names) allows(name string) bool {
	if n == nil {
		return false
	}
	return n.task[name] || newCell.MatchString(name) || (n.Known != nil && n.Known(name))
}

// Meaningful — имя со смыслом: каждая его часть через «_» — слово словаря
// lex в пределах потолка смыслов, служебное или ключевое слово, единица
// или число.
func Meaningful(name string, lex *Lexicon, maxMeanings int) bool {
	if lex == nil {
		lex = BaseLexicon()
	}
	if maxMeanings <= 0 {
		maxMeanings = 3
	}
	for _, part := range strings.Split(name, "_") {
		switch {
		case part == "":
			return false
		case isNumber(part), IsKeyword(part), lex.IsFunction(part), lex.IsUnit(part):
		default:
			entry, ok := lex.Lookup(part)
			if !ok || entry.N() > maxMeanings {
				return false
			}
		}
	}
	return true
}

// Admits — имя, которое модель может написать: из задачи, известный ochan
// объект, новая ячейка или имя со смыслом.
func (n *Names) Admits(name string, lex *Lexicon, maxMeanings int) bool {
	return IsKeyword(name) || n.allows(name) || Meaningful(name, lex, maxMeanings)
}

// ForeignNames — имена в тексте модели src, которых нет в задаче, не знает
// ochan и которые не составлены из слов языка. По находке на имя — первой
// в тексте. r — итог разбора src: из него берётся словарь W (вместе со
// скиллами, которые подключает задача).
func (r *Result) ForeignNames(src string, names *Names, maxMeanings int) []Diagnostic {
	lex := r.Lexicon

	var out []Diagnostic
	seen := map[string]bool{}
	for _, ln := range splitLines(src) {
		if ln.err != nil {
			continue
		}
		if word := firstWord(ln.raw); word == "binds_to" || word == "import" {
			if names == nil || !names.lines[ln.raw] {
				out = append(out, Diagnostic{Pos: Pos{ln.no, ln.indent + 1}, Code: ErrSyn,
					Msg: fmt.Sprintf("строка «%s» не из задачи: путь и версию модель берёт из задачи как есть", ln.raw)})
			}
			continue
		}
		for _, t := range ln.toks {
			if t.kind != tIdent || seen[t.text] {
				continue
			}
			name := t.text
			switch {
			case isClassName(name), names.Admits(name, lex, maxMeanings):
				continue
			}
			seen[name] = true
			out = append(out, Diagnostic{Pos: Pos{ln.no, t.col}, Code: ErrSyn,
				Msg: fmt.Sprintf("имя «%s» не из задачи и не из словаря объектов ochan, а слов языка в нём нет: "+
					"модель имён не придумывает — открытым именем она говорила бы мимо языка", name)})
		}
	}
	return out
}
