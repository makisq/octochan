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
		{Name: "substr", MinArgs: 2, MaxArgs: 3, Returns: TypeString,
			Doc: "часть строки по символам: substr(s, начало, конец) — с начала по конец, не включая его; без конца — до конца строки; отрицательное — от конца", Fn: builtinSubstr},
		{Name: "chars", MinArgs: 1, MaxArgs: 1, Returns: TypeList,
			Doc: "символы строки списком: chars(\"ab\") — [\"a\", \"b\"]", Fn: builtinChars},
		{Name: "bytes", MinArgs: 1, MaxArgs: 1, Returns: TypeList,
			Doc: "байты строки в UTF-8 списком чисел: bytes(\"я\") — [209, 143]", Fn: builtinBytes},
		{Name: "from_bytes", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "строка из списка байтов: from_bytes([104, 105]) — \"hi\"", Fn: builtinFromBytes},
	}
}

// runeBound переводит границу substr в номер символа: отрицательная — от
// конца строки.
func runeBound(name string, v Value, size int) (int, error) {
	n, ok := numeric(v)
	if !ok {
		return 0, fmt.Errorf("substr: %s должно быть числом, получено %s", name, v.TypeName())
	}
	i := int(n)
	if i < 0 {
		i += size
	}
	if i < 0 || i > size {
		return 0, fmt.Errorf("substr: %s %d вне строки длиной %d", name, int(n), size)
	}
	return i, nil
}

func builtinSubstr(_ *Interpreter, args []Value) (Value, error) {
	text, err := textArg("substr", args[0])
	if err != nil {
		return nil, err
	}
	runes := []rune(text)
	start, err := runeBound("начало", args[1], len(runes))
	if err != nil {
		return nil, err
	}
	end := len(runes)
	if len(args) == 3 {
		if end, err = runeBound("конец", args[2], len(runes)); err != nil {
			return nil, err
		}
	}
	if start > end {
		return nil, fmt.Errorf("substr: начало %d дальше конца %d", start, end)
	}
	return StringValue(string(runes[start:end])), nil
}

func builtinChars(_ *Interpreter, args []Value) (Value, error) {
	text, err := textArg("chars", args[0])
	if err != nil {
		return nil, err
	}
	list := ListValue{}
	for _, r := range text {
		list = append(list, StringValue(string(r)))
	}
	return list, nil
}

func builtinBytes(_ *Interpreter, args []Value) (Value, error) {
	text, err := textArg("bytes", args[0])
	if err != nil {
		return nil, err
	}
	list := make(ListValue, len(text))
	for i := 0; i < len(text); i++ {
		list[i] = IntValue(text[i])
	}
	return list, nil
}

func builtinFromBytes(_ *Interpreter, args []Value) (Value, error) {
	list, ok := args[0].(ListValue)
	if !ok {
		return nil, fmt.Errorf("from_bytes: нужен список чисел, получено %s", args[0].TypeName())
	}
	data := make([]byte, len(list))
	for i, item := range list {
		n, ok := numeric(item)
		if !ok || n < 0 || n > 255 || n != float64(int(n)) {
			return nil, fmt.Errorf("from_bytes: элемент %d — не байт (0…255): %s", i, Display(item))
		}
		data[i] = byte(n)
	}
	return StringValue(string(data)), nil
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
