// Package fp — формальный промпт: язык, на котором оператор описывает
// задачу модели, и детерминированные проверки, которые выполняются до неё.
//
// Это первый уровень формального промптирования (раздел 7.1 манифеста):
// модель не переобучается, формальность обеспечивают разбор, арбитраж и
// отчёт вокруг неё. Пакет не знает ни про модели, ни про octochan: он
// получает текст и отвечает, принадлежит ли тот языку, однозначен ли,
// хватает ли знания и совместны ли жёсткие правила. Всё — «на if и else».
package fp

import "fmt"

// Pos — место в тексте промпта.
type Pos struct {
	Line int
	Col  int
}

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// Program — разобранный формальный промпт.
type Program struct {
	Name string

	Goal            *Goal
	Classes         []*Class
	Relations       []*Relation
	Priorities      []*Priority
	Generalizations []*Generalization
	Instances       []*Instance
	Requires        []Name
	Imports         []*Import
	Rules           []*Rule
	Cells           []*Cell
	Report          []*ReportEntry
}

// Name — имя с местом, где оно написано.
type Name struct {
	Pos  Pos
	Text string
}

// Goal — заголовок `goal: слово`. Цель γ — класс, к которому относится слово.
type Goal struct {
	Pos  Pos
	Word string
}

// Class — объявление класса обобщения и слов, которыми он выражается.
// Одно слово в нескольких классах законно (П4): это одноимённость, и её
// разрешает контекст.
type Class struct {
	Pos   Pos
	Name  string
	Words []Name
}

// Relation — типизированное ребро графа программы: From --Type--> To.
// Label — имя отношения, по которому на него ссылаются.
type Relation struct {
	Pos   Pos
	From  string
	Type  string
	To    string
	Label string
}

// Priority — `A < B`: при конфликте отношений A побеждает B.
type Priority struct {
	Pos    Pos
	Higher string
	Lower  string
}

// Generalization — схема со слотами g(x₁:τ₁, …): Result = Op(Args).
type Generalization struct {
	Pos    Pos
	Name   string
	Params []*Param
	Result string
	Op     Name
	Args   []Name
}

// Param — слот обобщения: тип — класс или перечисленный домен значений.
type Param struct {
	Pos    Pos
	Name   string
	Class  string
	Domain []string
}

// Instance — частный случай: подстановка значений в слоты обобщения.
type Instance struct {
	Pos      Pos
	Name     string
	Of       Name
	Bindings []*Binding
}

// Binding — `слот := значение`.
type Binding struct {
	Pos   Pos
	Slot  string
	Value string
}

// Import — подключение скилла: `import @имя v1.2`.
type Import struct {
	Pos           Pos
	Skill         string
	Version       string
	Provides      []Name
	RequiresClass []Name
}

// RuleKind — вид жёсткого правила.
type RuleKind string

const (
	// Forbid — «никогда X, кроме перечисленного» (??n_, hard forbid).
	Forbid RuleKind = "forbid"
	// Require — «всегда X на объявленной области» (??a_, hard require).
	Require RuleKind = "require"
)

// Rule — жёсткое правило.
type Rule struct {
	Pos    Pos
	Kind   RuleKind
	Action string
	Args   []*RuleArg
}

// RuleArg — параметр правила: except или scope со значением-именем
// или множеством имён.
type RuleArg struct {
	Pos    Pos
	Name   string
	Values []string
}

// Cell — ячейка среды-ноутбука: декларативная правка.
type Cell struct {
	Pos   Pos
	Name  string
	Sets  []*Set
	Binds *CodeRange
}

// Set — `set Цель.атрибут = значение`: значение задаётся, а не приращивается.
type Set struct {
	Pos    Pos
	Target string
	Value  Value
}

// Value — значение: имя, ссылка через точку или число с единицей измерения.
type Value struct {
	Pos  Pos
	Text string
	// Ref — значение ссылается на элемент программы (содержит точку).
	Ref bool
}

// CodeRange — привязка ячейки к коду τ: файл и диапазон строк.
type CodeRange struct {
	Pos  Pos
	Path string
	From int
	To   int
}

// ReportEntry — запись отчёта ⟨q, q(I*), q(Impl(K)), ρ_q⟩.
type ReportEntry struct {
	Pos          Pos
	Question     string
	Choice       *Value
	Alternatives []string
	Basis        *Basis
}

// Basis — основание решения.
type Basis struct {
	Pos  Pos
	Kind string // not_in_prompt, priority, relation, skill, rule
	Args []string
}

func (b *Basis) String() string {
	switch b.Kind {
	case "not_in_prompt":
		return b.Kind
	case "priority":
		return fmt.Sprintf("priority(%s < %s)", b.Args[0], b.Args[1])
	case "skill":
		return fmt.Sprintf("skill(@%s)", b.Args[0])
	default:
		return fmt.Sprintf("%s(%s)", b.Kind, b.Args[0])
	}
}
