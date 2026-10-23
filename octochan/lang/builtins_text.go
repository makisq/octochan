package lang

import (
	"fmt"
	"strings"
)

// Строки: регистр, обрезка, разбиение, замена, поиск.
func textBuiltins() []Builtin {
	return []Builtin{
		{Name: "upper", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "строка в верхнем регистре", Fn: stringFn(strings.ToUpper)},
		{Name: "lower", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "строка в нижнем регистре", Fn: stringFn(strings.ToLower)},
		{Name: "trim", MinArgs: 1, MaxArgs: 2, Returns: TypeString,
			Doc: "убрать пробелы по краям; trim(s, \"/\") — эти символы", Fn: builtinTrim},
		{Name: "split", MinArgs: 2, MaxArgs: 2, Returns: TypeList,
			Doc: "разбить строку по разделителю: split(\"a,b\", \",\")", Fn: builtinSplit},
		{Name: "replace", MinArgs: 3, MaxArgs: 3, Returns: TypeString,
			Doc: "заменить все вхождения: replace(s, старое, новое)", Fn: builtinReplace},
		{Name: "contains", MinArgs: 2, MaxArgs: 2, Returns: TypeBool,
			Doc: "есть ли подстрока в строке или элемент в списке", Fn: builtinContains},
		{Name: "starts_with", MinArgs: 2, MaxArgs: 2, Returns: TypeBool,
			Doc: "начинается ли строка с префикса", Fn: stringTest(strings.HasPrefix)},
		{Name: "ends_with", MinArgs: 2, MaxArgs: 2, Returns: TypeBool,
			Doc: "кончается ли строка суффиксом", Fn: stringTest(strings.HasSuffix)},
	}
}

func textArg(name string, v Value) (string, error) {
	switch v.(type) {
	case StringValue, IntValue, FloatValue, BoolValue:
		return v.Text(), nil
	}
	return "", fmt.Errorf("%s: нужна строка, получено %s (%s)", name, v.TypeName(), Display(v))
}

func stringFn(fn func(string) string) func(*Interpreter, []Value) (Value, error) {
	return func(_ *Interpreter, args []Value) (Value, error) {
		text, err := textArg("строковая функция", args[0])
		if err != nil {
			return nil, err
		}
		return StringValue(fn(text)), nil
	}
}

func stringTest(fn func(string, string) bool) func(*Interpreter, []Value) (Value, error) {
	return func(_ *Interpreter, args []Value) (Value, error) {
		text, err := textArg("проверка строки", args[0])
		if err != nil {
			return nil, err
		}
		return BoolValue(fn(text, args[1].Text())), nil
	}
}

func builtinTrim(_ *Interpreter, args []Value) (Value, error) {
	text, err := textArg("trim", args[0])
	if err != nil {
		return nil, err
	}
	if len(args) == 2 {
		return StringValue(strings.Trim(text, args[1].Text())), nil
	}
	return StringValue(strings.TrimSpace(text)), nil
}

func builtinSplit(_ *Interpreter, args []Value) (Value, error) {
	text, err := textArg("split", args[0])
	if err != nil {
		return nil, err
	}
	sep := args[1].Text()
	if sep == "" {
		return nil, fmt.Errorf("split: пустой разделитель")
	}
	return stringList(strings.Split(text, sep)), nil
}

func builtinReplace(_ *Interpreter, args []Value) (Value, error) {
	text, err := textArg("replace", args[0])
	if err != nil {
		return nil, err
	}
	return StringValue(strings.ReplaceAll(text, args[1].Text(), args[2].Text())), nil
}

func builtinContains(_ *Interpreter, args []Value) (Value, error) {
	if list, ok := args[0].(ListValue); ok {
		for _, item := range list {
			if equalValues(item, args[1]) {
				return BoolValue(true), nil
			}
		}
		return BoolValue(false), nil
	}
	text, err := textArg("contains", args[0])
	if err != nil {
		return nil, err
	}
	return BoolValue(strings.Contains(text, args[1].Text())), nil
}
