package lang

import (
	"fmt"
	"sort"
	"strings"
)

// Problem — ошибка, найденная до выполнения.
type Problem struct {
	Pos Position
	Msg string
}

func (p Problem) String() string { return fmt.Sprintf("%s: %s", p.Pos, p.Msg) }

// typeNull — тип литерала null. Объявленным типом он быть не может, но
// проверке нужно отличать «null» от «тип неизвестен».
const typeNull TypeName = "null"

// Check проверяет файл, не выполняя его.
//
// Интерпретатор находит ошибку только тогда, когда до неё дошло выполнение:
// опечатка в ветке, которая срабатывает раз в месяц, всплывёт в бою. Check
// обходит все ветки и все алиасы файла и находит то, что видно без запуска:
// неизвестные имена и функции, число аргументов, break и return не на своём
// месте, явно неверные типы.
//
// Это и есть детерминированная проверка между тем, кто написал сценарий
// (человек или модель), и исполнением: она ничего не читает с диска и
// ничего не запускает. Алиасы файла должны быть уже добавлены в реестр —
// так же, как перед выполнением.
func Check(file *File, registry *Registry) []Problem {
	c := newChecker(registry)
	c.types(file)
	for _, decl := range file.Aliases {
		c.alias(decl)
	}
	c.stmts(file.Stmts, newCheckScope(nil), flow{topLevel: true})
	return c.sorted()
}

// types проверяет объявления записей и интерфейсов файла: типы полей
// известны, значения по умолчанию им подходят.
func (c *checker) types(file *File) {
	for _, decl := range file.Records {
		for _, field := range decl.Fields {
			c.typeName(field.Type, field.Pos)
			if field.Default == nil {
				continue
			}
			if actual := c.expr(field.Default, newCheckScope(nil)); !c.compatible(field.Type, actual) {
				c.report(field.Default.Position(), "поле %s записи %s: объявлен тип %s, значение по умолчанию типа %s",
					field.Name, decl.Name, field.Type.Russian(), actual.Russian())
			}
		}
	}
	for _, decl := range file.Interfaces {
		for _, field := range decl.Fields {
			c.typeName(field.Type, field.Pos)
		}
	}
}

// typeName сообщает о неизвестном своём типе в аннотации.
func (c *checker) typeName(t TypeName, pos Position) {
	if isBuiltinType(t) {
		return
	}
	decl, err := c.registry.lookupTypeName(t)
	if err != nil {
		c.report(pos, "%v", err)
		return
	}
	c.useType(decl)
}

// compatible — compatible с учётом своих типов: запись подходит только
// под себя, под интерфейс — если у неё есть его поля и методы.
func (c *checker) compatible(declared, actual TypeName) bool {
	if declared == "" || declared == TypeAny || actual == "" || actual == TypeAny {
		return true
	}
	builtin := func(t TypeName) bool { return isBuiltinType(t) || t == typeNull }
	if builtin(declared) {
		// Запись под встроенный тип не подходит.
		return builtin(actual) && compatible(declared, actual)
	}
	if actual == typeNull {
		return false
	}
	want, err := c.registry.lookupTypeName(declared)
	if err != nil {
		return true // о неизвестном типе сообщено при его объявлении
	}
	if !builtin(actual) {
		// Значение типа-интерфейса — любое подходящее: решит выполнение.
		if have, err := c.registry.lookupTypeName(actual); err == nil {
			if _, isInterface := have.(*InterfaceDecl); isInterface {
				return true
			}
		}
	}
	switch w := want.(type) {
	case *RecordDecl:
		have, err := c.registry.lookupTypeName(actual)
		return err == nil && have == want
	case *InterfaceDecl:
		if isBuiltinType(actual) {
			// Поля бывают у словаря, методы — у чего угодно: решит выполнение.
			return len(w.Fields) == 0 || actual == TypeMap
		}
		have, err := c.registry.lookupTypeName(actual)
		if err != nil {
			return true
		}
		record, ok := have.(*RecordDecl)
		return ok && c.recordConforms(record, w) == ""
	}
	return true
}

// recordConforms объясняет, чего записи не хватает до интерфейса.
func (c *checker) recordConforms(record *RecordDecl, iface *InterfaceDecl) string {
	// Метод по умолчанию принимает сам интерфейс: на втором круге считаем,
	// что запись подходит, как и при выполнении.
	if c.conforming[iface] {
		return ""
	}
	if c.conforming == nil {
		c.conforming = map[*InterfaceDecl]bool{}
	}
	c.conforming[iface] = true
	defer delete(c.conforming, iface)

	for _, want := range iface.Fields {
		have, ok := record.field(want.Name)
		if !ok {
			return "нет поля " + want.Name
		}
		if want.Type != "" && have.Type != "" && want.Type != have.Type &&
			!(isBuiltinType(want.Type) && compatible(want.Type, have.Type)) {
			return fmt.Sprintf("поле %s: нужен тип %s, объявлен %s", want.Name, want.Type.Russian(), have.Type.Russian())
		}
	}
	for _, method := range iface.Methods {
		if field, ok := record.field(method.Name); ok && callable(field.Type) {
			continue
		}
		if decl, err := c.registry.Lookup("", method.Name); err == nil && len(decl.Params) == len(method.Params)+1 &&
			c.compatible(decl.ParamTypes[0], TypeName(record.Qualified())) {
			continue
		}
		if builtin, ok := c.builtins[method.Name]; ok && builtin.checkArity(len(method.Params)+1) == nil {
			continue
		}
		return fmt.Sprintf("нет метода %s от %d %s", method.Name, len(method.Params)+1,
			plural(len(method.Params)+1, "аргумента", "аргументов", "аргументов"))
	}
	return ""
}

// recordOf — объявление записи по типу выражения.
func (c *checker) recordOf(t TypeName) *RecordDecl {
	if isBuiltinType(t) || t == TypeAny {
		return nil
	}
	decl, err := c.registry.lookupTypeName(t)
	if err != nil {
		return nil
	}
	record, _ := decl.(*RecordDecl)
	return record
}

// field проверяет x.поле по типу x и возвращает тип поля.
func (c *checker) field(target TypeName, name string, pos Position) TypeName {
	if record := c.recordOf(target); record != nil {
		field, ok := record.field(name)
		if !ok {
			c.report(pos, "у записи %s нет поля %q", record.Qualified(), name)
			return ""
		}
		return field.Type
	}
	switch target {
	case TypeConfig:
		return TypeString
	case TypeInt, TypeFloat, TypeNumber, TypeBool, TypeString, TypeList, TypeStream, TypeFunc, TypeMorph, TypeFunctor, typeNull:
		c.report(pos, "у значения типа %s нет полей: .%s читают у записи, словаря или конфига", target.Russian(), name)
	}
	return ""
}

// Uses перечисляет алиасы реестра, которые вызывает файл, — прямо или
// через другие алиасы. Собственные алиасы файла в список не входят.
//
// По этому списку заявка запоминает, от каких файлов библиотеки зависит
// сценарий: если между согласованием и выполнением кто-то поправил
// вызываемый алиас, выполнилось бы не то, на что сказали «да».
func Uses(file *File, registry *Registry) []*AliasDecl {
	c := newChecker(registry)
	c.uses = make(map[*AliasDecl]bool)

	visited := make(map[*AliasDecl]bool, len(file.Aliases))
	for _, decl := range file.Aliases {
		visited[decl] = true
		c.alias(decl)
	}
	c.stmts(file.Stmts, newCheckScope(nil), flow{topLevel: true})

	// Обход до неподвижной точки: алиас библиотеки может вызывать другие.
	for {
		var pending []*AliasDecl
		for decl := range c.uses {
			if !visited[decl] {
				pending = append(pending, decl)
			}
		}
		if len(pending) == 0 {
			break
		}
		for _, decl := range pending {
			visited[decl] = true
			c.alias(decl)
		}
	}

	own := make(map[*AliasDecl]bool, len(file.Aliases))
	for _, decl := range file.Aliases {
		own[decl] = true
	}
	var used []*AliasDecl
	for decl := range c.uses {
		if !own[decl] {
			used = append(used, decl)
		}
	}
	sort.Slice(used, func(i, j int) bool {
		if used[i].Module != used[j].Module {
			return used[i].Module < used[j].Module
		}
		return used[i].Name < used[j].Name
	})
	return used
}

// Dependency — объявление библиотеки, от которого зависит сценарий.
// Kind — alias, record или interface.
type Dependency struct {
	Module string
	Name   string
	File   string
	Kind   string
}

// Dependencies перечисляет алиасы, записи и интерфейсы библиотеки, к
// которым обращается файл, — прямо или через другие алиасы. По ним заявка
// запоминает файлы библиотеки и версии пакетов.
func Dependencies(file *File, registry *Registry) []Dependency {
	c := newChecker(registry)
	c.uses = make(map[*AliasDecl]bool)
	c.typeUses = make(map[any]bool)
	c.types(file)

	visited := make(map[*AliasDecl]bool, len(file.Aliases))
	for _, decl := range file.Aliases {
		visited[decl] = true
		c.alias(decl)
	}
	c.stmts(file.Stmts, newCheckScope(nil), flow{topLevel: true})
	for {
		var pending []*AliasDecl
		for decl := range c.uses {
			if !visited[decl] {
				pending = append(pending, decl)
			}
		}
		if len(pending) == 0 {
			break
		}
		for _, decl := range pending {
			visited[decl] = true
			c.alias(decl)
		}
	}

	own := map[any]bool{}
	for _, decl := range file.Aliases {
		own[decl] = true
	}
	for _, decl := range file.Records {
		own[decl] = true
	}
	for _, decl := range file.Interfaces {
		own[decl] = true
	}
	var deps []Dependency
	for decl := range c.uses {
		if !own[decl] {
			deps = append(deps, Dependency{decl.Module, decl.Name, decl.Pos.File, "alias"})
		}
	}
	for decl := range c.typeUses {
		if own[decl] {
			continue
		}
		switch d := decl.(type) {
		case *RecordDecl:
			deps = append(deps, Dependency{d.Module, d.Name, d.Pos.File, "record"})
		case *InterfaceDecl:
			deps = append(deps, Dependency{d.Module, d.Name, d.Pos.File, "interface"})
		}
	}
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].Module != deps[j].Module {
			return deps[i].Module < deps[j].Module
		}
		return deps[i].Name < deps[j].Name
	})
	return deps
}

// CheckRegistry проверяет все алиасы реестра — библиотеку целиком.
func CheckRegistry(registry *Registry) []Problem {
	c := newChecker(registry)
	for _, decl := range registry.Decls() {
		c.alias(decl)
	}
	return c.sorted()
}

type checker struct {
	registry *Registry
	builtins map[string]Builtin
	problems []Problem
	// uses собирает алиасы, к которым обращается код; nil — не собирать.
	uses map[*AliasDecl]bool
	// typeUses — записи и интерфейсы, к которым обращается код.
	typeUses map[any]bool
	// conforming — интерфейсы, соответствие которым проверяется сейчас.
	conforming map[*InterfaceDecl]bool
}

func newChecker(registry *Registry) *checker {
	return &checker{registry: registry, builtins: DefaultBuiltins()}
}

func (c *checker) report(pos Position, format string, args ...any) {
	c.problems = append(c.problems, Problem{Pos: pos, Msg: fmt.Sprintf(format, args...)})
}

func (c *checker) sorted() []Problem {
	sort.SliceStable(c.problems, func(i, j int) bool {
		a, b := c.problems[i].Pos, c.problems[j].Pos
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
	return c.problems
}

// checkScope повторяет области видимости интерпретатора: блок — своя
// область, присваивание без аннотации пишет туда, где переменная уже есть,
// иначе объявляет её в текущей. Поэтому переменная, заведённая внутри if,
// после него не видна — ровно как при выполнении.
//
// Тип переменной — написанный или тип первого значения, если он виден
// без выполнения; пустой — станет известен при выполнении.
type checkScope struct {
	vars     map[string]TypeName
	inferred map[string]bool
	consts   map[string]bool
	// frozen — снимок, который видит функция: присвоить нельзя.
	frozen bool
	parent *checkScope
}

func newCheckScope(parent *checkScope) *checkScope {
	return &checkScope{vars: make(map[string]TypeName), inferred: make(map[string]bool),
		consts: make(map[string]bool), parent: parent}
}

// owner — область, где объявлена переменная.
func (s *checkScope) owner(name string) *checkScope {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			return cur
		}
	}
	return nil
}

// capture — снимок видимых переменных для тела функции, как при
// выполнении.
func (s *checkScope) capture() *checkScope {
	flat := newCheckScope(nil)
	flat.frozen = true
	var chain []*checkScope
	for cur := s; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for name, t := range chain[i].vars {
			flat.vars[name] = t
			flat.inferred[name] = chain[i].inferred[name]
			flat.consts[name] = chain[i].consts[name]
		}
	}
	return flat
}

func (s *checkScope) lookup(name string) (TypeName, bool) {
	t, _, ok := s.lookupInferred(name)
	return t, ok
}

func (s *checkScope) lookupInferred(name string) (TypeName, bool, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if t, ok := cur.vars[name]; ok {
			return t, cur.inferred[name], true
		}
	}
	return "", false, false
}

func (s *checkScope) define(name string, declared TypeName) { s.vars[name] = declared }

// infer объявляет переменную с типом первого значения: null типа не
// задаёт, и такая переменная — any, как при выполнении.
func (s *checkScope) infer(name string, actual TypeName) {
	if actual == typeNull {
		actual = TypeAny
	}
	s.vars[name] = actual
	s.inferred[name] = true
}

func (s *checkScope) declaredHere(name string) bool {
	_, ok := s.vars[name]
	return ok
}

func (s *checkScope) names() []string {
	var names []string
	for cur := s; cur != nil; cur = cur.parent {
		for name := range cur.vars {
			names = append(names, name)
		}
	}
	return names
}

// flow — где находится инструкция: от этого зависит, допустимы ли
// break, continue и return.
type flow struct {
	// loop — ближайший объемлющий цикл последовательный.
	loop bool
	// parallel — инструкция внутри тела parallel for.
	parallel bool
	// topLevel — инструкции верхнего уровня файла, а не тело алиаса.
	topLevel bool
}

func (c *checker) alias(decl *AliasDecl) {
	scope := newCheckScope(nil) // алиас не видит переменные вызывающего
	for i, param := range decl.Params {
		var declared TypeName
		if i < len(decl.ParamTypes) {
			declared = decl.ParamTypes[i]
		}
		c.typeName(declared, decl.Pos)
		scope.define(param, declared)
	}
	// Тело алиаса выполняется прямо в области параметров, без своего блока.
	c.stmts(decl.Body.Stmts, scope, flow{})
}

func (c *checker) stmts(stmts []Stmt, scope *checkScope, f flow) {
	for _, stmt := range stmts {
		c.stmt(stmt, scope, f)
	}
}

func (c *checker) stmt(stmt Stmt, scope *checkScope, f flow) {
	switch s := stmt.(type) {
	case *ExprStmt:
		c.expr(s.Expr, scope)

	case *Assign:
		c.assign(s, scope)

	case *Block:
		c.stmts(s.Stmts, newCheckScope(scope), f)

	case *If:
		c.expr(s.Cond, scope)
		c.stmt(s.Then, scope, f)
		if s.Else != nil {
			c.stmt(s.Else, scope, f)
		}

	case *For:
		c.forLoop(s, scope, f)

	case *Try:
		c.stmt(s.Body, newCheckScope(scope), f)
		catchScope := newCheckScope(scope)
		if s.ErrVar != "" {
			catchScope.define(s.ErrVar, TypeString)
		}
		c.stmt(s.Catch, catchScope, f)

	case *Return:
		if f.parallel {
			c.report(s.Pos, "return недопустим внутри parallel for: итерации выполняются одновременно, и прерывать нечего")
		}
		if s.Value != nil {
			c.expr(s.Value, scope)
		}

	case *SetStmt:
		actual := c.expr(s.Value, scope)
		switch target := s.Target.(type) {
		case *Field:
			if t := c.field(c.expr(target.Target, scope), target.Name, target.Pos); !c.compatible(t, actual) {
				c.report(s.Pos, "поле %s: объявлен тип %s, присваивается значение типа %s",
					target.Name, t.Russian(), actual.Russian())
			}
		case *Index:
			switch t := c.expr(target.Target, scope); t {
			case TypeInt, TypeFloat, TypeNumber, TypeBool, TypeString, TypeStream, TypeFunc, TypeMorph, TypeFunctor, typeNull:
				c.report(s.Pos, "присвоить по индексу можно в список, словарь или конфиг, а не в %s", t.Russian())
			}
			c.expr(target.Index, scope)
		}

	case *Break:
		c.loopControl("break", s.Pos, f)

	case *Continue:
		c.loopControl("continue", s.Pos, f)
	}
}

func (c *checker) loopControl(what string, pos Position, f flow) {
	switch {
	case f.loop:
	case f.parallel:
		c.report(pos, "%s недопустим внутри parallel for: итерации выполняются одновременно, и прерывать нечего", what)
	case f.topLevel:
		c.report(pos, "%s вне цикла", what)
	default:
		c.report(pos, "%s допустим только внутри for", what)
	}
}

func (c *checker) assign(s *Assign, scope *checkScope) {
	var actual TypeName
	if s.Value != nil {
		actual = c.expr(s.Value, scope)
	}

	if s.Declare {
		c.typeName(s.Declared, s.Pos)
		if scope.declaredHere(s.Name) {
			c.report(s.Pos, "переменная %s уже объявлена в этой области; чтобы изменить значение, "+
				"присвойте без var и без типа: %s = …", s.Name, s.Name)
		}
		if s.Const {
			defer func() { scope.consts[s.Name] = true }()
		}
		switch {
		case s.Declared == "":
			scope.infer(s.Name, actual)
			return
		case s.Value == nil:
			if _, err := zeroValue(s.Declared); err != nil {
				c.report(s.Pos, "переменная %s: %v", s.Name, err)
			}
		case !c.compatible(s.Declared, actual):
			c.report(s.Pos, "переменная %s: объявлен тип %s, присваивается значение типа %s",
				s.Name, s.Declared.Russian(), actual.Russian())
		}
		scope.define(s.Name, s.Declared)
		return
	}

	declared, inferred, exists := scope.lookupInferred(s.Name)
	if !exists {
		scope.infer(s.Name, actual)
		return
	}
	if owner := scope.owner(s.Name); owner.consts[s.Name] {
		c.report(s.Pos, "%s объявлена через let, и её значение не меняется; "+
			"для изменяемой переменной объявите var %s", s.Name, s.Name)
		return
	} else if owner.frozen {
		c.report(s.Pos, "%s — переменная снаружи функции: функция видит её значение "+
			"на момент создания и не меняет его; объявите свою: var %s = …", s.Name, s.Name)
		return
	}
	if !c.compatible(declared, actual) {
		c.report(s.Pos, "переменная %s: %s, присваивается значение типа %s%s",
			s.Name, typeOrigin(declared, inferred), actual.Russian(), reassignHint(s.Name, declared, actual))
	}
}

func (c *checker) forLoop(s *For, scope *checkScope, f flow) {
	iterable := c.expr(s.Iterable, scope)
	switch iterable {
	case TypeInt, TypeFloat, TypeNumber, TypeBool, TypeStream, TypeFunc, TypeMorph, TypeFunctor:
		c.report(s.Pos, "по значению типа %s нельзя пройти циклом", iterable.Russian())
	}

	// Тип переменной цикла проверяется по литералу списка: остальное
	// станет известно только при выполнении.
	c.typeName(s.Declared, s.Pos)
	if list, ok := s.Iterable.(*ListLit); ok && s.Declared != "" {
		for _, element := range list.Elements {
			if actual := c.infer(element, scope); !c.compatible(s.Declared, actual) {
				c.report(element.Position(), "элемент цикла %s: объявлен тип %s, в списке значение типа %s",
					s.Variable, s.Declared.Russian(), actual.Russian())
			}
		}
	}

	body := newCheckScope(scope)
	body.define(s.Variable, s.Declared)

	inner := flow{loop: true, parallel: f.parallel, topLevel: f.topLevel}
	if s.Parallel {
		inner.loop = false
		inner.parallel = true
	}
	c.stmts(s.Body.Stmts, body, inner)
}

// expr проверяет выражение и возвращает его тип, если он известен без
// выполнения, иначе пустую строку.
func (c *checker) expr(e Expr, scope *checkScope) TypeName {
	switch x := e.(type) {
	case *Literal:
		return literalType(x.Value)

	case *Ident:
		return c.ident(x, scope)

	case *ListLit:
		for _, element := range x.Elements {
			c.expr(element, scope)
		}
		return TypeList

	case *MapLit:
		for _, entry := range x.Entries {
			c.expr(entry.Key, scope)
			c.expr(entry.Value, scope)
		}
		return TypeMap

	case *Unary:
		operand := c.expr(x.Operand, scope)
		if x.Op == TokenNot {
			return TypeBool
		}
		switch operand {
		case TypeInt, TypeFloat:
			return operand
		case TypeList, TypeMap, TypeConfig, TypeStream, TypeFunc, TypeMorph, TypeFunctor, typeNull:
			c.report(x.Pos, "унарный минус применим только к числу, получено %s", operand.Russian())
		}
		return ""

	case *Binary:
		return c.binary(x, scope)

	case *Call:
		return c.callExpr(x, scope, nil)

	case *Field:
		return c.field(c.expr(x.Target, scope), x.Name, x.Pos)

	case *FuncLit:
		c.funcLit(x, scope)
		return TypeFunc

	case *FuncRef:
		if t := c.funcRef(x.Target, scope); t != "" && t != TypeAny {
			return t
		}
		return TypeFunc

	case *Index:
		target := c.expr(x.Target, scope)
		c.expr(x.Index, scope)
		switch target {
		case TypeInt, TypeFloat, TypeNumber, TypeBool, TypeString, TypeStream, TypeFunc, TypeMorph, TypeFunctor, typeNull:
			c.report(x.Pos, "индексировать можно список, словарь или конфиг, получено %s", target.Russian())
		case TypeConfig:
			return TypeString
		}
		return ""
	}
	return ""
}

// infer возвращает тип выражения, не сообщая о его ошибках: нужен там,
// где выражение уже проверено или будет проверено отдельно.
func (c *checker) infer(e Expr, scope *checkScope) TypeName {
	saved := len(c.problems)
	t := c.expr(e, scope)
	c.problems = c.problems[:saved]
	return t
}

func (c *checker) binary(x *Binary, scope *checkScope) TypeName {
	switch x.Op {
	case TokenPipe:
		return c.pipe(x, scope)
	case TokenCompose:
		return composedType(x.Pos, c.funcOperand(x.Left, scope), c.funcOperand(x.Right, scope), c)
	case TokenArrow:
		for _, operand := range []Expr{x.Left, x.Right} {
			switch t := c.expr(operand, scope); t {
			case TypeInt, TypeFloat, TypeNumber, TypeBool, TypeList, TypeStream, TypeFunc, TypeMorph, TypeFunctor, typeNull:
				c.report(operand.Position(), "-> соединяет конфиги, получено %s", t.Russian())
			}
		}
		return TypeMorph
	}

	left := c.expr(x.Left, scope)
	right := c.expr(x.Right, scope)

	switch x.Op {
	case TokenAnd, TokenOr, TokenEQ, TokenNE, TokenLT, TokenLE, TokenGT, TokenGE:
		return TypeBool

	case TokenPlus:
		if left == TypeList {
			if right != "" && right != TypeList && right != TypeAny {
				c.report(x.Pos, "к списку можно прибавить только список, получено %s", right.Russian())
			}
			return TypeList
		}
		return numericResult(left, right)
	}

	// Остальная арифметика требует чисел. Строка может оказаться числом
	// ("5" - 1 даёт 4), поэтому отвергаются только заведомо нечисловые типы.
	for _, operand := range []TypeName{left, right} {
		switch operand {
		case TypeList, TypeMap, TypeConfig, TypeStream, TypeFunc, TypeMorph, TypeFunctor, typeNull:
			c.report(x.Pos, "оператор %s неприменим к %s", x.Op, operand.Russian())
			return ""
		}
	}

	switch x.Op {
	case TokenSlash:
		if isZeroLiteral(x.Right) {
			c.report(x.Pos, "деление на ноль")
		}
		return TypeFloat
	case TokenPercent:
		if isZeroLiteral(x.Right) {
			c.report(x.Pos, "деление на ноль")
		}
		return TypeInt
	}
	return numericResult(left, right)
}

// numericResult повторяет правило интерпретатора: два целых дают целое,
// целое с дробным — дробное. Строки и неизвестные типы оставляют результат
// неизвестным: строка бывает и числом, и текстом.
func numericResult(left, right TypeName) TypeName {
	isNumber := func(t TypeName) bool { return t == TypeInt || t == TypeFloat }
	switch {
	case left == TypeInt && right == TypeInt:
		return TypeInt
	case isNumber(left) && isNumber(right):
		return TypeFloat
	}
	return ""
}

func isZeroLiteral(e Expr) bool {
	lit, ok := e.(*Literal)
	if !ok {
		return false
	}
	switch v := lit.Value.(type) {
	case IntValue:
		return v == 0
	case FloatValue:
		return v == 0
	}
	return false
}

// pipe проверяет a | f(b): левое значение становится первым аргументом.
func (c *checker) pipe(x *Binary, scope *checkScope) TypeName {
	left := c.expr(x.Left, scope)

	switch right := x.Right.(type) {
	case *Call:
		return c.callExpr(right, scope, &left)
	case *Ident:
		return c.call(right, nil, nil, right.Pos, []TypeName{left}, scope)
	}

	// Справа — выражение, дающее функцию: x | f >> g.
	if t := c.expr(x.Right, scope); !callable(t) {
		c.report(x.Pos, "справа от | должен быть вызов алиаса, встроенной функции или функция, получено %s", t.Russian())
	}
	return ""
}

// callable — может ли значение такого типа оказаться функцией, морфизмом
// или функтором.
func callable(t TypeName) bool {
	switch t {
	case "", TypeAny, TypeFunc, TypeMorph, TypeFunctor:
		return true
	}
	return false
}

// composedType — тип f >> g: функтор с функтором, морфизм с морфизмом или
// функцией, функция с функцией.
func composedType(pos Position, left, right TypeName, c *checker) TypeName {
	switch {
	case left == TypeFunctor && right == TypeFunctor:
		return TypeFunctor
	case left == TypeFunctor && right != "" && right != TypeAny,
		right == TypeFunctor && left != "" && left != TypeAny:
		c.report(pos, "функтор композируется только с функтором, получено %s и %s", left.Russian(), right.Russian())
		return ""
	case left == TypeMorph || right == TypeMorph:
		return TypeMorph
	case left == TypeFunc && right == TypeFunc:
		return TypeFunc
	}
	return ""
}

// funcLit проверяет тело функции в снимке видимых переменных: как при
// выполнении, присвоить им нельзя, а под своим именем функция видит себя.
func (c *checker) funcLit(x *FuncLit, scope *checkScope) {
	inner := newCheckScope(scope.capture())
	if x.Name != "" {
		inner.define(x.Name, TypeFunc)
		inner.consts[x.Name] = true
	}
	for i, param := range x.Params {
		c.typeName(x.ParamTypes[i], x.Pos)
		inner.define(param, x.ParamTypes[i])
		delete(inner.consts, param)
	}
	c.typeName(x.Returns, x.Pos)
	if x.Expr != nil {
		if actual := c.expr(x.Expr, inner); x.Returns != "" && !c.compatible(x.Returns, actual) {
			c.report(x.Expr.Position(), "результат функции: объявлен тип %s, выражение типа %s",
				x.Returns.Russian(), actual.Russian())
		}
		return
	}
	c.stmts(x.Body.Stmts, inner, flow{})
}

// funcRef проверяет ссылку fn имя: имя должно быть тем, что вызывают.
// Возвращает тип значения.
func (c *checker) funcRef(target *Ident, scope *checkScope) TypeName {
	if target.Module == "" {
		if t, ok := scope.lookup(target.Name); ok {
			if !callable(t) {
				c.report(target.Pos, "%s — не функция, а %s", target.Name, t.Russian())
			}
			return t
		}
	}
	decl, err := c.registry.Lookup(target.Module, target.Name)
	if err == nil {
		c.use(decl)
		return TypeFunc
	}
	if _, ok := c.builtins[target.Qualified()]; ok {
		return TypeFunc
	}
	if target.Module != "" || c.registry.declared(target.Name) {
		c.report(target.Pos, "%v", err)
		return ""
	}
	c.report(target.Pos, "неизвестная функция %q%s", target.Qualified(), c.suggest(target.Name, scope))
	return ""
}

// funcOperand проверяет операнд композиции и возвращает его тип: голое
// имя здесь — функция, а не вызов.
func (c *checker) funcOperand(e Expr, scope *checkScope) TypeName {
	if id, ok := e.(*Ident); ok {
		return c.funcRef(id, scope)
	}
	t := c.expr(e, scope)
	if !callable(t) {
		c.report(e.Position(), "оператор >> соединяет функции, морфизмы или функторы, получено %s", t.Russian())
	}
	return t
}

func (c *checker) ident(x *Ident, scope *checkScope) TypeName {
	if x.Module == "" {
		if declared, ok := scope.lookup(x.Name); ok {
			return declared
		}
	} else if declared, ok := scope.lookup(x.Module); ok {
		// x.поле, где x — переменная.
		return c.field(declared, x.Name, x.Pos)
	}

	// Голое имя алиаса без скобок — вызов без аргументов.
	decl, err := c.registry.Lookup(x.Module, x.Name)
	if err == nil {
		c.use(decl)
		c.arity(decl, 0, x.Pos)
		return ""
	}

	switch {
	case x.Module != "" || c.registry.declared(x.Name):
		// Неизвестный пакет, имя файла без кавычек или имя, которое есть
		// сразу в нескольких пакетах: объяснение даёт сам реестр.
		c.report(x.Pos, "%v", err)
	case c.isBuiltin(x.Name):
		c.report(x.Pos, "%s — встроенная функция, вызывайте её со скобками: %s()", x.Name, x.Name)
	default:
		c.report(x.Pos, "неизвестное имя %q%s", x.Name, c.suggest(x.Name, scope))
	}
	return ""
}

// callExpr проверяет выражение вызова: функцию-выражение, x.метод(…) и
// вызов по имени. piped — тип значения слева от конвейера.
func (c *checker) callExpr(x *Call, scope *checkScope, piped *TypeName) TypeName {
	if x.Fn != nil {
		if t := c.expr(x.Fn, scope); !callable(t) {
			c.report(x.Pos, "вызвать можно функцию, морфизм или функтор, получено %s", t.Russian())
		}
		for _, arg := range x.Args {
			c.expr(arg, scope)
		}
		return ""
	}

	callee := x.Callee
	var receiver TypeName
	hasReceiver := false
	switch {
	case x.Receiver != nil:
		receiver, hasReceiver = c.expr(x.Receiver, scope), true
	case callee.Module != "":
		if t, ok := scope.lookup(callee.Module); ok {
			receiver, hasReceiver = t, true
			callee = &Ident{Pos: callee.Pos, Name: callee.Name}
		}
	}
	var pre []TypeName
	if hasReceiver {
		pre = append(pre, receiver)
	}
	if piped != nil {
		pre = append(pre, *piped)
	}
	if !hasReceiver {
		return c.call(callee, x.Args, x.Names, x.Pos, pre, scope)
	}

	// Поле-функция записи вызывается без самого значения.
	if record := c.recordOf(receiver); record != nil {
		if field, ok := record.field(callee.Name); ok && callable(field.Type) {
			for _, arg := range x.Args {
				c.expr(arg, scope)
			}
			return ""
		}
	}
	// У словаря и неизвестного значения поле-функция может появиться при
	// выполнении: если метода с таким именем нет, решит выполнение.
	if receiver == TypeMap || receiver == "" || receiver == TypeAny {
		if _, err := c.registry.Lookup("", callee.Name); err != nil && !c.isBuiltin(callee.Name) {
			if _, ok := scope.lookup(callee.Name); !ok {
				for _, arg := range x.Args {
					c.expr(arg, scope)
				}
				return ""
			}
		}
	}

	// x.f(a) — это f(x, a); x | r.f(a) — это f(r, x, a).
	return c.call(callee, x.Args, x.Names, x.Pos, pre, scope)
}

// call проверяет вызов. pre — типы значений, которые идут первыми
// аргументами: слева от конвейера и слева от точки.
func (c *checker) call(callee *Ident, args []Expr, argNames []string, pos Position, pre []TypeName, scope *checkScope) TypeName {
	types := append([]TypeName(nil), pre...)
	for _, arg := range args {
		types = append(types, c.expr(arg, scope))
	}
	named := !unnamed(argNames)
	var names []string
	if named {
		names = append(make([]string, len(pre)), argNames...)
	}

	// Переменная с функцией перекрывает алиас и встроенную функцию; с
	// другим значением — нет, как при выполнении.
	if callee.Module == "" {
		if t, ok := scope.lookup(callee.Name); ok {
			if t == TypeMorph {
				return TypeConfig
			}
			if callable(t) {
				// Функция или то, что станет известно при выполнении.
				return ""
			}
			if _, err := c.registry.Lookup("", callee.Name); err != nil && !c.isBuiltin(callee.Name) {
				c.report(pos, "%s — не функция, а %s", callee.Name, t.Russian())
				return ""
			}
		}
	}

	// Порядок разрешения тот же, что у интерпретатора: алиас перекрывает
	// встроенную функцию с тем же именем.
	decl, err := c.registry.Lookup(callee.Module, callee.Name)
	if err == nil {
		c.use(decl)
		if named {
			c.namedArgs("алиас "+decl.Name, "параметра", decl.Params, len(types), names, nil, pos)
			return ""
		}
		if c.arity(decl, len(types), pos) {
			c.paramTypes(decl, types, args, len(pre), pos)
		}
		return ""
	}

	if typeDecl, typeErr := c.registry.LookupType(callee.Module, callee.Name); typeErr == nil {
		c.useType(typeDecl)
		switch d := typeDecl.(type) {
		case *RecordDecl:
			c.construct(d, types, names, pos)
			return TypeName(d.Qualified())
		case *InterfaceDecl:
			c.report(pos, "%s — интерфейс: его не создают, ему соответствуют", d.Qualified())
			return ""
		}
	}

	if builtin, ok := c.builtins[callee.Qualified()]; ok {
		if named {
			c.report(pos, "%s: встроенные функции принимают аргументы только по порядку", callee.Qualified())
		} else if err := builtin.checkArity(len(types)); err != nil {
			c.report(pos, "%s: %v", callee.Qualified(), err)
		}
		return builtin.Returns
	}

	if callee.Module != "" {
		c.report(pos, "%v", err)
		return ""
	}

	if c.registry.declared(callee.Name) {
		c.report(pos, "%v", err)
		return ""
	}
	c.report(pos, "неизвестная функция %q%s", callee.Qualified(), c.suggest(callee.Name, nil))
	return ""
}

func (c *checker) use(decl *AliasDecl) {
	if c.uses != nil {
		c.uses[decl] = true
	}
}

func (c *checker) useType(decl any) {
	if c.typeUses != nil {
		c.typeUses[decl] = true
	}
}

// construct проверяет создание записи: число и имена аргументов, все
// поля без значений по умолчанию заданы, типы подходят.
func (c *checker) construct(decl *RecordDecl, types []TypeName, names []string, pos Position) {
	params := make([]string, len(decl.Fields))
	required := make([]bool, len(decl.Fields))
	for i, f := range decl.Fields {
		params[i] = f.Name
		required[i] = f.Default == nil
	}
	slots := c.namedArgs("запись "+decl.Qualified(), "поля", params, len(types), names, required, pos)
	for i, slot := range slots {
		if slot < 0 {
			continue
		}
		if field, t := decl.Fields[i], types[slot]; !c.compatible(field.Type, t) {
			c.report(pos, "поле %s записи %s: объявлен тип %s, передано значение типа %s",
				field.Name, decl.Qualified(), field.Type.Russian(), t.Russian())
		}
	}
}

// namedArgs проверяет раскладку n аргументов по параметрам. required —
// какие параметры обязательны (nil — все). Возвращает для каждого
// параметра номер аргумента или -1.
func (c *checker) namedArgs(what, kind string, params []string, n int, names []string, required []bool, pos Position) []int {
	slots := make([]int, len(params))
	for i := range slots {
		slots[i] = -1
	}
	bound := make([]bool, len(params))
	for i := 0; i < n; i++ {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		if name == "" {
			if i >= len(params) {
				c.report(pos, "%s принимает %d %s, передано %d", what, len(params),
					plural(len(params), "аргумент", "аргумента", "аргументов"), n)
				return nil
			}
			bound[i] = true
			slots[i] = i
			continue
		}
		index := -1
		for j, param := range params {
			if param == name {
				index = j
			}
		}
		if index < 0 {
			c.report(pos, "%s: нет %s %q; есть: %s", what, kind, name, strings.Join(params, ", "))
			continue
		}
		if bound[index] {
			c.report(pos, "%s: %s передан дважды — по порядку и по имени", what, name)
		}
		bound[index] = true
		slots[index] = i
	}
	for i, param := range params {
		if !bound[i] && (required == nil || required[i]) {
			if required == nil {
				c.report(pos, "%s: не передан параметр %s", what, param)
			} else {
				c.report(pos, "%s: не задано поле %s", what, param)
			}
		}
	}
	return slots
}

// arity сообщает о неверном числе аргументов алиаса.
func (c *checker) arity(decl *AliasDecl, n int, pos Position) bool {
	if n == len(decl.Params) {
		return true
	}
	c.report(pos, "алиас %s принимает %d %s, передано %d",
		decl.Name, len(decl.Params), plural(len(decl.Params), "аргумент", "аргумента", "аргументов"), n)
	return false
}

func (c *checker) paramTypes(decl *AliasDecl, types []TypeName, args []Expr, pre int, pos Position) {
	for i, actual := range types {
		if i >= len(decl.ParamTypes) {
			break
		}
		declared := decl.ParamTypes[i]
		if c.compatible(declared, actual) {
			continue
		}

		// Позиция аргумента точнее позиции вызова; у значения из конвейера
		// своей позиции в этом вызове нет.
		at := pos
		argIndex := i - pre
		if argIndex >= 0 && argIndex < len(args) {
			at = args[argIndex].Position()
		}
		c.report(at, "параметр %s алиаса %s: объявлен тип %s, передано значение типа %s",
			decl.Params[i], decl.Name, declared.Russian(), actual.Russian())
	}
}

func (c *checker) isBuiltin(name string) bool {
	_, ok := c.builtins[name]
	return ok
}

func (c *checker) suggest(name string, scope *checkScope) string {
	candidates := c.registry.Names()
	for builtin := range c.builtins {
		candidates = append(candidates, builtin)
	}
	if scope != nil {
		candidates = append(candidates, scope.names()...)
	}
	return suggestName(name, candidates)
}

// literalType — тип значения литерала.
func literalType(v Value) TypeName {
	switch v.(type) {
	case IntValue:
		return TypeInt
	case FloatValue:
		return TypeFloat
	case StringValue:
		return TypeString
	case BoolValue:
		return TypeBool
	case NullValue:
		return typeNull
	}
	return ""
}

// compatible сообщает, может ли значение типа actual попасть туда, где
// объявлен тип declared. Неизвестный тип совместим со всем: проверка
// отвергает только то, что неверно наверняка, — остальное проверит
// интерпретатор.
func compatible(declared, actual TypeName) bool {
	if declared == "" || declared == TypeAny || actual == "" || actual == TypeAny {
		return true
	}
	switch declared {
	case TypeFloat, TypeNumber:
		return actual == TypeInt || actual == TypeFloat || actual == TypeNumber
	case TypeInt:
		// Объявленное «число» может оказаться и целым, и дробным.
		return actual == TypeInt || actual == TypeNumber
	}
	return declared == actual
}
