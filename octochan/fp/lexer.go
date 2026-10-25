package fp

import (
	"fmt"
	"strings"
	"unicode"
)

// Code — вид отказа. Обозначения взяты из манифеста.
type Code string

const (
	// ErrSyn — текст не принадлежит языку ℒ.
	ErrSyn Code = "⊥syn"
	// ErrUndef — не хватает знания: отношения, класса, скилла.
	ErrUndef Code = "⊥undef"
	// ErrAmbig — лексическая неоднозначность: |A(P)| > 1.
	ErrAmbig Code = "⊥ambig"
	// ErrConflict — противоречие: несовместные правила, цикл приоритетов,
	// повторное объявление.
	ErrConflict Code = "⊥conflict"
	// Note — замечание, не отказ.
	Note Code = "замечание"
)

// Diagnostic — находка разбора или проверки.
//
// Hint — законный путь: чем закрыть отказ, не нарушая протокола, —
// скилл, в котором есть операция, исключение правила, раздел протокола.
// Подсказку строит та же детерминированная проверка, она не догадка.
type Diagnostic struct {
	Pos  Pos
	Code Code
	Msg  string
	Hint string
}

// IsError сообщает, что находка — отказ, а не замечание.
func (d Diagnostic) IsError() bool { return d.Code != Note }

func (d Diagnostic) String() string {
	text := fmt.Sprintf("%s: %s: %s", d.Pos, d.Code, d.Msg)
	if d.Hint != "" {
		text += "\n    подсказка: " + d.Hint
	}
	return text
}

type tokKind int

const (
	tEnd tokKind = iota
	tIdent
	tNumber
	tString
	tPunct
)

type token struct {
	kind tokKind
	text string
	col  int
}

func (t token) describe() string {
	switch t.kind {
	case tEnd:
		return "конец строки"
	case tString:
		return fmt.Sprintf("строка %q", t.text)
	default:
		return fmt.Sprintf("«%s»", t.text)
	}
}

// srcLine — строка исходника после снятия комментария.
type srcLine struct {
	no     int
	indent int
	// raw — текст без отступа и комментария.
	raw  string
	toks []token
	err  *Diagnostic
}

// puncts перечислены от длинных к коротким: «-->» должно узнаваться
// раньше «--».
var puncts = []string{"-->", "--", ":=", "??", ":", ",", "(", ")", "{", "}", "=", ".", "<", "@", "/", "%"}

// splitLines режет исходник на строки, снимает комментарии и разбивает
// каждую на лексемы. Пустые строки и строки из одного комментария
// пропускаются.
func splitLines(src string) []srcLine {
	var lines []srcLine
	for i, text := range strings.Split(src, "\n") {
		text = strings.TrimRight(text, "\r")
		ln := srcLine{no: i + 1}

		body := strings.TrimLeft(text, " ")
		ln.indent = len([]rune(text)) - len([]rune(body))
		if strings.HasPrefix(body, "\t") {
			ln.err = &Diagnostic{Pos: Pos{ln.no, ln.indent + 1}, Code: ErrSyn,
				Msg: "табуляция в отступе: отступ пишется пробелами, иначе его глубина зависит от редактора"}
			lines = append(lines, ln)
			continue
		}

		// Путь к коду и имя скилла содержат «-» и «/», которых в остальном
		// языке нет. Такие строки разбираются по шаблону целиком.
		if word := firstWord(body); word == "binds_to" || word == "import" {
			raw := body
			if cut := strings.IndexByte(raw, '#'); cut >= 0 {
				raw = raw[:cut]
			}
			ln.raw = strings.TrimSpace(raw)
			ln.toks = []token{{tIdent, word, ln.indent + 1}}
			lines = append(lines, ln)
			continue
		}

		toks, raw, err := tokenize(body, ln.indent)
		if err != nil {
			err.Pos.Line = ln.no
			ln.err = err
		}
		if len(toks) == 0 && err == nil {
			continue
		}
		ln.raw = raw
		ln.toks = toks
		lines = append(lines, ln)
	}
	return lines
}

// tokenize разбивает строку без отступа на лексемы. Возвращает и текст
// строки без комментария: он нужен там, где разбор идёт по шаблону
// (путь в binds_to, версия скилла).
func tokenize(text string, indent int) ([]token, string, *Diagnostic) {
	runes := []rune(text)
	var toks []token
	end := len(runes)

	fail := func(i int, format string, args ...any) ([]token, string, *Diagnostic) {
		return nil, "", &Diagnostic{Pos: Pos{0, indent + i + 1}, Code: ErrSyn, Msg: fmt.Sprintf(format, args...)}
	}

	i := 0
	for i < len(runes) {
		r := runes[i]
		col := indent + i + 1

		switch {
		case r == ' ' || r == '\t':
			i++

		case r == '#':
			end = i
			i = len(runes)

		case r == '"':
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				j++
			}
			if j == len(runes) {
				return fail(i, "строка не закрыта: нет \"")
			}
			toks = append(toks, token{tString, string(runes[i+1 : j]), col})
			i = j + 1

		case unicode.IsDigit(r):
			j := i
			for j < len(runes) && unicode.IsDigit(runes[j]) {
				j++
			}
			if j+1 < len(runes) && runes[j] == '.' && unicode.IsDigit(runes[j+1]) {
				j++
				for j < len(runes) && unicode.IsDigit(runes[j]) {
					j++
				}
			}
			toks = append(toks, token{tNumber, string(runes[i:j]), col})
			i = j

		case unicode.IsLetter(r) || r == '_':
			j := i
			for j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j]) || runes[j] == '_') {
				j++
			}
			toks = append(toks, token{tIdent, string(runes[i:j]), col})
			i = j

		case r == '!' && i == 0 && (strings.HasPrefix(string(runes[1:]), "n_") || strings.HasPrefix(string(runes[1:]), "a_")):
			return fail(i, "маркер «!» не используется: он занят командами ochan (!install), а в C-подобных "+
				"языках читается как отрицание (открытый вопрос 7); пишите ??n_действие(...) или hard forbid действие(...)")

		case r == '!':
			return fail(i, "восклицание — не высказывание языка: модель здесь слой, а не собеседник (П1)")

		case r == '?' && (i+1 >= len(runes) || runes[i+1] != '?'):
			return fail(i, "вопрос — не высказывание языка: модель здесь слой, а не собеседник (П1)")

		default:
			matched := ""
			for _, p := range puncts {
				if strings.HasPrefix(string(runes[i:]), p) {
					matched = p
					break
				}
			}
			if matched == "" {
				return fail(i, "символ %q не входит в язык", r)
			}
			toks = append(toks, token{tPunct, matched, col})
			i += len([]rune(matched))
		}
	}
	return toks, strings.TrimSpace(string(runes[:end])), nil
}

func firstWord(text string) string {
	end := strings.IndexFunc(text, func(r rune) bool { return !(unicode.IsLetter(r) || r == '_') })
	if end < 0 {
		return text
	}
	return text[:end]
}

// isClassName — классы пишутся с заглавной латинской буквы: так класс
// отличается от имени отношения, слота или значения без оглядки на
// контекст.
func isClassName(s string) bool {
	return s != "" && s[0] >= 'A' && s[0] <= 'Z'
}

// isName — имя отношения, слота, ячейки, значения: со строчной буквы.
func isName(s string) bool {
	if s == "" {
		return false
	}
	r := []rune(s)[0]
	return r == '_' || unicode.IsLower(r)
}
