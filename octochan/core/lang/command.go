package lang

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Команды ochan из сценария — ochan(). Язык один в файле и в оболочке, как
// у bash: всё, что пишут в оболочке, можно написать и в алиасе, — журнал,
// инцидент, сервер, не переписывая их своими алиасами.
//
//	ochan("logs rank --min warning")
//	ochan("incident", "open", "--level", "low", "--summary", summary)
//
// Права — права самой команды у текущей роли: ochan проверяет их, как в
// командной строке, и пишет решение в журнал аудита. Читающая команда
// (право read) выполняется и в плане: она ничего не меняет, а её вывод
// нужен сценарию. Меняющая — действие плана, как запуск программы: в плане
// не выполняется, отпечаток с аргументами — в заявке, арбитр видит её
// (hard forbid ochan(except = {logs_rank})).

// Commands — команды ochan, доступные сценарию; их даёт ochan, а не язык.
type Commands interface {
	// Resolve — команда по аргументам ochan(): разобранные аргументы, ключ
	// команды (incident.open) и только ли она читает.
	Resolve(args []string) (argv []string, key string, query bool, err error)
	// Run выполняет команду и возвращает её вывод.
	Run(ctx context.Context, argv []string) (string, error)
}

// NestedPlanner — команды, которые сами выполняют сценарии: apply
// плейбука, run сценария. Их план входит в план сценария, который их
// вызывает: согласуют внешний сценарий вместе со всем, что он запустит.
// nested == false — команда не вложенный сценарий. shared — общие строки
// вызова f @ (…), которые плейбук получит из ответов на другие вызовы.
type NestedPlanner interface {
	PlanNested(ctx context.Context, argv []string, shared *AtShared) (plan, sum string, nested bool, err error)
}

// AtShared — общие строки вызова внутри f @ (…): параметр → строка в
// ответе, и операнды, из ответов на которые она может прийти. Значения в
// плане нет — оно появится при выполнении; план говорит, откуда.
type AtShared struct {
	Needs map[string]string
	From  []string
}

// SetCommands даёт сценарию команды ochan. Без вызова ochan() отказывает:
// в песочнице сценарий команд не видит.
func (in *Interpreter) SetCommands(c Commands) { in.commands = c }

func commandBuiltins() []Builtin {
	return []Builtin{
		{Name: "ochan", MinArgs: 1, MaxArgs: -1, Returns: TypeString,
			Doc: "выполнить команду ochan и вернуть её вывод: ochan(\"logs rank --min warning\") или по аргументам; " +
				"права — права команды у роли; читающая выполняется и в плане, меняющая в плане — действие и пустая строка",
			Fn: builtinOchan},
	}
}

func builtinOchan(in *Interpreter, args []Value) (Value, error) {
	text := make([]string, len(args))
	for i, a := range args {
		text[i] = a.Text()
	}
	if in.commands == nil || in.sandbox != nil {
		return nil, fmt.Errorf("ochan %s: команды ochan здесь недоступны — в песочнице сценарий их не видит", strings.Join(text, " "))
	}
	argv, key, query, err := in.commands.Resolve(text)
	if err != nil {
		return nil, err
	}
	if !query {
		effect := Effect{Kind: EffectCommand, Path: key, Args: in.templateArgs(argv), Calls: in.activeCalls()}
		// Вложенный сценарий: его план — часть плана. Строится и перед
		// выполнением — по нему видно, что выполнилось то, что согласовали.
		if planner, ok := in.commands.(NestedPlanner); ok {
			plan, sum, nested, err := planner.PlanNested(in.ctx, in.templateArgs(argv), in.atShared)
			if err != nil {
				return nil, fmt.Errorf("ochan %s: вложенный сценарий: %w", strings.Join(argv, " "), err)
			}
			if nested {
				effect.Nested, effect.NestedSum = plan, sum
			}
		}
		in.effects = append(in.effects, effect)
		if in.dry != nil {
			return StringValue(""), nil
		}
	}
	out, err := in.commands.Run(in.ctx, argv)
	if errors.Is(in.ctx.Err(), context.Canceled) || errors.Is(in.ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("ochan %s: прервано: %v", strings.Join(argv, " "), in.ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("ochan %s: %w", strings.Join(argv, " "), err)
	}
	return StringValue(out), nil
}

// Session — переменные оболочки: строка за строкой, как в одном сценарии.
// Интерпретатор у каждой строки свой (её можно прервать), переменные —
// общие.
type Session struct{ env *scope }

// NewSession — пустой сеанс оболочки.
func NewSession() *Session { return &Session{env: newScope(nil)} }

// Defined сообщает, что в сеансе есть такая переменная.
func (s *Session) Defined(name string) bool {
	_, ok := s.env.lookup(name)
	return ok
}

// RunLine выполняет строку оболочки в сеансе. Значение одиночного
// выражения возвращается — оболочка его печатает, как eval; иначе null.
func (in *Interpreter) RunLine(s *Session, file *File) (Value, error) {
	if len(file.Stmts) == 1 {
		if stmt, ok := file.Stmts[0].(*ExprStmt); ok {
			return in.eval(stmt.Expr, s.env)
		}
	}
	for _, stmt := range file.Stmts {
		if err := in.execStmt(stmt, s.env); err != nil {
			var ret returnSignal
			if errors.As(err, &ret) {
				return ret.value, nil
			}
			return nil, err
		}
	}
	return NullValue{}, nil
}

// templateArgs — аргументы с плейбуком вместо его копии с общими строками
// (c.at.yaml → c.yaml): план строится до того, как строки известны, и при
// выполнении действие должно совпасть с планом.
func (in *Interpreter) templateArgs(argv []string) []string {
	if len(in.atFilled) == 0 {
		return argv
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		if template, ok := in.atFilled[a]; ok {
			a = template
		}
		out[i] = a
	}
	return out
}
