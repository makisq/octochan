package lang

import (
	"fmt"
	"strconv"
	"strings"
)

// Parser — рекурсивный спуск с восхождением по приоритетам.
type Parser struct {
	tokens []Token
	pos    int
	src    []string // строки исходника для комментариев-описаний
}

// Parse разбирает исходник в файл с объявлениями и инструкциями.
func Parse(name, src string) (*File, error) {
	tokens, err := NewLexer(name, src).Tokenize()
	if err != nil {
		return nil, err
	}

	p := &Parser{tokens: tokens, src: strings.Split(src, "\n")}
	return p.parseFile(name)
}

func (p *Parser) peek() Token { return p.tokens[p.pos] }
func (p *Parser) atEOF() bool { return p.peek().Kind == TokenEOF }
func (p *Parser) advance() Token {
	tok := p.tokens[p.pos]
	if tok.Kind != TokenEOF {
		p.pos++
	}
	return tok
}

func (p *Parser) check(kind TokenKind) bool { return p.peek().Kind == kind }

func (p *Parser) match(kinds ...TokenKind) bool {
	for _, kind := range kinds {
		if p.check(kind) {
			p.advance()
			return true
		}
	}
	return false
}

func (p *Parser) expect(kind TokenKind, context string) (Token, error) {
	if p.check(kind) {
		return p.advance(), nil
	}
	return Token{}, p.errorf(p.peek().Pos, "ожидалось %s %s, получено %s",
		kind, context, p.peek())
}

func (p *Parser) errorf(pos Position, format string, args ...any) error {
	return fmt.Errorf("%s: %s", pos, fmt.Sprintf(format, args...))
}

// skipNewlines пропускает пустые строки и точки с запятой.
func (p *Parser) skipNewlines() {
	for p.check(TokenNewline) || p.check(TokenSemicolon) {
		p.advance()
	}
}

func (p *Parser) parseFile(name string) (*File, error) {
	file := &File{Name: name}

	for {
		p.skipNewlines()
		if p.atEOF() {
			return file, nil
		}

		if p.check(TokenAlias) {
			decl, err := p.parseAlias()
			if err != nil {
				return nil, err
			}
			file.Aliases = append(file.Aliases, decl)
			continue
		}

		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		file.Stmts = append(file.Stmts, stmt)
	}
}

// docComment собирает комментарии, идущие непосредственно перед строкой line.
func (p *Parser) docComment(line int) string {
	var lines []string
	for i := line - 2; i >= 0 && i < len(p.src); i-- {
		trimmed := strings.TrimSpace(p.src[i])
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
		lines = append([]string{strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))}, lines...)
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

func (p *Parser) parseAlias() (*AliasDecl, error) {
	aliasTok := p.advance() // alias

	nameTok, err := p.expect(TokenIdent, "— имя алиаса")
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(TokenLParen, "после имени алиаса"); err != nil {
		return nil, err
	}
	params, paramTypes, err := p.parseParams()
	if err != nil {
		return nil, err
	}

	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	return &AliasDecl{
		Pos:        aliasTok.Pos,
		Name:       nameTok.Text,
		Params:     params,
		ParamTypes: paramTypes,
		Body:       body,
		Doc:        p.docComment(aliasTok.Pos.Line),
	}, nil
}

// parseParams разбирает параметры после открывающей скобки и закрывающую
// скобку: имена с необязательными типами.
func (p *Parser) parseParams() ([]string, []TypeName, error) {
	var (
		params     []string
		paramTypes []TypeName
	)
	seen := make(map[string]bool)
	for !p.check(TokenRParen) {
		paramTok, err := p.expect(TokenIdent, "— имя параметра")
		if err != nil {
			return nil, nil, err
		}
		if seen[paramTok.Text] {
			return nil, nil, p.errorf(paramTok.Pos, "параметр %q объявлен дважды", paramTok.Text)
		}
		seen[paramTok.Text] = true
		params = append(params, paramTok.Text)

		declared, err := p.parseOptionalType()
		if err != nil {
			return nil, nil, err
		}
		paramTypes = append(paramTypes, declared)

		if !p.match(TokenComma) {
			break
		}
	}

	if _, err := p.expect(TokenRParen, "в списке параметров"); err != nil {
		return nil, nil, err
	}
	return params, paramTypes, nil
}

// parseOptionalType разбирает необязательную аннотацию `: тип`.
// Отсутствие аннотации — не ошибка: типы добровольные.
func (p *Parser) parseOptionalType() (TypeName, error) {
	if !p.match(TokenColon) {
		return "", nil
	}
	if p.match(TokenFn) {
		return TypeFunc, nil
	}

	typeTok, err := p.expect(TokenIdent, "— имя типа после двоеточия")
	if err != nil {
		return "", err
	}

	declared, err := ParseTypeName(typeTok.Text)
	if err != nil {
		return "", p.errorf(typeTok.Pos, "%v", err)
	}
	return declared, nil
}

func (p *Parser) parseBlock() (*Block, error) {
	p.skipNewlines()

	openTok, err := p.expect(TokenLBrace, "— начало блока")
	if err != nil {
		return nil, err
	}

	block := &Block{Pos: openTok.Pos}
	for {
		p.skipNewlines()

		if p.check(TokenRBrace) {
			p.advance()
			return block, nil
		}
		if p.atEOF() {
			return nil, p.errorf(openTok.Pos, "блок не закрыт: нет }")
		}

		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		block.Stmts = append(block.Stmts, stmt)
	}
}

func (p *Parser) parseStatement() (Stmt, error) {
	switch p.peek().Kind {
	case TokenIf:
		return p.parseIf()
	case TokenFor:
		return p.parseFor(false)
	case TokenParallel:
		parallelTok := p.advance()
		if !p.check(TokenFor) {
			return nil, p.errorf(parallelTok.Pos, "после parallel ожидается for")
		}
		return p.parseFor(true)
	case TokenTry:
		return p.parseTry()
	case TokenReturn:
		return p.parseReturn()
	case TokenBreak:
		tok := p.advance()
		return &Break{Pos: tok.Pos}, p.endStatement()
	case TokenContinue:
		tok := p.advance()
		return &Continue{Pos: tok.Pos}, p.endStatement()
	case TokenLBrace:
		return p.parseBlock()
	case TokenAlias:
		return nil, p.errorf(p.peek().Pos, "алиас можно объявлять только на верхнем уровне файла")
	case TokenVar:
		return p.parseVar()
	case TokenLet:
		return p.parseLet()
	}

	// Присваивание: идентификатор, за которым идёт '=' или ': тип ='.
	// Обычное '==' сюда не попадает, потому что это отдельный токен.
	next := p.tokens[p.pos+1].Kind
	if p.check(TokenIdent) && (next == TokenAssign || next == TokenColon) {
		nameTok := p.advance()

		declared, err := p.parseOptionalType()
		if err != nil {
			return nil, err
		}

		if _, err := p.expect(TokenAssign, "после имени переменной"); err != nil {
			return nil, err
		}

		value, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		nameFunc(value, nameTok.Text)
		return &Assign{
			Pos:      nameTok.Pos,
			Name:     nameTok.Text,
			Declare:  declared != "",
			Declared: declared,
			Value:    value,
		}, p.endStatement()
	}

	pos := p.peek().Pos
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &ExprStmt{Pos: pos, Expr: expr}, p.endStatement()
}

// parseVar разбирает объявление переменной:
//
//	var имя: тип = значение
//	var имя = значение      — тип задаёт значение
//	var имя: тип            — нулевое значение типа
func (p *Parser) parseVar() (Stmt, error) {
	varTok := p.advance()
	nameTok, err := p.expect(TokenIdent, "— имя переменной после var")
	if err != nil {
		return nil, err
	}

	declared, err := p.parseOptionalType()
	if err != nil {
		return nil, err
	}

	node := &Assign{Pos: varTok.Pos, Name: nameTok.Text, Declare: true, Declared: declared}
	if !p.match(TokenAssign) {
		if declared == "" {
			return nil, p.errorf(nameTok.Pos, "var %s: укажите тип (var %s: int) или значение (var %s = 0)",
				nameTok.Text, nameTok.Text, nameTok.Text)
		}
		return node, p.endStatement()
	}

	node.Value, err = p.parseExpr()
	if err != nil {
		return nil, err
	}
	nameFunc(node.Value, nameTok.Text)
	return node, p.endStatement()
}

// parseLet разбирает `let имя[: тип] = значение`: объявление переменной,
// которой нельзя присвоить новое значение.
func (p *Parser) parseLet() (Stmt, error) {
	letTok := p.advance()
	nameTok, err := p.expect(TokenIdent, "— имя после let")
	if err != nil {
		return nil, err
	}
	declared, err := p.parseOptionalType()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(TokenAssign, "— у let всегда есть значение: let "+nameTok.Text+" = …"); err != nil {
		return nil, err
	}
	value, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	nameFunc(value, nameTok.Text)
	return &Assign{Pos: letTok.Pos, Name: nameTok.Text, Declare: true, Declared: declared,
		Value: value, Const: true}, p.endStatement()
}

// nameFunc даёт функции, которую сразу присваивают переменной, имя этой
// переменной: под ним функция видит саму себя и может вызывать себя
// рекурсивно, а в сообщениях об ошибках видно, какая это функция.
func nameFunc(value Expr, name string) {
	if lit, ok := value.(*FuncLit); ok && lit.Name == "" {
		lit.Name = name
	}
}

// endStatement требует, чтобы инструкция завершалась переводом строки,
// точкой с запятой или закрывающей скобкой блока.
func (p *Parser) endStatement() error {
	switch p.peek().Kind {
	case TokenNewline, TokenSemicolon:
		p.advance()
		return nil
	case TokenRBrace, TokenEOF:
		return nil
	case TokenColon:
		// Скорее всего, это литерал словаря в начале инструкции: там
		// фигурная скобка означает блок, и разбор уходит не туда.
		return p.errorf(p.peek().Pos,
			"неожиданное двоеточие; если это словарь, присвойте его переменной "+
				"или оберните в скобки: x = {\"ключ\": значение}")
	default:
		return p.errorf(p.peek().Pos, "лишнее выражение после инструкции: %s", p.peek())
	}
}

func (p *Parser) parseIf() (Stmt, error) {
	ifTok := p.advance()

	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	then, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	node := &If{Pos: ifTok.Pos, Cond: cond, Then: then}

	// else может стоять на следующей строке после }.
	saved := p.pos
	p.skipNewlines()
	if !p.match(TokenElse) {
		p.pos = saved
		return node, nil
	}

	if p.check(TokenIf) {
		elseIf, err := p.parseIf()
		if err != nil {
			return nil, err
		}
		node.Else = elseIf
		return node, nil
	}

	elseBlock, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	node.Else = elseBlock
	return node, nil
}

// parseTry разбирает try { ... } catch [имя] { ... }.
//
// Ветка catch обязательна: try без неё означал бы «проглотить ошибку молча»,
// а это ровно то поведение, из-за которого сбои остаются незамеченными.
func (p *Parser) parseTry() (Stmt, error) {
	tryTok := p.advance()

	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	p.skipNewlines()
	if _, err := p.expect(TokenCatch, "после блока try"); err != nil {
		return nil, err
	}

	node := &Try{Pos: tryTok.Pos, Body: body}

	// Имя переменной необязательно: catch { ... } тоже допустим,
	// если текст ошибки не нужен.
	if p.check(TokenIdent) {
		node.ErrVar = p.advance().Text
	}

	catchBlock, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	node.Catch = catchBlock

	return node, nil
}

func (p *Parser) parseFor(parallel bool) (Stmt, error) {
	forTok := p.advance()

	varTok, err := p.expect(TokenIdent, "— переменная цикла")
	if err != nil {
		return nil, err
	}

	declared, err := p.parseOptionalType()
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(TokenIn, "после переменной цикла"); err != nil {
		return nil, err
	}

	iterable, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}

	return &For{
		Pos:      forTok.Pos,
		Variable: varTok.Text,
		Declared: declared,
		Iterable: iterable,
		Body:     body,
		Parallel: parallel,
	}, nil
}

func (p *Parser) parseReturn() (Stmt, error) {
	retTok := p.advance()

	// return без значения.
	if p.check(TokenNewline) || p.check(TokenSemicolon) || p.check(TokenRBrace) || p.atEOF() {
		return &Return{Pos: retTok.Pos}, p.endStatement()
	}

	value, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &Return{Pos: retTok.Pos, Value: value}, p.endStatement()
}

// ---------------------------------------------------------------------------
// Выражения
//
// Приоритеты, от низшего к высшему:
//
//	or
//	and
//	not
//	== != < <= > >=
//	|                 конвейер связывает крепче сравнения, чтобы
//	                  a | b != 0 читалось как (a | b) != 0
//	>>                композиция: x | f >> g — это x | (f >> g)
//	+ -
//	* / %
//	унарный -
//	вызов, индекс
// ---------------------------------------------------------------------------

func (p *Parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *Parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}

	for p.check(TokenOr) {
		opTok := p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &Binary{Pos: opTok.Pos, Op: TokenOr, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}

	for p.check(TokenAnd) {
		opTok := p.advance()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &Binary{Pos: opTok.Pos, Op: TokenAnd, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseNot() (Expr, error) {
	if p.check(TokenNot) {
		opTok := p.advance()
		operand, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &Unary{Pos: opTok.Pos, Op: TokenNot, Operand: operand}, nil
	}
	return p.parseComparison()
}

func (p *Parser) parseComparison() (Expr, error) {
	left, err := p.parsePipe()
	if err != nil {
		return nil, err
	}

	for {
		kind := p.peek().Kind
		switch kind {
		case TokenEQ, TokenNE, TokenLT, TokenLE, TokenGT, TokenGE:
			opTok := p.advance()
			right, err := p.parsePipe()
			if err != nil {
				return nil, err
			}
			left = &Binary{Pos: opTok.Pos, Op: kind, Left: left, Right: right}
		default:
			return left, nil
		}
	}
}

func (p *Parser) parsePipe() (Expr, error) {
	left, err := p.parseCompose()
	if err != nil {
		return nil, err
	}

	for p.check(TokenPipe) {
		opTok := p.advance()
		p.skipNewlines() // конвейер можно переносить на следующую строку

		right, err := p.parseCompose()
		if err != nil {
			return nil, err
		}
		left = &Binary{Pos: opTok.Pos, Op: TokenPipe, Left: left, Right: right}
	}
	return left, nil
}

// parseCompose разбирает композицию f >> g: сначала f, затем g.
func (p *Parser) parseCompose() (Expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}

	for p.check(TokenCompose) {
		opTok := p.advance()
		p.skipNewlines() // цепочку можно переносить, как и конвейер

		right, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		left = &Binary{Pos: opTok.Pos, Op: TokenCompose, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseAdditive() (Expr, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}

	for p.check(TokenPlus) || p.check(TokenMinus) {
		opTok := p.advance()
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &Binary{Pos: opTok.Pos, Op: opTok.Kind, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseMultiplicative() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}

	for p.check(TokenStar) || p.check(TokenSlash) || p.check(TokenPercent) {
		opTok := p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &Binary{Pos: opTok.Pos, Op: opTok.Kind, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseUnary() (Expr, error) {
	if p.check(TokenMinus) {
		opTok := p.advance()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &Unary{Pos: opTok.Pos, Op: TokenMinus, Operand: operand}, nil
	}
	return p.parsePostfix()
}

func (p *Parser) parsePostfix() (Expr, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	for {
		switch {
		case p.check(TokenLParen):
			// Имя — алиас, встроенная функция или переменная с функцией;
			// любое другое выражение должно дать функцию при выполнении.
			ident, _ := expr.(*Ident)
			call, err := p.parseCallArgs(ident)
			if err != nil {
				return nil, err
			}
			if ident == nil {
				call.Fn = expr
			}
			expr = call

		case p.check(TokenLBracket):
			openTok := p.advance()
			index, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(TokenRBracket, "после индекса"); err != nil {
				return nil, err
			}
			expr = &Index{Pos: openTok.Pos, Target: expr, Index: index}

		default:
			return expr, nil
		}
	}
}

func (p *Parser) parseCallArgs(callee *Ident) (*Call, error) {
	openTok := p.advance() // (

	var args []Expr
	for !p.check(TokenRParen) {
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)

		if !p.match(TokenComma) {
			break
		}
	}

	if _, err := p.expect(TokenRParen, "в списке аргументов"); err != nil {
		return nil, err
	}
	return &Call{Pos: openTok.Pos, Callee: callee, Args: args}, nil
}

func (p *Parser) parsePrimary() (Expr, error) {
	tok := p.peek()

	switch tok.Kind {
	case TokenInt:
		p.advance()
		n, err := strconv.ParseInt(tok.Text, 10, 64)
		if err != nil {
			return nil, p.errorf(tok.Pos, "не удалось разобрать целое %q: %v", tok.Text, err)
		}
		return &Literal{Pos: tok.Pos, Value: IntValue(n)}, nil

	case TokenFloat:
		p.advance()
		f, err := strconv.ParseFloat(tok.Text, 64)
		if err != nil {
			return nil, p.errorf(tok.Pos, "не удалось разобрать число %q: %v", tok.Text, err)
		}
		return &Literal{Pos: tok.Pos, Value: FloatValue(f)}, nil

	case TokenString:
		p.advance()
		return &Literal{Pos: tok.Pos, Value: StringValue(tok.Text)}, nil

	case TokenTrue:
		p.advance()
		return &Literal{Pos: tok.Pos, Value: BoolValue(true)}, nil

	case TokenFalse:
		p.advance()
		return &Literal{Pos: tok.Pos, Value: BoolValue(false)}, nil

	case TokenNull:
		p.advance()
		return &Literal{Pos: tok.Pos, Value: NullValue{}}, nil

	case TokenIdent:
		return p.parseIdent()

	case TokenLParen:
		p.advance()
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(TokenRParen, "— закрывающая скобка"); err != nil {
			return nil, err
		}
		return expr, nil

	case TokenLBracket:
		return p.parseList()

	case TokenLBrace:
		return p.parseMap()

	case TokenFn:
		return p.parseFn()
	}

	return nil, p.errorf(tok.Pos, "неожиданный %s в выражении", tok)
}

func (p *Parser) parseIdent() (Expr, error) {
	nameTok := p.advance()

	// Квалифицированное имя: модуль.алиас
	if p.check(TokenDot) {
		p.advance()
		memberTok, err := p.expect(TokenIdent, "после точки — имя алиаса из модуля")
		if err != nil {
			return nil, err
		}
		return &Ident{Pos: nameTok.Pos, Module: nameTok.Text, Name: memberTok.Text}, nil
	}

	return &Ident{Pos: nameTok.Pos, Name: nameTok.Text}, nil
}

// parseFn разбирает функцию-значение:
//
//	fn(x, y: int) { return x + y }   — тело-блок
//	fn(x) => x * 2                   — тело-выражение
//	fn(x: int): int => x * 2         — с типом результата
//	fn upper, fn devops.проверить    — ссылка на алиас или встроенную функцию
func (p *Parser) parseFn() (Expr, error) {
	fnTok := p.advance()

	if p.check(TokenIdent) {
		target, err := p.parseIdent()
		if err != nil {
			return nil, err
		}
		return &FuncRef{Pos: fnTok.Pos, Target: target.(*Ident)}, nil
	}

	if _, err := p.expect(TokenLParen, "после fn: параметры функции fn(x) или имя алиаса fn имя"); err != nil {
		return nil, err
	}
	params, paramTypes, err := p.parseParams()
	if err != nil {
		return nil, err
	}
	lit := &FuncLit{Pos: fnTok.Pos, Params: params, ParamTypes: paramTypes}
	if lit.Returns, err = p.parseOptionalType(); err != nil {
		return nil, err
	}

	if p.match(TokenFatArrow) {
		p.skipNewlines()
		if lit.Expr, err = p.parseExpr(); err != nil {
			return nil, err
		}
		return lit, nil
	}
	if !p.check(TokenLBrace) {
		return nil, p.errorf(p.peek().Pos, "после параметров функции ожидалось тело: { … } или => выражение, получено %s", p.peek())
	}
	if lit.Body, err = p.parseBlock(); err != nil {
		return nil, err
	}
	return lit, nil
}

// parseMap разбирает литерал словаря {ключ: значение, ...}.
//
// Вызывается только из parsePrimary, то есть там, где ожидается выражение.
// В начале инструкции { остаётся началом блока — иначе `if x { ... }`
// пришлось бы разбирать с оглядкой назад.
func (p *Parser) parseMap() (Expr, error) {
	openTok := p.advance() // {

	node := &MapLit{Pos: openTok.Pos}
	for {
		p.skipNewlines()
		if p.check(TokenRBrace) {
			break
		}

		key, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(TokenColon, "между ключом и значением словаря"); err != nil {
			return nil, err
		}
		value, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		node.Entries = append(node.Entries, MapEntry{Key: key, Value: value})

		p.skipNewlines()
		if !p.match(TokenComma) {
			break
		}
	}

	p.skipNewlines()
	if _, err := p.expect(TokenRBrace, "— закрывающая скобка словаря"); err != nil {
		return nil, err
	}
	return node, nil
}

func (p *Parser) parseList() (Expr, error) {
	openTok := p.advance() // [

	list := &ListLit{Pos: openTok.Pos}
	for !p.check(TokenRBracket) {
		element, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		list.Elements = append(list.Elements, element)

		if !p.match(TokenComma) {
			break
		}
	}

	if _, err := p.expect(TokenRBracket, "— закрывающая скобка списка"); err != nil {
		return nil, err
	}
	return list, nil
}
