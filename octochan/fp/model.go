package fp

import (
	"strings"
	"unicode"
)

// Текст, который пишет модель.
//
// Модель отвечает утверждениями языка (П1): ни приветствий, ни
// рассуждений. Принадлежность языку проверяет тот же разбор, что у
// промпта. Сверх него у текста модели нет комментариев и нет букв вне
// ASCII: комментарий — естественный язык, через него модель говорила бы
// мимо языка, а сам язык — замкнутое подмножество английского.

// ModelLexemes — то, чего в тексте модели быть не может: комментарий,
// символ вне ASCII, управляющий символ. По находке на строку — первой.
func ModelLexemes(src string) []Diagnostic {
	var out []Diagnostic
	for i, line := range strings.Split(src, "\n") {
		line = strings.TrimRight(line, "\r")
		for col, r := range []rune(line) {
			msg := ""
			switch {
			case r == '#':
				msg = "комментарий в тексте модели: комментарий — естественный язык, модель пишет только утверждения языка"
			case r > unicode.MaxASCII:
				msg = "символ «" + string(r) + "» вне ASCII: язык — замкнутое подмножество английского, модель пишет только его"
			case r < ' ':
				msg = "управляющий символ в тексте модели"
			}
			if msg != "" {
				out = append(out, Diagnostic{Pos: Pos{i + 1, col + 1}, Code: ErrSyn, Msg: msg})
				break
			}
		}
	}
	return out
}

// CheckModelText — CheckText для текста модели: разбор, проверки и
// ModelLexemes.
func CheckModelText(name, src string, opts Options) *Result {
	r := CheckText(name, src, opts)
	if extra := ModelLexemes(src); len(extra) > 0 {
		r.Diagnostics = append(extra, r.Diagnostics...)
		sortDiagnostics(r.Diagnostics)
	}
	return r
}

// InLanguage — текст принадлежит языку: в нём нет ни одного ⊥syn.
// Остальные отказы (⊥ambig, ⊥undef, ⊥conflict) — о задаче, а не о языке:
// сообщение, на которое ответят «не хватает знания», написано на языке.
func (r *Result) InLanguage() bool {
	for _, d := range r.Diagnostics {
		if d.Code == ErrSyn {
			return false
		}
	}
	return true
}

// Syntax — находки ⊥syn: почему текст не на языке.
func (r *Result) Syntax() []Diagnostic {
	var out []Diagnostic
	for _, d := range r.Diagnostics {
		if d.Code == ErrSyn {
			out = append(out, d)
		}
	}
	return out
}

// StatementSyntax — что мешает дописать утверждение statement к тексту
// prefix: находки ⊥syn текста модели prefix+statement, кроме тех, что
// снимет продолжение (Incomplete). Пусто — утверждение на языке.
//
// Утверждение — строка: разбор языка построчный, и строка — наименьшая
// часть текста, которую можно проверить, не зная продолжения. Поэтому
// декодер встроенной модели проверяет язык по строкам и откатывается к
// началу строки. Находка может прийти и в прежней строке: пустой раздел
// перестаёт быть «ещё не дописанным», когда за ним начат другой.
func StatementSyntax(prefix, statement string, opts Options) []Diagnostic {
	r := CheckModelText("model", prefix+statement, opts)
	var out []Diagnostic
	for _, d := range r.Diagnostics {
		if d.Code == ErrSyn && !d.Incomplete {
			out = append(out, d)
		}
	}
	return out
}
