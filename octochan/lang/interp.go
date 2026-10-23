package lang

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
)

// maxCallDepth защищает от бесконечной рекурсии: alias a() { a() }
// не должен вешать процесс.
const maxCallDepth = 200

// RuntimeError — ошибка выполнения с указанием места и цепочки вызовов.
type RuntimeError struct {
	Pos   Position
	Msg   string
	Stack []string
}

func (e *RuntimeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Pos, e.Msg)
	for i := len(e.Stack) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "\n  в алиасе %s", e.Stack[i])
	}
	return b.String()
}

// scope — окружение с переменными и ссылкой на родителя.
//
// Объявленные типы хранятся рядом со значениями: аннотация переменной
// действует не только в момент объявления, но и при всех последующих
// присваиваниях — иначе от неё было бы мало толку.
type scope struct {
	vars   map[string]Value
	types  map[string]TypeName
	parent *scope
}

func newScope(parent *scope) *scope {
	return &scope{
		vars:   make(map[string]Value),
		types:  make(map[string]TypeName),
		parent: parent,
	}
}

func (s *scope) lookup(name string) (Value, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if v, ok := cur.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// declaredType возвращает объявленный тип переменной, если он есть.
func (s *scope) declaredType(name string) (TypeName, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			t, hasType := cur.types[name]
			return t, hasType
		}
	}
	return "", false
}

// define объявляет переменную в текущей области с типом.
func (s *scope) define(name string, declared TypeName, value Value) {
	s.vars[name] = value
	if declared != "" {
		s.types[name] = declared
	} else {
		delete(s.types, name)
	}
}

// set присваивает переменную в ближайшей области, где она уже объявлена,
// иначе объявляет её в текущей.
func (s *scope) set(name string, value Value) {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			cur.vars[name] = value
			return
		}
	}
	s.vars[name] = value
}

// Interpreter выполняет разобранный код.
type Interpreter struct {
	registry *Registry
	builtins map[string]Builtin
	out      io.Writer
	ctx      context.Context

	// sandbox непуст, если встроенным функциям доступно не всё.
	sandbox *Sandbox
	// dir — каталог для относительных путей; пусто — текущий.
	dir string
	// dry непуст в режиме плана: записи попадают сюда, а не на диск.
	dry *overlay
	// effects — действия сценария за пределами интерпретатора.
	effects []Effect
	// calls — вызовы алиасов; callIDs — стек текущих вызовов; nextCall —
	// счётчик ID, общий для итераций parallel for.
	calls    []Invocation
	callIDs  []int
	nextCall *atomic.Int64

	stack []string
}

// NewInterpreter создаёт вычислитель поверх реестра алиасов.
func NewInterpreter(ctx context.Context, registry *Registry, out io.Writer) *Interpreter {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Interpreter{
		registry: registry,
		builtins: DefaultBuiltins(),
		out:      out,
		ctx:      ctx,
		nextCall: new(atomic.Int64),
	}
}

// Context возвращает контекст выполнения.
func (in *Interpreter) Context() context.Context { return in.ctx }

// Out возвращает поток вывода сценария.
func (in *Interpreter) Out() io.Writer { return in.out }

// Управляющие сигналы передаются как ошибки, чтобы не тащить их
// через каждое возвращаемое значение.
var (
	errBreak    = errors.New("break вне цикла")
	errContinue = errors.New("continue вне цикла")
)

type returnSignal struct{ value Value }

func (returnSignal) Error() string { return "return вне алиаса" }

func (in *Interpreter) errorf(pos Position, format string, args ...any) error {
	stack := make([]string, len(in.stack))
	copy(stack, in.stack)
	return &RuntimeError{Pos: pos, Msg: fmt.Sprintf(format, args...), Stack: stack}
}

// wrap добавляет позицию и стек к ошибке из встроенной функции.
func (in *Interpreter) wrap(pos Position, err error) error {
	var runtimeErr *RuntimeError
	if errors.As(err, &runtimeErr) {
		return err
	}
	return in.errorf(pos, "%v", err)
}

// RunFile выполняет инструкции верхнего уровня файла.
func (in *Interpreter) RunFile(file *File) error {
	env := newScope(nil)
	for _, stmt := range file.Stmts {
		if err := in.execStmt(stmt, env); err != nil {
			var ret returnSignal
			if errors.As(err, &ret) {
				return nil
			}
			return err
		}
	}
	return nil
}

// EvalExpr вычисляет одиночное выражение в пустом окружении.
// Нужен команде eval, чтобы печатать результат, а не выполнять его вхолостую.
func (in *Interpreter) EvalExpr(expr Expr) (Value, error) {
	return in.eval(expr, newScope(nil))
}

// CallAlias вызывает алиас по имени с готовыми значениями аргументов.
func (in *Interpreter) CallAlias(name string, args []Value) (Value, error) {
	decl, err := in.registry.Lookup("", name)
	if err != nil {
		return nil, err
	}
	return in.invoke(decl, args, Position{File: name})
}

// ---------------------------------------------------------------------------
// Инструкции
// ---------------------------------------------------------------------------

func (in *Interpreter) execStmt(stmt Stmt, env *scope) error {
	if err := in.ctx.Err(); err != nil {
		return in.errorf(stmt.Position(), "выполнение прервано: %v", err)
	}

	switch s := stmt.(type) {
	case *ExprStmt:
		_, err := in.eval(s.Expr, env)
		return err

	case *Assign:
		value, err := in.eval(s.Value, env)
		if err != nil {
			return err
		}
		return in.assign(s, env, value)

	case *Block:
		inner := newScope(env)
		for _, child := range s.Stmts {
			if err := in.execStmt(child, inner); err != nil {
				return err
			}
		}
		return nil

	case *If:
		return in.execIf(s, env)

	case *For:
		return in.execFor(s, env)

	case *Try:
		return in.execTry(s, env)

	case *Return:
		if s.Value == nil {
			return returnSignal{value: NullValue{}}
		}
		value, err := in.eval(s.Value, env)
		if err != nil {
			return err
		}
		return returnSignal{value: value}

	case *Break:
		return errBreak

	case *Continue:
		return errContinue
	}

	return in.errorf(stmt.Position(), "неизвестная инструкция %T", stmt)
}

// assign выполняет присваивание с учётом объявленного типа.
//
// Написанная аннотация объявляет переменную заново в текущей области.
// Без аннотации значение кладётся туда, где переменная уже есть, и при
// этом обязано соответствовать её прежнему объявленному типу.
func (in *Interpreter) assign(node *Assign, env *scope, value Value) error {
	if node.Declared != "" {
		if err := checkType(node.Declared, fmt.Sprintf("переменная %s", node.Name), value); err != nil {
			return in.wrap(node.Pos, err)
		}
		env.define(node.Name, node.Declared, value)
		return nil
	}

	if declared, ok := env.declaredType(node.Name); ok {
		if err := checkType(declared, fmt.Sprintf("переменная %s", node.Name), value); err != nil {
			return in.wrap(node.Pos, err)
		}
	}

	env.set(node.Name, value)
	return nil
}

func (in *Interpreter) execIf(node *If, env *scope) error {
	cond, err := in.eval(node.Cond, env)
	if err != nil {
		return err
	}

	if cond.Truthy() {
		return in.execStmt(node.Then, env)
	}
	if node.Else != nil {
		return in.execStmt(node.Else, env)
	}
	return nil
}

func (in *Interpreter) execFor(node *For, env *scope) error {
	iterable, err := in.eval(node.Iterable, env)
	if err != nil {
		return err
	}

	items, err := in.iterate(node.Pos, iterable)
	if err != nil {
		return err
	}

	if node.Parallel {
		return in.execForParallel(node, env, items)
	}

	for _, item := range items {
		if err := in.ctx.Err(); err != nil {
			return in.errorf(node.Pos, "выполнение прервано: %v", err)
		}

		if node.Declared != "" {
			if err := checkType(node.Declared,
				fmt.Sprintf("элемент цикла %s", node.Variable), item); err != nil {
				return in.wrap(node.Pos, err)
			}
		}

		// Тело получает свою область видимости на каждой итерации,
		// чтобы объявленные внутри переменные не текли между шагами.
		body := newScope(env)
		body.define(node.Variable, node.Declared, item)

		err := in.execStmt(node.Body, body)
		switch {
		case err == nil:
		case errors.Is(err, errBreak):
			return nil
		case errors.Is(err, errContinue):
			continue
		default:
			return err
		}
	}
	return nil
}

// iterate превращает значение в последовательность.
func (in *Interpreter) iterate(pos Position, v Value) ([]Value, error) {
	switch val := v.(type) {
	case ListValue:
		return val, nil
	case MapValue:
		// По словарю ходим ключами: значение достаётся как карта[ключ].
		keys := val.SortedKeys()
		items := make([]Value, len(keys))
		for i, key := range keys {
			items[i] = StringValue(key)
		}
		return items, nil
	case ConfigValue:
		keys := val.SortedKeys()
		items := make([]Value, len(keys))
		for i, key := range keys {
			items[i] = StringValue(key)
		}
		return items, nil
	case StringValue:
		// Строку обходим построчно: так удобно работать с выводом
		// текстовых операций через конвейер.
		if strings.TrimSpace(string(val)) == "" {
			return nil, nil
		}
		lines := strings.Split(strings.TrimRight(string(val), "\n"), "\n")
		items := make([]Value, len(lines))
		for i, line := range lines {
			items[i] = StringValue(line)
		}
		return items, nil
	case NullValue:
		return nil, nil
	default:
		return nil, in.errorf(pos, "по значению типа %s нельзя пройти циклом", v.TypeName())
	}
}

// ---------------------------------------------------------------------------
// Выражения
// ---------------------------------------------------------------------------

// eval вычисляет выражение.
func (in *Interpreter) eval(expr Expr, env *scope) (Value, error) {
	switch e := expr.(type) {
	case *Literal:
		return e.Value, nil

	case *Ident:
		return in.evalIdent(e, env)

	case *ListLit:
		items := make(ListValue, 0, len(e.Elements))
		for _, element := range e.Elements {
			value, err := in.eval(element, env)
			if err != nil {
				return nil, err
			}
			items = append(items, value)
		}
		return items, nil

	case *MapLit:
		entries := make(MapValue, len(e.Entries))
		for _, entry := range e.Entries {
			key, err := in.eval(entry.Key, env)
			if err != nil {
				return nil, err
			}
			value, err := in.eval(entry.Value, env)
			if err != nil {
				return nil, err
			}
			entries[key.Text()] = value
		}
		return entries, nil

	case *Unary:
		return in.evalUnary(e, env)

	case *Binary:
		return in.evalBinary(e, env)

	case *Call:
		return in.evalCall(e, env)

	case *Index:
		return in.evalIndex(e, env)
	}

	return nil, in.errorf(expr.Position(), "неизвестное выражение %T", expr)
}

func (in *Interpreter) evalIdent(e *Ident, env *scope) (Value, error) {
	if e.Module == "" {
		if value, ok := env.lookup(e.Name); ok {
			return value, nil
		}
		// Голое имя алиаса без скобок трактуем как вызов без аргументов:
		// это делает запись сценариев короче.
		if decl, err := in.registry.Lookup("", e.Name); err == nil {
			return in.invoke(decl, nil, e.Pos)
		}
		if _, ok := in.builtins[e.Name]; ok {
			return nil, in.errorf(e.Pos, "%s — встроенная функция, вызывайте её со скобками: %s()", e.Name, e.Name)
		}
		return nil, in.errorf(e.Pos, "неизвестное имя %q%s", e.Name, in.suggest(e.Name))
	}

	decl, err := in.registry.Lookup(e.Module, e.Name)
	if err != nil {
		return nil, in.wrap(e.Pos, err)
	}
	return in.invoke(decl, nil, e.Pos)
}

// suggest подсказывает похожее имя, если оно есть.
func (in *Interpreter) suggest(name string) string {
	var candidates []string
	for builtin := range in.builtins {
		candidates = append(candidates, builtin)
	}
	candidates = append(candidates, in.registry.Names()...)
	return suggestName(name, candidates)
}

// suggestName выбирает среди кандидатов самое похожее имя.
func suggestName(name string, candidates []string) string {
	// Кандидаты сортируются, чтобы при равном расстоянии подсказка не
	// зависела от порядка обхода map.
	sort.Strings(candidates)

	var best string
	bestDistance := 3 // не подсказываем при слишком далёких именах
	for _, candidate := range candidates {
		if d := editDistance(strings.ToLower(name), strings.ToLower(candidate)); d < bestDistance {
			best, bestDistance = candidate, d
		}
	}

	if best == "" {
		return ""
	}
	return fmt.Sprintf("; возможно, имелось в виду %q", best)
}

func (in *Interpreter) evalUnary(e *Unary, env *scope) (Value, error) {
	operand, err := in.eval(e.Operand, env)
	if err != nil {
		return nil, err
	}

	switch e.Op {
	case TokenNot:
		return BoolValue(!operand.Truthy()), nil
	case TokenMinus:
		n, ok := numeric(operand)
		if !ok {
			return nil, in.errorf(e.Pos, "унарный минус применим только к числу, получено %s", operand.TypeName())
		}
		if isInt(operand) {
			return IntValue(-int64(n)), nil
		}
		return FloatValue(-n), nil
	}

	return nil, in.errorf(e.Pos, "неизвестный унарный оператор %s", e.Op)
}

func (in *Interpreter) evalBinary(e *Binary, env *scope) (Value, error) {
	// and и or вычисляются лениво.
	switch e.Op {
	case TokenAnd:
		left, err := in.eval(e.Left, env)
		if err != nil {
			return nil, err
		}
		if !left.Truthy() {
			return BoolValue(false), nil
		}
		right, err := in.eval(e.Right, env)
		if err != nil {
			return nil, err
		}
		return BoolValue(right.Truthy()), nil

	case TokenOr:
		left, err := in.eval(e.Left, env)
		if err != nil {
			return nil, err
		}
		if left.Truthy() {
			return BoolValue(true), nil
		}
		right, err := in.eval(e.Right, env)
		if err != nil {
			return nil, err
		}
		return BoolValue(right.Truthy()), nil

	case TokenPipe:
		return in.evalPipe(e, env)
	}

	left, err := in.eval(e.Left, env)
	if err != nil {
		return nil, err
	}
	right, err := in.eval(e.Right, env)
	if err != nil {
		return nil, err
	}

	switch e.Op {
	case TokenEQ:
		return BoolValue(equalValues(left, right)), nil
	case TokenNE:
		return BoolValue(!equalValues(left, right)), nil
	case TokenLT, TokenLE, TokenGT, TokenGE:
		cmp, err := compareValues(left, right)
		if err != nil {
			return nil, in.wrap(e.Pos, err)
		}
		switch e.Op {
		case TokenLT:
			return BoolValue(cmp < 0), nil
		case TokenLE:
			return BoolValue(cmp <= 0), nil
		case TokenGT:
			return BoolValue(cmp > 0), nil
		default:
			return BoolValue(cmp >= 0), nil
		}
	}

	return in.arithmetic(e, left, right)
}

// evalPipe вычисляет a | f(b) как f(a, b).
//
// Первая версия передавала правой части текст левого значения. Так список
// расплющивался в строку и терял тип, а вызовы вроде lines(t) | len()
// не работали вовсе, потому что len требует аргумент. Подстановка значения
// первым аргументом сохраняет тип и объясняется одной строчкой.
func (in *Interpreter) evalPipe(e *Binary, env *scope) (Value, error) {
	left, err := in.eval(e.Left, env)
	if err != nil {
		return nil, err
	}

	switch right := e.Right.(type) {
	case *Call:
		args := make([]Value, 0, len(right.Args)+1)
		args = append(args, left)
		for _, arg := range right.Args {
			value, err := in.eval(arg, env)
			if err != nil {
				return nil, err
			}
			args = append(args, value)
		}
		return in.dispatch(right.Callee, args, right.Pos)

	case *Ident:
		// Имя без скобок: a | печать эквивалентно печать(a).
		return in.dispatch(right, []Value{left}, right.Pos)

	default:
		return nil, in.errorf(e.Pos, "справа от | должен быть вызов алиаса или встроенной функции")
	}
}

func (in *Interpreter) arithmetic(e *Binary, left, right Value) (Value, error) {
	// Сложение строк и списков — конкатенация.
	if e.Op == TokenPlus {
		if leftList, ok := left.(ListValue); ok {
			rightList, ok := right.(ListValue)
			if !ok {
				return nil, in.errorf(e.Pos, "к списку можно прибавить только список, получено %s", right.TypeName())
			}
			combined := make(ListValue, 0, len(leftList)+len(rightList))
			combined = append(combined, leftList...)
			combined = append(combined, rightList...)
			return combined, nil
		}
		_, leftNum := numeric(left)
		_, rightNum := numeric(right)
		if !leftNum || !rightNum {
			return StringValue(left.Text() + right.Text()), nil
		}
	}

	leftNum, ok := numeric(left)
	if !ok {
		return nil, in.errorf(e.Pos, "оператор %s неприменим к %s", e.Op, left.TypeName())
	}
	rightNum, ok := numeric(right)
	if !ok {
		return nil, in.errorf(e.Pos, "оператор %s неприменим к %s", e.Op, right.TypeName())
	}

	bothInt := isInt(left) && isInt(right)

	switch e.Op {
	case TokenPlus:
		return numberValue(leftNum+rightNum, bothInt), nil
	case TokenMinus:
		return numberValue(leftNum-rightNum, bothInt), nil
	case TokenStar:
		return numberValue(leftNum*rightNum, bothInt), nil
	case TokenSlash:
		if rightNum == 0 {
			return nil, in.errorf(e.Pos, "деление на ноль")
		}
		return numberValue(leftNum/rightNum, false), nil
	case TokenPercent:
		if int64(rightNum) == 0 {
			return nil, in.errorf(e.Pos, "деление на ноль")
		}
		return IntValue(int64(leftNum) % int64(rightNum)), nil
	}

	return nil, in.errorf(e.Pos, "неизвестный оператор %s", e.Op)
}

func numberValue(n float64, asInt bool) Value {
	if asInt && n == float64(int64(n)) {
		return IntValue(int64(n))
	}
	return FloatValue(n)
}

func (in *Interpreter) evalIndex(e *Index, env *scope) (Value, error) {
	target, err := in.eval(e.Target, env)
	if err != nil {
		return nil, err
	}
	indexValue, err := in.eval(e.Index, env)
	if err != nil {
		return nil, err
	}

	// Словарь и конфиг индексируются строковым ключом.
	switch container := target.(type) {
	case MapValue:
		key := indexValue.Text()
		value, ok := container[key]
		if !ok {
			return nil, in.errorf(e.Pos, "в словаре нет ключа %q; есть: %s",
				key, strings.Join(container.SortedKeys(), ", "))
		}
		return value, nil

	case ConfigValue:
		value, err := container.Lookup(indexValue.Text())
		if err != nil {
			return nil, in.wrap(e.Pos, err)
		}
		return StringValue(value), nil
	}

	n, ok := numeric(indexValue)
	if !ok {
		return nil, in.errorf(e.Pos, "индекс должен быть числом, получено %s", indexValue.TypeName())
	}

	list, ok := target.(ListValue)
	if !ok {
		return nil, in.errorf(e.Pos, "индексировать можно список, словарь или конфиг, получено %s", target.TypeName())
	}

	index := int(n)
	if index < 0 {
		index += len(list) // отрицательный индекс считается с конца
	}
	if index < 0 || index >= len(list) {
		return nil, in.errorf(e.Pos, "индекс %d вне списка длиной %d", int(n), len(list))
	}
	return list[index], nil
}

func (in *Interpreter) evalCall(e *Call, env *scope) (Value, error) {
	args := make([]Value, 0, len(e.Args))
	for _, arg := range e.Args {
		value, err := in.eval(arg, env)
		if err != nil {
			return nil, err
		}
		args = append(args, value)
	}
	return in.dispatch(e.Callee, args, e.Pos)
}

// dispatch вызывает алиас или встроенную функцию с готовыми аргументами.
// Общая точка для обычного вызова и для конвейера.
func (in *Interpreter) dispatch(callee *Ident, args []Value, pos Position) (Value, error) {
	// Алиасы имеют приоритет: пользователь может переопределить встроенное имя
	// собственным алиасом, и это его осознанный выбор.
	if decl, err := in.registry.Lookup(callee.Module, callee.Name); err == nil {
		return in.invoke(decl, args, pos)
	} else if callee.Module != "" {
		return nil, in.wrap(pos, err)
	}

	builtin, ok := in.builtins[callee.Name]
	if !ok {
		return nil, in.errorf(pos, "неизвестная функция %q%s", callee.Qualified(), in.suggest(callee.Name))
	}

	if err := builtin.checkArity(len(args)); err != nil {
		return nil, in.errorf(pos, "%s: %v", callee.Name, err)
	}

	result, err := builtin.Fn(in, args)
	if err != nil {
		return nil, in.wrap(pos, err)
	}
	return result, nil
}

// invoke выполняет тело алиаса.
func (in *Interpreter) invoke(decl *AliasDecl, args []Value, pos Position) (Value, error) {
	if len(args) != len(decl.Params) {
		return nil, in.errorf(pos, "алиас %s принимает %d %s, передано %d",
			decl.Name, len(decl.Params), plural(len(decl.Params), "аргумент", "аргумента", "аргументов"), len(args))
	}

	if len(in.stack) >= maxCallDepth {
		return nil, in.errorf(pos, "слишком глубокая вложенность вызовов (%d); похоже на бесконечную рекурсию", maxCallDepth)
	}

	in.stack = append(in.stack, decl.Name)
	defer func() { in.stack = in.stack[:len(in.stack)-1] }()
	defer in.beginCall(decl, args)()

	env := newScope(nil) // алиас не видит переменные вызывающего
	for i, param := range decl.Params {
		var declared TypeName
		if i < len(decl.ParamTypes) {
			declared = decl.ParamTypes[i]
		}

		// Проверка на входе — главный смысл аннотаций: ошибка всплывает
		// на границе алиаса, а не в его середине.
		if declared != "" {
			if err := checkType(declared,
				fmt.Sprintf("параметр %s алиаса %s", param, decl.Name), args[i]); err != nil {
				return nil, in.wrap(pos, err)
			}
		}
		env.define(param, declared, args[i])
	}

	for _, stmt := range decl.Body.Stmts {
		err := in.execStmt(stmt, env)
		if err == nil {
			continue
		}

		var ret returnSignal
		if errors.As(err, &ret) {
			return ret.value, nil
		}
		if errors.Is(err, errBreak) || errors.Is(err, errContinue) {
			return nil, in.errorf(stmt.Position(), "break и continue допустимы только внутри for")
		}
		return nil, err
	}

	return NullValue{}, nil
}

func plural(n int, one, few, many string) string {
	mod100 := n % 100
	if mod100 >= 11 && mod100 <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}

// editDistance — расстояние Левенштейна для подсказок по опечаткам.
func editDistance(a, b string) int {
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
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// BuiltinNames возвращает отсортированный список встроенных функций.
func (in *Interpreter) BuiltinNames() []string {
	names := make([]string, 0, len(in.builtins))
	for name := range in.builtins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Builtins возвращает описания встроенных функций.
func (in *Interpreter) Builtins() map[string]Builtin { return in.builtins }
