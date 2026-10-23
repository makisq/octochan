package lang

import (
	"errors"
	"fmt"
	"sort"
)

// Функции высшего порядка: принимают функцию-значение и вызывают её.
//
// Функцию передают как fn(x) => …, fn имя_алиаса или переменной, в
// которой лежит функция. Вызов идёт тем же путём, что и обычный: проверка
// типов параметров, песочница, режим плана и действия — те же.
func funcBuiltins() []Builtin {
	return []Builtin{
		{Name: "each", MinArgs: 2, MaxArgs: 2, Returns: TypeList,
			Doc: "применить функцию к каждому элементу и собрать результаты: each(список, fn(x) => x * 2)",
			Fn:  builtinEach},
		{Name: "filter", MinArgs: 2, MaxArgs: 2, Returns: TypeList,
			Doc: "элементы, для которых функция истинна: filter(список, fn(x) => x > 0)", Fn: builtinFilter},
		{Name: "reduce", MinArgs: 3, MaxArgs: 3,
			Doc: "свернуть список: reduce(список, fn(итог, x) => итог + x, 0)", Fn: builtinReduce},
		{Name: "sort", MinArgs: 1, MaxArgs: 2, Returns: TypeList,
			Doc: "отсортировать список: sort(список) или sort(список, fn(x) => ключ); порядок равных сохраняется",
			Fn:  builtinSort},
		{Name: "call", MinArgs: 1, MaxArgs: -1,
			Doc: "вызвать функцию с аргументами: call(f, 1, 2)", Fn: builtinCall},
		{Name: "partial", MinArgs: 2, MaxArgs: -1, Returns: TypeFunc,
			Doc: "закрепить первые аргументы: partial(fn put, c)(\"x\", \"1\") — то же, что put(c, \"x\", \"1\")",
			Fn:  builtinPartial},
		{Name: "compose", MinArgs: 1, MaxArgs: -1, Returns: TypeFunc,
			Doc: "функции по порядку применения: compose(f, g)(x) — это g(f(x)), то же, что f >> g",
			Fn:  builtinCompose},
		{Name: "id", MinArgs: 0, MaxArgs: 0, Returns: TypeFunc,
			Doc: "тождественная функция: id()(x) — это x", Fn: builtinID},
		{Name: "range", MinArgs: 1, MaxArgs: 3, Returns: TypeList,
			Doc: "список целых: range(n) — от 0 до n-1, range(a, b) — от a до b-1, range(a, b, шаг)",
			Fn:  builtinRange},
	}
}

// maxRange — предел длины списка range: сценарий не должен съесть память
// одной опечаткой в границе.
const maxRange = 1_000_000

// funcArg — аргумент, который вызывают: функция, морфизм или функтор.
func funcArg(name string, v Value) (Value, error) {
	if !isCallable(v) {
		return nil, fmt.Errorf("%s: нужна функция — fn(x) => …, fn имя или переменная с функцией; получено %s (%s)",
			name, v.TypeName(), Display(v))
	}
	return v, nil
}

// items — элементы, по которым идёт функция высшего порядка: те же, что
// обходит for.
func (in *Interpreter) items(name string, v Value) ([]Value, error) {
	items, err := in.iterate(Position{}, v)
	if err != nil {
		var runtimeErr *RuntimeError
		if errors.As(err, &runtimeErr) {
			return nil, fmt.Errorf("%s: %s", name, runtimeErr.Msg)
		}
		return nil, err
	}
	return items, nil
}

func builtinEach(in *Interpreter, args []Value) (Value, error) {
	items, err := in.items("each", args[0])
	if err != nil {
		return nil, err
	}
	fn, err := funcArg("each", args[1])
	if err != nil {
		return nil, err
	}
	out := make(ListValue, 0, len(items))
	for _, item := range items {
		value, err := in.callValue(fn, []Value{item}, Position{})
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

func builtinFilter(in *Interpreter, args []Value) (Value, error) {
	items, err := in.items("filter", args[0])
	if err != nil {
		return nil, err
	}
	fn, err := funcArg("filter", args[1])
	if err != nil {
		return nil, err
	}
	out := ListValue{}
	for _, item := range items {
		keep, err := in.callValue(fn, []Value{item}, Position{})
		if err != nil {
			return nil, err
		}
		if keep.Truthy() {
			out = append(out, item)
		}
	}
	return out, nil
}

func builtinReduce(in *Interpreter, args []Value) (Value, error) {
	items, err := in.items("reduce", args[0])
	if err != nil {
		return nil, err
	}
	fn, err := funcArg("reduce", args[1])
	if err != nil {
		return nil, err
	}
	acc := args[2]
	for _, item := range items {
		if acc, err = in.callValue(fn, []Value{acc, item}, Position{}); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

func builtinSort(in *Interpreter, args []Value) (Value, error) {
	items, err := in.items("sort", args[0])
	if err != nil {
		return nil, err
	}
	keys := items
	if len(args) == 2 {
		fn, err := funcArg("sort", args[1])
		if err != nil {
			return nil, err
		}
		keys = make([]Value, len(items))
		for i, item := range items {
			if keys[i], err = in.callValue(fn, []Value{item}, Position{}); err != nil {
				return nil, err
			}
		}
	}

	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	var cmpErr error
	sort.SliceStable(order, func(a, b int) bool {
		cmp, err := compareValues(keys[order[a]], keys[order[b]])
		if err != nil && cmpErr == nil {
			cmpErr = fmt.Errorf("sort: %w", err)
		}
		return cmp < 0
	})
	if cmpErr != nil {
		return nil, cmpErr
	}
	out := make(ListValue, len(items))
	for i, idx := range order {
		out[i] = items[idx]
	}
	return out, nil
}

func builtinCall(in *Interpreter, args []Value) (Value, error) {
	fn, err := funcArg("call", args[0])
	if err != nil {
		return nil, err
	}
	return in.callValue(fn, args[1:], Position{})
}

func builtinPartial(_ *Interpreter, args []Value) (Value, error) {
	fn, ok := args[0].(*FuncValue)
	if !ok {
		return nil, fmt.Errorf("partial: нужна функция, получено %s (%s)", args[0].TypeName(), Display(args[0]))
	}
	return &FuncValue{Base: fn, Bound: append([]Value(nil), args[1:]...)}, nil
}

func builtinCompose(_ *Interpreter, args []Value) (Value, error) {
	out, err := funcArg("compose", args[0])
	if err != nil {
		return nil, err
	}
	for _, arg := range args[1:] {
		next, err := funcArg("compose", arg)
		if err != nil {
			return nil, err
		}
		if out, err = compose(out, next); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// identity — общая тождественная функция: id() == id().
var identity = &FuncValue{Name: "id", Params: []string{"x"}, ParamTypes: []TypeName{""},
	Expr: &Ident{Name: "x"}, env: newScope(nil)}

func builtinID(_ *Interpreter, _ []Value) (Value, error) { return identity, nil }

func builtinRange(_ *Interpreter, args []Value) (Value, error) {
	bounds := make([]int64, len(args))
	for i, arg := range args {
		n, ok := arg.(IntValue)
		if !ok {
			return nil, fmt.Errorf("range: границы — целые числа, получено %s (%s)", arg.TypeName(), Display(arg))
		}
		bounds[i] = int64(n)
	}
	start, stop, step := int64(0), bounds[0], int64(1)
	if len(bounds) >= 2 {
		start, stop = bounds[0], bounds[1]
	}
	if len(bounds) == 3 {
		step = bounds[2]
	}
	if step == 0 {
		return nil, errors.New("range: шаг не может быть нулём")
	}
	var count int64
	if step > 0 && stop > start {
		count = (stop - start + step - 1) / step
	} else if step < 0 && stop < start {
		count = (start - stop - step - 1) / -step
	}
	if count > maxRange {
		return nil, fmt.Errorf("range: %d элементов — больше предела %d", count, maxRange)
	}
	out := make(ListValue, 0, count)
	for i := int64(0); i < count; i++ {
		out = append(out, IntValue(start+i*step))
	}
	return out, nil
}
