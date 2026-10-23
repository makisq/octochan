package lang

import (
	"fmt"
	"strings"
	"unicode"
)

// Lexer разбирает исходный текст на лексемы.
type Lexer struct {
	src   []rune
	file  string
	pos   int
	line  int
	col   int
	depth int // глубина вложенности ( и [
	// braces — глубина ( и [ снаружи каждой открытой {.
	braces []int
}

// NewLexer создаёт лексер для исходника src с именем файла file.
func NewLexer(file, src string) *Lexer {
	return &Lexer{
		src:  []rune(src),
		file: file,
		line: 1,
		col:  1,
	}
}

// Tokenize возвращает все лексемы вплоть до конца файла.
func (l *Lexer) Tokenize() ([]Token, error) {
	var tokens []Token
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, tok)
		if tok.Kind == TokenEOF {
			return tokens, nil
		}
	}
}

func (l *Lexer) position() Position {
	return Position{File: l.file, Line: l.line, Col: l.col}
}

func (l *Lexer) peek() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *Lexer) peekAt(offset int) rune {
	if l.pos+offset >= len(l.src) {
		return 0
	}
	return l.src[l.pos+offset]
}

func (l *Lexer) advance() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	r := l.src[l.pos]
	l.pos++
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *Lexer) errorf(pos Position, format string, args ...any) error {
	return fmt.Errorf("%s: %s", pos, fmt.Sprintf(format, args...))
}

func (l *Lexer) next() (Token, error) {
	l.skipSpaceAndComments()

	pos := l.position()
	if l.pos >= len(l.src) {
		return Token{Kind: TokenEOF, Pos: pos}, nil
	}

	r := l.peek()

	if r == '\n' {
		l.advance()
		// Внутри скобок перевод строки незначим: это позволяет переносить
		// длинные списки аргументов, не ломая разбор.
		if l.depth > 0 {
			return l.next()
		}
		return Token{Kind: TokenNewline, Pos: pos}, nil
	}

	switch {
	case isIdentStart(r):
		return l.lexIdent(pos), nil
	case unicode.IsDigit(r):
		return l.lexNumber(pos)
	case r == '"' || r == '\'':
		return l.lexString(pos)
	}

	return l.lexOperator(pos)
}

func (l *Lexer) skipSpaceAndComments() {
	for l.pos < len(l.src) {
		r := l.peek()
		switch {
		case r == ' ' || r == '\t' || r == '\r':
			l.advance()
		case r == '#':
			for l.pos < len(l.src) && l.peek() != '\n' {
				l.advance()
			}
		case r == '\\' && l.peekAt(1) == '\n':
			// Явный перенос строки.
			l.advance()
			l.advance()
		default:
			return
		}
	}
}

// isIdentStart разрешает любые буквы Unicode, поэтому имена алиасов
// и переменных можно писать по-русски.
func isIdentStart(r rune) bool {
	return unicode.IsLetter(r) || r == '_'
}

func isIdentPart(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func (l *Lexer) lexIdent(pos Position) Token {
	var b strings.Builder
	for l.pos < len(l.src) && isIdentPart(l.peek()) {
		b.WriteRune(l.advance())
	}

	text := b.String()
	if kind, ok := keywords[text]; ok {
		return Token{Kind: kind, Text: text, Pos: pos}
	}
	return Token{Kind: TokenIdent, Text: text, Pos: pos}
}

func (l *Lexer) lexNumber(pos Position) (Token, error) {
	var b strings.Builder
	isFloat := false

	for l.pos < len(l.src) {
		r := l.peek()
		if unicode.IsDigit(r) {
			b.WriteRune(l.advance())
			continue
		}
		// Точка считается частью числа, только если за ней идёт цифра:
		// иначе 1.foo разберётся неверно.
		if r == '.' && !isFloat && unicode.IsDigit(l.peekAt(1)) {
			isFloat = true
			b.WriteRune(l.advance())
			continue
		}
		if r == '_' {
			l.advance() // разделитель разрядов: 1_000_000
			continue
		}
		break
	}

	// Суффикс единиц измерения запрещаем явно: 8GB — это строка, а не число,
	// иначе выражение легко прочитать не так, как задумано.
	if l.pos < len(l.src) && isIdentStart(l.peek()) {
		return Token{}, l.errorf(pos,
			"после числа %s идёт %q; значения с единицами измерения пишите строкой, например \"8GB\"",
			b.String(), string(l.peek()))
	}

	kind := TokenInt
	if isFloat {
		kind = TokenFloat
	}
	return Token{Kind: kind, Text: b.String(), Pos: pos}, nil
}

func (l *Lexer) lexString(pos Position) (Token, error) {
	quote := l.advance()

	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return Token{}, l.errorf(pos, "незакрытая строка")
		}

		r := l.advance()
		if r == quote {
			return Token{Kind: TokenString, Text: b.String(), Pos: pos}, nil
		}
		if r == '\n' {
			return Token{}, l.errorf(pos, "перевод строки внутри строкового литерала")
		}

		if r != '\\' {
			b.WriteRune(r)
			continue
		}

		esc := l.advance()
		switch esc {
		case 'n':
			b.WriteRune('\n')
		case 't':
			b.WriteRune('\t')
		case 'r':
			b.WriteRune('\r')
		case '\\', '\'', '"':
			b.WriteRune(esc)
		case 0:
			return Token{}, l.errorf(pos, "незакрытая строка")
		default:
			return Token{}, l.errorf(l.position(), "неизвестная escape-последовательность \\%c", esc)
		}
	}
}

func (l *Lexer) lexOperator(pos Position) (Token, error) {
	r := l.advance()

	simple := func(kind TokenKind) (Token, error) {
		switch kind {
		case TokenLParen, TokenLBracket:
			l.depth++
		case TokenRParen, TokenRBracket:
			if l.depth > 0 {
				l.depth--
			}
		}
		return Token{Kind: kind, Pos: pos}, nil
	}

	switch r {
	case '(':
		return simple(TokenLParen)
	case ')':
		return simple(TokenRParen)
	case '[':
		return simple(TokenLBracket)
	case ']':
		return simple(TokenRBracket)
	case '{':
		// Внутри фигурных скобок переводы строк снова значимы, даже если
		// сами скобки стоят в круглых: тело функции, переданной
		// аргументом, — это инструкции по строкам.
		l.braces = append(l.braces, l.depth)
		l.depth = 0
		return simple(TokenLBrace)
	case '}':
		if n := len(l.braces); n > 0 {
			l.depth = l.braces[n-1]
			l.braces = l.braces[:n-1]
		}
		return simple(TokenRBrace)
	case ',':
		return simple(TokenComma)
	case ';':
		return simple(TokenSemicolon)
	case ':':
		// Двоеточие отделяет имя от объявленного типа: x: строка = ...
		return simple(TokenColon)
	case '.':
		// Точка разделяет модуль и алиас: devops.проверить().
		// Дробные числа сюда не попадают — их разбирает lexNumber.
		return simple(TokenDot)
	case '+':
		return simple(TokenPlus)
	case '-':
		if l.peek() == '>' {
			l.advance()
			return simple(TokenArrow)
		}
		return simple(TokenMinus)
	case '*':
		return simple(TokenStar)
	case '/':
		return simple(TokenSlash)
	case '%':
		return simple(TokenPercent)
	case '|':
		if l.peek() == '|' {
			l.advance()
			return Token{}, l.errorf(pos, "оператор || не поддерживается, используйте or")
		}
		return simple(TokenPipe)
	case '&':
		if l.peek() == '&' {
			l.advance()
			return Token{}, l.errorf(pos, "оператор && не поддерживается, используйте and")
		}
		return Token{}, l.errorf(pos, "неожиданный символ &")
	case '=':
		switch l.peek() {
		case '=':
			l.advance()
			return simple(TokenEQ)
		case '>':
			l.advance()
			return simple(TokenFatArrow)
		}
		return simple(TokenAssign)
	case '!':
		if l.peek() == '=' {
			l.advance()
			return simple(TokenNE)
		}
		return Token{}, l.errorf(pos, "неожиданный символ !, отрицание пишется как not")
	case '<':
		if l.peek() == '=' {
			l.advance()
			return simple(TokenLE)
		}
		return simple(TokenLT)
	case '>':
		switch l.peek() {
		case '=':
			l.advance()
			return simple(TokenGE)
		case '>':
			l.advance()
			return simple(TokenCompose)
		}
		return simple(TokenGT)
	}

	return Token{}, l.errorf(pos, "неожиданный символ %q", string(r))
}
