package lang

// Узлы дерева намеренно простые: язык небольшой, и отдельная стадия
// типизации ему не нужна.

// Node — общий интерфейс узла с позицией в исходнике.
type Node interface {
	Position() Position
}

// ---------------------------------------------------------------------------
// Выражения
// ---------------------------------------------------------------------------

// Expr — выражение, вычисляемое в значение.
type Expr interface {
	Node
	exprNode()
}

// Literal — числовой, строковый или логический литерал, а также null.
type Literal struct {
	Pos   Position
	Value Value
}

// Ident — обращение к переменной, параметру или алиасу.
// Module непуст для квалифицированных имён вида devops.проверить.
type Ident struct {
	Pos    Position
	Module string
	Name   string
}

// Qualified возвращает имя в том виде, в каком его написал пользователь.
func (i *Ident) Qualified() string {
	if i.Module == "" {
		return i.Name
	}
	return i.Module + "." + i.Name
}

// ListLit — литерал списка: [a, b, c].
type ListLit struct {
	Pos      Position
	Elements []Expr
}

// MapEntry — пара «ключ: значение» в литерале словаря.
type MapEntry struct {
	Key   Expr
	Value Expr
}

// MapLit — литерал словаря: {"srv1": "10.0.0.1"}.
//
// Фигурные скобки заняты блоками, поэтому литерал распознаётся только там,
// где ожидается выражение. В начале инструкции { по-прежнему открывает блок.
type MapLit struct {
	Pos     Position
	Entries []MapEntry
}

// Unary — унарный оператор: not x, -x.
type Unary struct {
	Pos     Position
	Op      TokenKind
	Operand Expr
}

// Binary — бинарный оператор, включая and/or и конвейер.
type Binary struct {
	Pos   Position
	Op    TokenKind
	Left  Expr
	Right Expr
}

// Call — вызов алиаса, встроенной функции или функции-значения.
//
// Callee — имя: алиас, встроенная функция или переменная с функцией.
// Fn — вызываемое выражение, когда это не имя: сделать_сумматор(1)(2),
// функции[0](x). Заполнено ровно одно из двух.
type Call struct {
	Pos    Position
	Callee *Ident
	Fn     Expr
	Args   []Expr
}

// FuncLit — функция-значение: fn(x, y) { … } или fn(x) => выражение.
//
// Функция запоминает значения видимых переменных в момент создания
// (снимок) и не меняет их. Name — имя переменной, которой функцию
// присвоили при объявлении: под ним она видит саму себя, и так работает
// рекурсия.
type FuncLit struct {
	Pos        Position
	Name       string
	Params     []string
	ParamTypes []TypeName
	Returns    TypeName
	// Body — тело-блок; Expr — тело-выражение после =>. Заполнено одно.
	Body *Block
	Expr Expr
}

// FuncRef — ссылка на алиас или встроенную функцию как на значение:
// fn upper, fn devops.проверить. Без fn голое имя алиаса — это вызов.
type FuncRef struct {
	Pos    Position
	Target *Ident
}

// Index — обращение по индексу: список[0].
type Index struct {
	Pos    Position
	Target Expr
	Index  Expr
}

func (e *Literal) Position() Position { return e.Pos }
func (e *Ident) Position() Position   { return e.Pos }
func (e *ListLit) Position() Position { return e.Pos }
func (e *MapLit) Position() Position  { return e.Pos }
func (e *Unary) Position() Position   { return e.Pos }
func (e *Binary) Position() Position  { return e.Pos }
func (e *Call) Position() Position    { return e.Pos }
func (e *Index) Position() Position   { return e.Pos }
func (e *FuncLit) Position() Position { return e.Pos }
func (e *FuncRef) Position() Position { return e.Pos }

func (*Literal) exprNode() {}
func (*Ident) exprNode()   {}
func (*ListLit) exprNode() {}
func (*MapLit) exprNode()  {}
func (*Unary) exprNode()   {}
func (*Binary) exprNode()  {}
func (*Call) exprNode()    {}
func (*Index) exprNode()   {}
func (*FuncLit) exprNode() {}
func (*FuncRef) exprNode() {}

// ---------------------------------------------------------------------------
// Инструкции
// ---------------------------------------------------------------------------

// Stmt — инструкция.
type Stmt interface {
	Node
	stmtNode()
}

// ExprStmt — выражение, выполняемое ради побочного эффекта.
type ExprStmt struct {
	Pos  Position
	Expr Expr
}

// Assign — объявление переменной или присваивание ей.
//
// Объявление (Declare) — `var имя: тип = значение`, `let имя = значение`,
// `var имя = значение`,
// `var имя: тип` (нулевое значение типа) и краткое `имя: тип = значение`.
// Declared — написанный тип; Value пусто только у `var имя: тип`.
//
// `имя = значение` присваивает объявленной переменной, а необъявленную
// объявляет с типом значения.
type Assign struct {
	Pos      Position
	Name     string
	Declare  bool
	Declared TypeName
	Value    Expr
	// Const — `let имя = значение`: переменная без права на новое значение.
	Const bool
}

// Block — последовательность инструкций в фигурных скобках.
type Block struct {
	Pos   Position
	Stmts []Stmt
}

// If — условие с необязательной веткой else.
// Else хранит либо *Block, либо вложенный *If для else if.
type If struct {
	Pos  Position
	Cond Expr
	Then *Block
	Else Stmt
}

// For — обход элементов списка.
type For struct {
	Pos      Position
	Variable string
	Declared TypeName // необязательный тип элемента
	Iterable Expr
	Body     *Block
	// Parallel означает `parallel for`: итерации выполняются одновременно.
	Parallel bool
}

// Try — выполнение с перехватом ошибки.
//
// Нужен для основного сценария предметной области: раскатать изменение
// на полсотни серверов, продолжить при отказе двух и отчитаться. Без него
// первая же неудача обрывала весь сценарий.
type Try struct {
	Pos  Position
	Body *Block
	// ErrVar — имя переменной, в которую попадёт текст ошибки.
	// Пустое, если написано `catch { ... }` без имени.
	ErrVar string
	Catch  *Block
}

// Return — возврат значения из алиаса.
type Return struct {
	Pos   Position
	Value Expr // может быть nil
}

// Break прерывает цикл.
type Break struct{ Pos Position }

// Continue переходит к следующей итерации.
type Continue struct{ Pos Position }

func (s *ExprStmt) Position() Position { return s.Pos }
func (s *Assign) Position() Position   { return s.Pos }
func (s *Block) Position() Position    { return s.Pos }
func (s *If) Position() Position       { return s.Pos }
func (s *For) Position() Position      { return s.Pos }
func (s *Try) Position() Position      { return s.Pos }
func (s *Return) Position() Position   { return s.Pos }
func (s *Break) Position() Position    { return s.Pos }
func (s *Continue) Position() Position { return s.Pos }

func (*ExprStmt) stmtNode() {}
func (*Assign) stmtNode()   {}
func (*Block) stmtNode()    {}
func (*If) stmtNode()       {}
func (*For) stmtNode()      {}
func (*Try) stmtNode()      {}
func (*Return) stmtNode()   {}
func (*Break) stmtNode()    {}
func (*Continue) stmtNode() {}

// ---------------------------------------------------------------------------
// Объявления
// ---------------------------------------------------------------------------

// AliasDecl — объявление алиаса.
type AliasDecl struct {
	Pos    Position
	Name   string
	Params []string
	// ParamTypes соответствует Params по индексу; пустое значение
	// означает отсутствие аннотации.
	ParamTypes []TypeName
	Body       *Block
	// Module — пакет, из которого пришёл алиас. Пустая строка означает
	// алиасы самого пользователя.
	Module string
	// Doc — комментарий-описание, показывается в списке алиасов.
	Doc string
}

// File — разобранный исходник: объявления алиасов и инструкции верхнего уровня.
type File struct {
	Name    string
	Aliases []*AliasDecl
	Stmts   []Stmt
}
