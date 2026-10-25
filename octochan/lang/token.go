// Package lang реализует язык алиасов octochan: небольшой интерпретируемый
// язык, в котором последовательность операций над конфигами оборачивается
// в именованный алиас с параметрами.
//
// Алиасы нельзя было сделать обычными командами cobra: у if, else и for есть
// тело, то есть блок вложенных инструкций, а у алиаса — ещё и собственное
// окружение со стеком вызовов. Поэтому здесь полноценные лексер, парсер
// и вычислитель, а команды CLI и язык одинаково опираются на пакет core.
package lang

import "fmt"

// TokenKind — вид лексемы.
type TokenKind int

const (
	TokenEOF TokenKind = iota
	TokenNewline
	TokenIdent
	TokenInt
	TokenFloat
	TokenString

	// Ключевые слова
	TokenAlias
	TokenVar
	TokenIf
	TokenElse
	TokenFor
	TokenIn
	TokenReturn
	TokenTry
	TokenCatch
	TokenParallel
	TokenBreak
	TokenContinue
	TokenAnd
	TokenOr
	TokenNot
	TokenTrue
	TokenFalse
	TokenNull
	TokenFn
	TokenLet
	TokenRecord
	TokenInterface
	TokenImport

	// Знаки
	TokenLParen
	TokenRParen
	TokenLBrace
	TokenRBrace
	TokenLBracket
	TokenRBracket
	TokenComma
	TokenSemicolon
	TokenDot
	TokenColon
	TokenAssign
	TokenPipe
	TokenEQ
	TokenNE
	TokenLT
	TokenLE
	TokenGT
	TokenGE
	TokenPlus
	TokenMinus
	TokenStar
	TokenSlash
	TokenPercent
	TokenFatArrow
	TokenCompose
	TokenArrow
)

var tokenNames = map[TokenKind]string{
	TokenEOF:       "конец файла",
	TokenNewline:   "перевод строки",
	TokenIdent:     "идентификатор",
	TokenInt:       "целое",
	TokenFloat:     "дробное",
	TokenString:    "строка",
	TokenAlias:     "alias",
	TokenVar:       "var",
	TokenIf:        "if",
	TokenElse:      "else",
	TokenFor:       "for",
	TokenIn:        "in",
	TokenReturn:    "return",
	TokenTry:       "try",
	TokenCatch:     "catch",
	TokenParallel:  "parallel",
	TokenBreak:     "break",
	TokenContinue:  "continue",
	TokenAnd:       "and",
	TokenOr:        "or",
	TokenNot:       "not",
	TokenTrue:      "true",
	TokenFalse:     "false",
	TokenNull:      "null",
	TokenFn:        "fn",
	TokenLet:       "let",
	TokenRecord:    "record",
	TokenInterface: "interface",
	TokenImport:    "import",
	TokenLParen:    "(",
	TokenRParen:    ")",
	TokenLBrace:    "{",
	TokenRBrace:    "}",
	TokenLBracket:  "[",
	TokenRBracket:  "]",
	TokenComma:     ",",
	TokenSemicolon: ";",
	TokenDot:       ".",
	TokenColon:     ":",
	TokenAssign:    "=",
	TokenPipe:      "|",
	TokenEQ:        "==",
	TokenNE:        "!=",
	TokenLT:        "<",
	TokenLE:        "<=",
	TokenGT:        ">",
	TokenGE:        ">=",
	TokenPlus:      "+",
	TokenMinus:     "-",
	TokenStar:      "*",
	TokenSlash:     "/",
	TokenPercent:   "%",
	TokenFatArrow:  "=>",
	TokenCompose:   ">>",
	TokenArrow:     "->",
}

func (k TokenKind) String() string {
	if name, ok := tokenNames[k]; ok {
		return name
	}
	return fmt.Sprintf("токен(%d)", int(k))
}

// keywords — зарезервированные слова. Записаны латиницей намеренно:
// имена алиасов и переменных при этом могут быть на любом языке.
var keywords = map[string]TokenKind{
	"alias":     TokenAlias,
	"var":       TokenVar,
	"if":        TokenIf,
	"else":      TokenElse,
	"for":       TokenFor,
	"in":        TokenIn,
	"return":    TokenReturn,
	"try":       TokenTry,
	"catch":     TokenCatch,
	"parallel":  TokenParallel,
	"break":     TokenBreak,
	"continue":  TokenContinue,
	"and":       TokenAnd,
	"or":        TokenOr,
	"not":       TokenNot,
	"true":      TokenTrue,
	"false":     TokenFalse,
	"null":      TokenNull,
	"fn":        TokenFn,
	"let":       TokenLet,
	"record":    TokenRecord,
	"interface": TokenInterface,
	// trait — то же, что interface: методы по умолчанию — алиасы, первый
	// параметр которых объявлен этим интерфейсом.
	"trait":  TokenInterface,
	"import": TokenImport,
}

// Position — место в исходном тексте, нужное для внятных сообщений об ошибках.
type Position struct {
	File string
	Line int
	Col  int
}

func (p Position) String() string {
	if p.File == "" {
		return fmt.Sprintf("строка %d, колонка %d", p.Line, p.Col)
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// Token — распознанная лексема.
type Token struct {
	Kind TokenKind
	Text string
	Pos  Position
}

func (t Token) String() string {
	if t.Text != "" {
		return fmt.Sprintf("%s (%q)", t.Kind, t.Text)
	}
	return t.Kind.String()
}
