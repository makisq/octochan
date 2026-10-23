package lang

import (
	"fmt"
	"strings"
)

// Записи, интерфейсы и свои типы.
//
// Запись — именованный набор полей с типами: record Сервер { имя: string;
// порт: int = 5432 }. Создаётся вызовом по имени, поля читаются и
// пишутся через точку, тип поля проверяется при каждой записи.
//
// Интерфейс — структурный: значение подходит, если у него есть нужные
// поля и для него есть нужные методы. Метод — это алиас, встроенная
// функция или поле-функция: x.метод(a) — то же, что метод(x, a). Алиас,
// первый параметр которого объявлен интерфейсом, работает для любого
// подходящего значения — это метод по умолчанию, как у трейта.

// accepts — подходит ли значение под тип, в том числе свой.
func (in *Interpreter) accepts(t TypeName, v Value) bool {
	if isBuiltinType(t) {
		return t.Accepts(v)
	}
	decl, err := in.registry.lookupTypeName(t)
	if err != nil {
		return false
	}
	switch d := decl.(type) {
	case *RecordDecl:
		r, ok := v.(*RecordValue)
		return ok && r.Decl == d
	case *InterfaceDecl:
		return in.conforms(v, d) == nil
	}
	return false
}

// checkType проверяет значение и возвращает понятную ошибку.
func (in *Interpreter) checkType(declared TypeName, what string, v Value) error {
	if in.accepts(declared, v) {
		return nil
	}
	if !isBuiltinType(declared) {
		decl, err := in.registry.lookupTypeName(declared)
		if err != nil {
			return fmt.Errorf("%s: %v", what, err)
		}
		if iface, ok := decl.(*InterfaceDecl); ok {
			return fmt.Errorf("%s: значение типа %s (%s) не подходит под интерфейс %s: %v",
				what, v.TypeName(), Display(v), iface.Qualified(), in.conforms(v, iface))
		}
	}
	// Формулировка нейтральна по роду: what бывает и переменной,
	// и параметром, и элементом цикла.
	return fmt.Errorf("%s: объявлен тип %s, получено значение типа %s (%s)",
		what, declared.Russian(), v.TypeName(), Display(v))
}

// isBuiltinType — встроенный тип или пустая аннотация.
func isBuiltinType(t TypeName) bool {
	if t == "" {
		return true
	}
	_, err := ParseTypeName(string(t))
	return err == nil
}

// conforms объясняет, чего значению не хватает до интерфейса; nil —
// подходит.
func (in *Interpreter) conforms(v Value, iface *InterfaceDecl) error {
	// Метод по умолчанию принимает сам интерфейс: проверяя, подходит ли
	// значение, считаем, что подходит, — иначе проверка зациклилась бы.
	if in.conforming[iface] {
		return nil
	}
	if in.conforming == nil {
		in.conforming = map[*InterfaceDecl]bool{}
	}
	in.conforming[iface] = true
	defer delete(in.conforming, iface)

	for _, field := range iface.Fields {
		value, ok := memberOf(v, field.Name)
		if !ok {
			return fmt.Errorf("нет поля %s", field.Name)
		}
		if field.Type != "" && !in.accepts(field.Type, value) {
			return fmt.Errorf("поле %s: нужен тип %s, а там %s", field.Name, field.Type.Russian(), value.TypeName())
		}
	}
	for _, method := range iface.Methods {
		if !in.hasMethod(v, method.Name, len(method.Params)) {
			how := "единственным аргументом"
			if n := len(method.Params) + 1; n > 1 {
				how = fmt.Sprintf("первым из %d аргументов", n)
			}
			return fmt.Errorf("нет метода %s(%s): ни поля-функции, ни алиаса или встроенной функции %s, "+
				"которая принимает это значение %s", method.Name, strings.Join(method.Params, ", "), method.Name, how)
		}
	}
	return nil
}

// hasMethod — можно ли вызвать v.name с n аргументами.
func (in *Interpreter) hasMethod(v Value, name string, n int) bool {
	if member, ok := memberOf(v, name); ok && isCallable(member) {
		return true
	}
	if decl, err := in.registry.Lookup("", name); err == nil {
		if len(decl.Params) != n+1 {
			return false
		}
		first := decl.ParamTypes[0]
		return first == "" || in.accepts(first, v)
	}
	if builtin, ok := in.builtins[name]; ok {
		return builtin.checkArity(n+1) == nil
	}
	return false
}

// memberOf — поле записи или ключ словаря.
func memberOf(v Value, name string) (Value, bool) {
	switch val := v.(type) {
	case *RecordValue:
		value, ok := val.Fields[name]
		return value, ok
	case MapValue:
		value, ok := val[name]
		return value, ok
	}
	return nil, false
}

// fieldOf — x.поле: поле записи, ключ словаря или параметр конфига.
func (in *Interpreter) fieldOf(v Value, name string) (Value, error) {
	switch val := v.(type) {
	case *RecordValue:
		if value, ok := val.Fields[name]; ok {
			return value, nil
		}
		names := make([]string, len(val.Decl.Fields))
		for i, f := range val.Decl.Fields {
			names[i] = f.Name
		}
		return nil, fmt.Errorf("у записи %s нет поля %q; есть: %s", val.Decl.Qualified(), name, strings.Join(names, ", "))
	case MapValue:
		if value, ok := val[name]; ok {
			return value, nil
		}
		return nil, fmt.Errorf("в словаре нет ключа %q; есть: %s", name, strings.Join(val.SortedKeys(), ", "))
	case ConfigValue:
		value, err := val.Lookup(name)
		if err != nil {
			return nil, err
		}
		return StringValue(value), nil
	}
	return nil, fmt.Errorf("у значения типа %s нет полей: .%s читают у записи, словаря или конфига", v.TypeName(), name)
}

// setMember — x.поле = значение.
func (in *Interpreter) setMember(target Value, name string, value Value) error {
	switch val := target.(type) {
	case *RecordValue:
		field, ok := val.Decl.field(name)
		if !ok {
			_, err := in.fieldOf(val, name)
			return err
		}
		if field.Type != "" {
			if err := in.checkType(field.Type, fmt.Sprintf("поле %s записи %s", name, val.Decl.Qualified()), value); err != nil {
				return err
			}
		}
		val.Fields[name] = value
		return nil
	case MapValue:
		val[name] = value
		return nil
	case ConfigValue:
		val.Set(name, value.Text())
		return nil
	}
	return fmt.Errorf("поле .%s можно записать у записи, словаря или конфига, а не у значения типа %s", name, target.TypeName())
}

// setIndex — x[ключ] = значение.
func (in *Interpreter) setIndex(target, index, value Value) error {
	switch val := target.(type) {
	case MapValue, ConfigValue, *RecordValue:
		return in.setMember(val, index.Text(), value)
	case ListValue:
		n, ok := index.(IntValue)
		if !ok {
			return fmt.Errorf("индекс списка — целое, получено %s", index.TypeName())
		}
		i := int(n)
		if i < 0 {
			i += len(val)
		}
		if i < 0 || i >= len(val) {
			return fmt.Errorf("индекс %d вне списка длиной %d", int(n), len(val))
		}
		val[i] = value
		return nil
	}
	return fmt.Errorf("присвоить по индексу можно в список, словарь или конфиг, а не в %s", target.TypeName())
}

// construct создаёт запись: аргументы по порядку полей и по именам,
// пропущенные поля — значения по умолчанию.
func (in *Interpreter) construct(decl *RecordDecl, args []Value, names []string) (Value, error) {
	params := make([]string, len(decl.Fields))
	for i, f := range decl.Fields {
		params[i] = f.Name
	}
	bound, err := bindArgs("запись "+decl.Qualified(), "поля", params, args, names)
	if err != nil {
		return nil, err
	}
	record := &RecordValue{Decl: decl, Fields: make(map[string]Value, len(decl.Fields))}
	for i, field := range decl.Fields {
		value := bound[i]
		if value == nil {
			if field.Default == nil {
				return nil, fmt.Errorf("запись %s: не задано поле %s", decl.Qualified(), field.Name)
			}
			if value, err = in.eval(field.Default, newScope(nil)); err != nil {
				return nil, err
			}
		}
		if field.Type != "" {
			if err := in.checkType(field.Type, fmt.Sprintf("поле %s записи %s", field.Name, decl.Qualified()), value); err != nil {
				return nil, err
			}
		}
		record.Fields[field.Name] = value
	}
	return record, nil
}

// bindArgs раскладывает аргументы по параметрам: сначала по порядку,
// затем по именам. Непереданный параметр — nil.
func bindArgs(what, kind string, params []string, args []Value, names []string) ([]Value, error) {
	bound := make([]Value, len(params))
	for i, arg := range args {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		if name == "" {
			if i >= len(params) {
				return nil, fmt.Errorf("%s принимает %d %s, передано %d", what, len(params),
					plural(len(params), "аргумент", "аргумента", "аргументов"), len(args))
			}
			bound[i] = arg
			continue
		}
		index := -1
		for j, param := range params {
			if param == name {
				index = j
			}
		}
		if index < 0 {
			return nil, fmt.Errorf("%s: нет %s %q; есть: %s", what, kind, name, strings.Join(params, ", "))
		}
		if bound[index] != nil {
			return nil, fmt.Errorf("%s: %s передан дважды — по порядку и по имени", what, name)
		}
		bound[index] = arg
	}
	return bound, nil
}

// fillArgs — аргументы алиаса или функции: именованные разложены по
// параметрам, все обязательны.
func fillArgs(what string, params []string, args []Value, names []string) ([]Value, error) {
	if unnamed(names) {
		// Число аргументов по порядку проверяет сам вызов.
		return args, nil
	}
	bound, err := bindArgs(what, "параметра", params, args, names)
	if err != nil {
		return nil, err
	}
	for i, value := range bound {
		if value == nil {
			return nil, fmt.Errorf("%s: не передан параметр %s", what, params[i])
		}
	}
	return bound, nil
}

// ---------------------------------------------------------------------------
// Встроенные функции о типах
// ---------------------------------------------------------------------------

func typeBuiltins() []Builtin {
	return []Builtin{
		{Name: "type_of", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "тип значения: int, float, string, bool, list, map, config, stream, fn, morph, functor, null или имя записи",
			Fn:  builtinTypeOf},
		{Name: "is", MinArgs: 2, MaxArgs: 2, Returns: TypeBool,
			Doc: "подходит ли значение под тип, в том числе запись или интерфейс: is(x, \"Сервер\")", Fn: builtinIs},
	}
}

func builtinTypeOf(_ *Interpreter, args []Value) (Value, error) {
	switch v := args[0].(type) {
	case NullValue:
		return StringValue("null"), nil
	case *RecordValue:
		return StringValue(v.Decl.Qualified()), nil
	}
	return StringValue(typeOf(args[0])), nil
}

func builtinIs(in *Interpreter, args []Value) (Value, error) {
	name := strings.TrimSpace(args[1].Text())
	t, err := ParseTypeName(name)
	if err != nil {
		if _, lookupErr := in.registry.lookupTypeName(TypeName(name)); lookupErr != nil {
			return nil, fmt.Errorf("is: %v", lookupErr)
		}
		t = TypeName(name)
	}
	return BoolValue(in.accepts(t, args[0])), nil
}
