package lang

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// maxCallDepth защищает от бесконечной рекурсии: alias a() { a() }
// не должен вешать процесс. Обычной рекурсии — обходу дерева, счёту по
// списку — этой глубины хватает с запасом.
const maxCallDepth = 1000

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
		switch frame := e.Stack[i]; {
		case frame == "fn":
			b.WriteString("\n  в функции")
		case strings.HasPrefix(frame, "fn "):
			fmt.Fprintf(&b, "\n  в функции %s", strings.TrimPrefix(frame, "fn "))
		case frame == "eval":
			b.WriteString("\n  в eval")
		default:
			fmt.Fprintf(&b, "\n  в алиасе %s", frame)
		}
	}
	return b.String()
}

// scope — окружение с переменными и ссылкой на родителя.
//
// Переменные строго типизированы: тип задаёт объявление, а без
// аннотации — первое значение, и дальше он не меняется. Типы хранятся
// рядом со значениями и проверяются при каждом присваивании.
type scope struct {
	vars  map[string]Value
	types map[string]TypeName
	// inferred — переменные, тип которых задан значением, а не написан:
	// это нужно только для понятного сообщения об ошибке.
	inferred map[string]bool
	// consts — переменные, объявленные через let.
	consts map[string]bool
	// frozen — снимок переменных, который видит функция: значения на
	// момент её создания, и присвоить им нельзя.
	frozen bool
	parent *scope
}

func newScope(parent *scope) *scope {
	return &scope{
		vars:     make(map[string]Value),
		types:    make(map[string]TypeName),
		inferred: make(map[string]bool),
		consts:   make(map[string]bool),
		parent:   parent,
	}
}

// owner — область, в которой объявлена переменная.
func (s *scope) owner(name string) *scope {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			return cur
		}
	}
	return nil
}

// capture — снимок видимых переменных для функции. Значения не
// копируются: словарь, изменённый через put, — тот же словарь; но
// присвоить переменной снимка функция не может.
func (s *scope) capture() *scope {
	flat := newScope(nil)
	flat.frozen = true

	var chain []*scope
	for cur := s; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for name, value := range chain[i].vars {
			flat.vars[name] = value
			flat.types[name] = chain[i].types[name]
			flat.inferred[name] = chain[i].inferred[name]
			flat.consts[name] = chain[i].consts[name]
		}
	}
	return flat
}

func (s *scope) lookup(name string) (Value, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if v, ok := cur.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// declaredType возвращает тип переменной и то, задан ли он значением.
func (s *scope) declaredType(name string) (TypeName, bool, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			return cur.types[name], cur.inferred[name], true
		}
	}
	return "", false, false
}

// declaredHere сообщает, что переменная объявлена в этой самой области.
func (s *scope) declaredHere(name string) bool {
	_, ok := s.vars[name]
	return ok
}

// define объявляет переменную в текущей области. Без написанного типа
// тип задаёт значение.
func (s *scope) define(name string, declared TypeName, value Value) {
	s.vars[name] = value
	s.inferred[name] = declared == ""
	if declared == "" {
		declared = typeOf(value)
	}
	s.types[name] = declared
}

// set присваивает переменной в ближайшей области, где она объявлена.
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
	// errOut — поток ошибок для print(stderr(), …).
	errOut io.Writer
	ctx    context.Context

	// sandbox непуст, если встроенным функциям доступно не всё.
	sandbox *Sandbox
	// protected — каталоги, закрытые для встроенных функций (SetProtected).
	protected []string
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

	// appending — файлы, в которые сейчас дописывает print: путь и номер
	// действия в effects. Печать подряд в один файл — одно действие.
	appending map[string]int
	// deferAppends — итерация parallel for: печать в файл копится в
	// pending и дописывается после цикла в порядке элементов.
	deferAppends bool
	pending      []pendingAppend

	stack []string

	// conforming — интерфейсы, соответствие которым проверяется сейчас.
	conforming map[*InterfaceDecl]bool

	// clock — время запуска сценария: его отдаёт now(). seed — зерно
	// rand(). Оба фиксируются в заявке: план и выполнение видят одно
	// время и одну случайность, иначе выполнилось бы не согласованное.
	clock time.Time
	seed  int64
	rng   *rand.Rand
}

// NewInterpreter создаёт вычислитель поверх реестра алиасов.
func NewInterpreter(ctx context.Context, registry *Registry, out io.Writer) *Interpreter {
	if ctx == nil {
		ctx = context.Background()
	}
	in := &Interpreter{
		registry: registry,
		builtins: DefaultBuiltins(),
		out:      out,
		errOut:   os.Stderr,
		ctx:      ctx,
		nextCall: new(atomic.Int64),
		clock:    time.Now(),
	}
	in.SetSeed(NewSeed())
	return in
}

// NewSeed — случайное зерно для rand().
func NewSeed() int64 {
	var raw [8]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.BigEndian.Uint64(raw[:]) >> 1)
}

// SetClock задаёт время запуска сценария: его вернёт now().
func (in *Interpreter) SetClock(t time.Time) { in.clock = t }

// Clock — время запуска сценария.
func (in *Interpreter) Clock() time.Time { return in.clock }

// SetSeed задаёт зерно rand(): с одним зерном сценарий выдаёт одни и те же
// случайные числа — в плане и при выполнении.
func (in *Interpreter) SetSeed(seed int64) {
	in.seed = seed
	in.rng = rand.New(rand.NewSource(seed))
}

// Seed — зерно rand().
func (in *Interpreter) Seed() int64 { return in.seed }

// SetErr задаёт поток ошибок сценария: туда пишет print(stderr(), …).
func (in *Interpreter) SetErr(w io.Writer) { in.errOut = w }

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
// Алиас пакета называется пакет.алиас, как и в тексте сценария.
func (in *Interpreter) CallAlias(name string, args []Value) (Value, error) {
	module, alias, qualified := strings.Cut(name, ".")
	if !qualified {
		module, alias = "", name
	}
	decl, err := in.registry.Lookup(module, alias)
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
		var value Value
		if s.Value != nil {
			var err error
			if value, err = in.eval(s.Value, env); err != nil {
				return err
			}
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

	case *SetStmt:
		return in.execSet(s, env)
	}

	return in.errorf(stmt.Position(), "неизвестная инструкция %T", stmt)
}

// execSet — x.поле = значение и x[ключ] = значение. Меняется сам объект,
// а не переменная, поэтому так можно и с переменной из let.
func (in *Interpreter) execSet(s *SetStmt, env *scope) error {
	value, err := in.eval(s.Value, env)
	if err != nil {
		return err
	}
	switch target := s.Target.(type) {
	case *Field:
		object, err := in.eval(target.Target, env)
		if err != nil {
			return err
		}
		if err := in.setMember(object, target.Name, value); err != nil {
			return in.wrap(s.Pos, err)
		}
	case *Index:
		object, err := in.eval(target.Target, env)
		if err != nil {
			return err
		}
		index, err := in.eval(target.Index, env)
		if err != nil {
			return err
		}
		if err := in.setIndex(object, index, value); err != nil {
			return in.wrap(s.Pos, err)
		}
	}
	return nil
}

// assign объявляет переменную или присваивает ей.
//
// Объявление заводит переменную в текущей области; повторное объявление
// в той же области — ошибка, во вложенной — новая переменная, которая
// затеняет внешнюю. Присваивание пишет туда, где переменная объявлена,
// и значение обязано подходить её типу. Присваивание необъявленной
// переменной объявляет её с типом значения.
func (in *Interpreter) assign(node *Assign, env *scope, value Value) error {
	if node.Declare {
		if env.declaredHere(node.Name) {
			return in.errorf(node.Pos, "переменная %s уже объявлена в этой области; "+
				"чтобы изменить значение, присвойте без var и без типа: %s = …", node.Name, node.Name)
		}
		if value == nil {
			zero, err := zeroValue(node.Declared)
			if err != nil {
				return in.wrap(node.Pos, fmt.Errorf("переменная %s: %w", node.Name, err))
			}
			value = zero
		}
		if err := in.checkType(node.Declared, fmt.Sprintf("переменная %s", node.Name), value); err != nil {
			return in.wrap(node.Pos, err)
		}
		env.define(node.Name, node.Declared, value)
		if node.Const {
			env.consts[node.Name] = true
		}
		return nil
	}

	declared, inferred, exists := env.declaredType(node.Name)
	if !exists {
		env.define(node.Name, "", value)
		return nil
	}
	if owner := env.owner(node.Name); owner.consts[node.Name] {
		return in.errorf(node.Pos, "%s объявлена через let, и её значение не меняется; "+
			"для изменяемой переменной объявите var %s", node.Name, node.Name)
	} else if owner.frozen {
		return in.errorf(node.Pos, "%s — переменная снаружи функции: функция видит её значение "+
			"на момент создания и не меняет его; объявите свою: var %s = …", node.Name, node.Name)
	}
	if !in.accepts(declared, value) {
		return in.wrap(node.Pos, reassignError(node.Name, declared, inferred, value))
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
			if err := in.checkType(node.Declared,
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

	case *FuncLit:
		return &FuncValue{
			Name:       e.Name,
			Params:     e.Params,
			ParamTypes: e.ParamTypes,
			Returns:    e.Returns,
			Body:       e.Body,
			Expr:       e.Expr,
			env:        env.capture(),
		}, nil

	case *FuncRef:
		return in.funcRef(e.Target, env)

	case *Field:
		target, err := in.eval(e.Target, env)
		if err != nil {
			return nil, err
		}
		value, err := in.fieldOf(target, e.Name)
		if err != nil {
			return nil, in.wrap(e.Pos, err)
		}
		return value, nil
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

	// x.поле, где x — переменная: переменная перекрывает пакет с тем же
	// именем, как и везде.
	if value, ok := env.lookup(e.Module); ok {
		field, err := in.fieldOf(value, e.Name)
		if err != nil {
			return nil, in.wrap(e.Pos, err)
		}
		return field, nil
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

	case TokenCompose:
		return in.evalCompose(e, env)

	case TokenArrow:
		return in.evalArrow(e, env)
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
		return in.evalCallWith(right, env, left)

	case *Ident:
		// Имя без скобок: a | печать эквивалентно печать(a).
		fn, err := in.varFunc(right, env)
		if err != nil {
			return nil, err
		}
		if fn != nil {
			return in.callValue(fn, []Value{left}, right.Pos)
		}
		return in.dispatch(right, []Value{left}, nil, right.Pos)

	default:
		// Справа — выражение, дающее функцию: x | f >> g, x | fn(v) => v * 2.
		fn, err := in.funcOperand(e.Right, env)
		if err != nil {
			return nil, err
		}
		return in.callValue(fn, []Value{left}, e.Pos)
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
	return in.evalCallWith(e, env, nil)
}

// evalCallWith вычисляет вызов; piped — значение слева от конвейера, оно
// становится первым аргументом.
//
// x.f(a): если у записи или словаря x есть поле f с функцией — вызывается
// она; иначе это f(x, a) — для алиаса, встроенной функции или переменной
// с функцией. a.b(…), где a — переменная, — то же самое: переменная
// перекрывает пакет с тем же именем.
func (in *Interpreter) evalCallWith(e *Call, env *scope, piped Value) (Value, error) {
	callee := e.Callee
	var receiver Value
	hasReceiver := false
	switch {
	case e.Receiver != nil:
		value, err := in.eval(e.Receiver, env)
		if err != nil {
			return nil, err
		}
		receiver, hasReceiver = value, true
	case callee != nil && callee.Module != "":
		if value, ok := env.lookup(callee.Module); ok {
			receiver, hasReceiver = value, true
			callee = &Ident{Pos: callee.Pos, Name: callee.Name}
		}
	}

	var args []Value
	var names []string
	if hasReceiver {
		if member, ok := memberOf(receiver, callee.Name); ok && isCallable(member) {
			// Поле-функция: вызывается без самого значения.
			args, names, err := in.callArgs(e, env, piped)
			if err != nil {
				return nil, err
			}
			return in.callNamed(member, args, names, e.Pos)
		}
		args = append(args, receiver)
		names = append(names, "")
	}

	fn, err := in.callTarget(&Call{Pos: e.Pos, Callee: callee, Fn: e.Fn}, env)
	if err != nil {
		return nil, err
	}
	rest, restNames, err := in.callArgs(e, env, piped)
	if err != nil {
		return nil, err
	}
	args = append(args, rest...)
	names = append(names, restNames...)
	if fn != nil {
		return in.callNamed(fn, args, names, e.Pos)
	}
	return in.dispatch(callee, args, names, e.Pos)
}

// callArgs вычисляет аргументы вызова; piped — первым.
func (in *Interpreter) callArgs(e *Call, env *scope, piped Value) ([]Value, []string, error) {
	args := make([]Value, 0, len(e.Args)+1)
	names := make([]string, 0, len(e.Args)+1)
	if piped != nil {
		args = append(args, piped)
		names = append(names, "")
	}
	for i, arg := range e.Args {
		value, err := in.eval(arg, env)
		if err != nil {
			return nil, nil, err
		}
		args = append(args, value)
		name := ""
		if i < len(e.Names) {
			name = e.Names[i]
		}
		names = append(names, name)
	}
	return args, names, nil
}

// unnamed — нет ли среди имён аргументов ни одного.
func unnamed(names []string) bool {
	for _, name := range names {
		if name != "" {
			return false
		}
	}
	return true
}

// isCallable — значение, которое можно вызвать: функция, морфизм или
// функтор.
func isCallable(v Value) bool {
	switch v.(type) {
	case *FuncValue, *MorphValue, *FunctorValue:
		return true
	}
	return false
}

// callValue вызывает функцию, применяет морфизм или функтор.
func (in *Interpreter) callValue(target Value, args []Value, pos Position) (Value, error) {
	return in.callNamed(target, args, nil, pos)
}

// callNamed — callValue с именами аргументов.
func (in *Interpreter) callNamed(target Value, args []Value, names []string, pos Position) (Value, error) {
	if !unnamed(names) {
		fn, ok := target.(*FuncValue)
		var params []string
		switch {
		case ok && fn.Decl != nil:
			params = fn.Decl.Params
		case ok && fn.Builtin == "" && fn.Base == nil && fn.Chain == nil:
			params = fn.Params
		default:
			return nil, in.errorf(pos, "%s принимает аргументы только по порядку", Display(target))
		}
		bound, err := fillArgs(Display(target), params, args, names)
		if err != nil {
			return nil, in.wrap(pos, err)
		}
		args = bound
	}
	switch t := target.(type) {
	case *FuncValue:
		return in.callFunc(t, args, pos)

	case *MorphValue:
		if len(args) != 1 {
			return nil, in.errorf(pos, "морфизм применяется к одному конфигу: m(c), передано %d", len(args))
		}
		c, err := objectOf(in, args[0])
		if err != nil {
			return nil, in.wrap(pos, err)
		}
		out, err := in.applyMorph(t, c)
		if err != nil {
			return nil, in.wrap(pos, err)
		}
		return out, nil

	case *FunctorValue:
		if len(args) < 1 || len(args) > 2 {
			return nil, in.errorf(pos, "функтор применяется к конфигу F(c) или к морфизму F(m[, c]), передано %d", len(args))
		}
		if m, ok := args[0].(*MorphValue); ok {
			var at *ConfigValue
			if len(args) == 2 {
				c, err := objectOf(in, args[1])
				if err != nil {
					return nil, in.wrap(pos, err)
				}
				at = &c
			}
			out, err := in.functorMorph(t, m, at)
			if err != nil {
				return nil, in.wrap(pos, err)
			}
			return out, nil
		}
		if len(args) == 2 {
			return nil, in.errorf(pos, "второй аргумент функтора — объект для морфизма: F(m, c)")
		}
		c, err := objectOf(in, args[0])
		if err != nil {
			return nil, in.wrap(pos, err)
		}
		out, err := in.functorObject(t, c)
		if err != nil {
			return nil, in.wrap(pos, err)
		}
		return out, nil
	}
	return nil, in.errorf(pos, "вызвать можно функцию, морфизм или функтор, получено %s (%s)", target.TypeName(), Display(target))
}

// callTarget находит значение, которое вызывает выражение: вычисленное
// выражение или переменную с функцией, морфизмом, функтором. nil —
// вызывается алиас или встроенная функция по имени.
func (in *Interpreter) callTarget(e *Call, env *scope) (Value, error) {
	if e.Fn == nil {
		return in.varFunc(e.Callee, env)
	}
	value, err := in.eval(e.Fn, env)
	if err != nil {
		return nil, err
	}
	if !isCallable(value) {
		return nil, in.errorf(e.Pos, "вызвать можно функцию, морфизм или функтор, получено %s (%s)", value.TypeName(), Display(value))
	}
	return value, nil
}

// varFunc — то, что лежит в переменной с этим именем, если это можно
// вызвать. Переменная с другим значением вызов не перехватывает: имя
// остаётся за алиасом или встроенной функцией, как было до функций-значений.
func (in *Interpreter) varFunc(callee *Ident, env *scope) (Value, error) {
	if callee.Module != "" {
		return nil, nil
	}
	value, ok := env.lookup(callee.Name)
	if !ok {
		return nil, nil
	}
	if isCallable(value) {
		return value, nil
	}
	if _, err := in.registry.Lookup("", callee.Name); err == nil {
		return nil, nil
	}
	if _, ok := in.builtins[callee.Name]; ok {
		return nil, nil
	}
	return nil, in.errorf(callee.Pos, "%s — не функция, а %s (%s)", callee.Name, value.TypeName(), Display(value))
}

// funcRef — алиас или встроенная функция как значение: fn имя. Имя
// переменной даёт то, что в ней, если это можно вызвать.
func (in *Interpreter) funcRef(target *Ident, env *scope) (Value, error) {
	if target.Module == "" {
		if value, ok := env.lookup(target.Name); ok {
			if isCallable(value) {
				return value, nil
			}
			return nil, in.errorf(target.Pos, "%s — не функция, а %s (%s)", target.Name, value.TypeName(), Display(value))
		}
	}
	decl, err := in.registry.Lookup(target.Module, target.Name)
	if err == nil {
		return &FuncValue{Decl: decl}, nil
	}
	if _, ok := in.builtins[target.Qualified()]; ok {
		return &FuncValue{Builtin: target.Qualified()}, nil
	}
	if target.Module != "" {
		return nil, in.wrap(target.Pos, err)
	}
	return nil, in.errorf(target.Pos, "неизвестная функция %q%s", target.Qualified(), in.suggest(target.Name))
}

// funcOperand вычисляет операнд, который должен быть функцией: часть
// композиции или правую часть конвейера. Голое имя алиаса здесь — сама
// функция, а не её вызов: в f >> g нужны функции, а не их результаты.
func (in *Interpreter) funcOperand(expr Expr, env *scope) (Value, error) {
	if id, ok := expr.(*Ident); ok {
		return in.funcRef(id, env)
	}
	value, err := in.eval(expr, env)
	if err != nil {
		return nil, err
	}
	if !isCallable(value) {
		return nil, in.errorf(expr.Position(), "ожидалась функция, морфизм или функтор, получено %s (%s)",
			value.TypeName(), Display(value))
	}
	return value, nil
}

// evalCompose — f >> g: сначала f, потом g. Для функций — функция, для
// морфизмов — морфизм со склейкой и проверкой стыковки, для функторов —
// функтор. id() нейтрален для всех трёх.
func (in *Interpreter) evalCompose(e *Binary, env *scope) (Value, error) {
	left, err := in.funcOperand(e.Left, env)
	if err != nil {
		return nil, err
	}
	right, err := in.funcOperand(e.Right, env)
	if err != nil {
		return nil, err
	}
	out, err := compose(left, right)
	if err != nil {
		return nil, in.wrap(e.Pos, err)
	}
	return out, nil
}

// evalArrow — a -> b: морфизм, переводящий конфиг a в конфиг b.
func (in *Interpreter) evalArrow(e *Binary, env *scope) (Value, error) {
	var objects [2]ConfigValue
	for i, operand := range []Expr{e.Left, e.Right} {
		value, err := in.eval(operand, env)
		if err != nil {
			return nil, err
		}
		if objects[i], err = objectOf(in, value); err != nil {
			return nil, in.errorf(operand.Position(), "-> соединяет конфиги: %v", err)
		}
	}
	return deltaBetween(objects[0], objects[1]), nil
}

// compose склеивает два вызываемых значения.
func compose(left, right Value) (Value, error) {
	if left == Value(identity) {
		return right, nil
	}
	if right == Value(identity) {
		return left, nil
	}
	lf, lFunctor := left.(*FunctorValue)
	rf, rFunctor := right.(*FunctorValue)
	switch {
	case lFunctor && rFunctor:
		var chain []*FunctorValue
		for _, f := range []*FunctorValue{lf, rf} {
			if f.chain != nil {
				chain = append(chain, f.chain...)
			} else {
				chain = append(chain, f)
			}
		}
		return &FunctorValue{chain: chain}, nil
	case lFunctor || rFunctor:
		return nil, fmt.Errorf("функтор композируется только с функтором, получено %s и %s", left.TypeName(), right.TypeName())
	}

	_, lMorph := left.(*MorphValue)
	_, rMorph := right.(*MorphValue)
	if lMorph || rMorph {
		a, err := morphArg(">>", left)
		if err != nil {
			return nil, err
		}
		b, err := morphArg(">>", right)
		if err != nil {
			return nil, err
		}
		return composeMorph(a, b)
	}
	return composeFuncs(left.(*FuncValue), right.(*FuncValue)), nil
}

// composeFuncs склеивает цепочки, чтобы f >> g >> h была одной цепочкой
// из трёх шагов, а не вложенной.
func composeFuncs(fns ...*FuncValue) *FuncValue {
	var chain []*FuncValue
	for _, fn := range fns {
		if fn.Chain != nil {
			chain = append(chain, fn.Chain...)
		} else {
			chain = append(chain, fn)
		}
	}
	return &FuncValue{Chain: chain}
}

// callFunc вызывает функцию-значение.
func (in *Interpreter) callFunc(fn *FuncValue, args []Value, pos Position) (Value, error) {
	switch {
	case fn.Decl != nil:
		return in.invoke(fn.Decl, args, pos)

	case fn.Builtin != "":
		builtin, ok := in.builtins[fn.Builtin]
		if !ok {
			return nil, in.errorf(pos, "неизвестная функция %q", fn.Builtin)
		}
		if err := builtin.checkArity(len(args)); err != nil {
			return nil, in.errorf(pos, "%s: %v", fn.Builtin, err)
		}
		result, err := builtin.Fn(in, args)
		if err != nil {
			return nil, in.wrap(pos, err)
		}
		return result, nil

	case fn.Base != nil:
		all := make([]Value, 0, len(fn.Bound)+len(args))
		all = append(all, fn.Bound...)
		all = append(all, args...)
		return in.callFunc(fn.Base, all, pos)

	case fn.Chain != nil:
		value, err := in.callFunc(fn.Chain[0], args, pos)
		if err != nil {
			return nil, err
		}
		for _, step := range fn.Chain[1:] {
			if value, err = in.callFunc(step, []Value{value}, pos); err != nil {
				return nil, err
			}
		}
		return value, nil
	}
	return in.callLambda(fn, args, pos)
}

// callLambda выполняет функцию, написанную через fn(…).
func (in *Interpreter) callLambda(fn *FuncValue, args []Value, pos Position) (Value, error) {
	frame := "fn"
	if fn.Name != "" {
		frame = "fn " + fn.Name
	}
	if len(args) != len(fn.Params) {
		return nil, in.errorf(pos, "функция %s принимает %d %s, передано %d", fn.Text(),
			len(fn.Params), plural(len(fn.Params), "аргумент", "аргумента", "аргументов"), len(args))
	}
	if len(in.stack) >= maxCallDepth {
		return nil, in.errorf(pos, "слишком глубокая вложенность вызовов (%d); похоже на бесконечную рекурсию", maxCallDepth)
	}
	in.stack = append(in.stack, frame)
	defer func() { in.stack = in.stack[:len(in.stack)-1] }()

	env := newScope(fn.env)
	if fn.Name != "" {
		// Под своим именем функция видит саму себя: так работает рекурсия.
		env.define(fn.Name, TypeFunc, fn)
		env.consts[fn.Name] = true
	}
	for i, param := range fn.Params {
		declared := fn.ParamTypes[i]
		if declared != "" {
			if err := in.checkType(declared, fmt.Sprintf("параметр %s функции %s", param, fn.Text()), args[i]); err != nil {
				return nil, in.wrap(pos, err)
			}
		}
		env.define(param, declared, args[i])
		delete(env.consts, param)
	}

	var result Value = NullValue{}
	if fn.Expr != nil {
		value, err := in.eval(fn.Expr, env)
		if err != nil {
			return nil, err
		}
		result = value
	} else {
		for _, stmt := range fn.Body.Stmts {
			err := in.execStmt(stmt, env)
			if err == nil {
				continue
			}
			var ret returnSignal
			if errors.As(err, &ret) {
				result = ret.value
				break
			}
			if errors.Is(err, errBreak) || errors.Is(err, errContinue) {
				return nil, in.errorf(stmt.Position(), "break и continue допустимы только внутри for")
			}
			return nil, err
		}
	}

	if fn.Returns != "" {
		if err := in.checkType(fn.Returns, "результат функции "+fn.Text(), result); err != nil {
			return nil, in.wrap(pos, err)
		}
	}
	return result, nil
}

// dispatch вызывает алиас или встроенную функцию с готовыми аргументами.
// Общая точка для обычного вызова и для конвейера.
func (in *Interpreter) dispatch(callee *Ident, args []Value, names []string, pos Position) (Value, error) {
	// Алиасы имеют приоритет: пользователь может переопределить встроенное имя
	// собственным алиасом, и это его осознанный выбор.
	decl, aliasErr := in.registry.Lookup(callee.Module, callee.Name)
	if aliasErr == nil {
		bound, err := fillArgs("алиас "+decl.Name, decl.Params, args, names)
		if err != nil {
			return nil, in.wrap(pos, err)
		}
		return in.invoke(decl, bound, pos)
	}

	// Запись создаётся вызовом по имени: Сервер("db1", порт: 5433).
	if typeDecl, err := in.registry.LookupType(callee.Module, callee.Name); err == nil {
		switch d := typeDecl.(type) {
		case *RecordDecl:
			record, err := in.construct(d, args, names)
			if err != nil {
				return nil, in.wrap(pos, err)
			}
			return record, nil
		case *InterfaceDecl:
			return nil, in.errorf(pos, "%s — интерфейс: его не создают, ему соответствуют", d.Qualified())
		}
	}

	if callee.Module != "" {
		// Встроенные функции с точкой — time.now, time.sleep — живут в
		// своём пространстве; пакет с тем же именем их перекрывает.
		if _, ok := in.builtins[callee.Qualified()]; !ok {
			return nil, in.wrap(pos, aliasErr)
		}
	}

	builtin, ok := in.builtins[callee.Qualified()]
	if !ok {
		return nil, in.errorf(pos, "неизвестная функция %q%s", callee.Qualified(), in.suggest(callee.Name))
	}
	if !unnamed(names) {
		return nil, in.errorf(pos, "%s: встроенные функции принимают аргументы только по порядку", callee.Qualified())
	}

	if err := builtin.checkArity(len(args)); err != nil {
		return nil, in.errorf(pos, "%s: %v", callee.Qualified(), err)
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
			if err := in.checkType(declared,
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
