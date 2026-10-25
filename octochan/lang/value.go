package lang

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"octochan/config"
)

// Value — значение языка.
//
// Модель намеренно разводит два понятия, которые в наброске языка были
// смешаны: «операция не выполнилась» — это ошибка, прерывающая сценарий,
// а не число, а Value — то, что операция вычислила. Поэтому diff возвращает
// количество расхождений, и условие пишется как diff(a, b) != 0.
type Value interface {
	// Text — представление значения в конвейере и при печати.
	Text() string
	// Truthy — истинность в условиях.
	Truthy() bool
	// TypeName — имя типа для сообщений об ошибках.
	TypeName() string
}

// NullValue — отсутствие значения.
type NullValue struct{}

func (NullValue) Text() string     { return "" }
func (NullValue) Truthy() bool     { return false }
func (NullValue) TypeName() string { return "null" }

// BoolValue — логическое значение.
type BoolValue bool

func (v BoolValue) Text() string {
	if v {
		return "true"
	}
	return "false"
}
func (v BoolValue) Truthy() bool   { return bool(v) }
func (BoolValue) TypeName() string { return "логическое" }

// IntValue — целое число.
type IntValue int64

func (v IntValue) Text() string   { return strconv.FormatInt(int64(v), 10) }
func (v IntValue) Truthy() bool   { return v != 0 }
func (IntValue) TypeName() string { return "целое" }

// FloatValue — дробное число.
type FloatValue float64

func (v FloatValue) Text() string   { return strconv.FormatFloat(float64(v), 'f', -1, 64) }
func (v FloatValue) Truthy() bool   { return v != 0 }
func (FloatValue) TypeName() string { return "дробное" }

// StringValue — строка.
type StringValue string

func (v StringValue) Text() string   { return string(v) }
func (v StringValue) Truthy() bool   { return len(v) > 0 }
func (StringValue) TypeName() string { return "строка" }

// ListValue — список значений.
type ListValue []Value

func (v ListValue) Text() string {
	parts := make([]string, len(v))
	for i, item := range v {
		parts[i] = item.Text()
	}
	// Перевод строки как разделитель выбран ради конвейеров: список,
	// отданный в текстовую операцию, выглядит как построчный вывод.
	return strings.Join(parts, "\n")
}
func (v ListValue) Truthy() bool   { return len(v) > 0 }
func (ListValue) TypeName() string { return "список" }

// MapValue — словарь: строковый ключ и произвольное значение.
// Нужен, чтобы держать в переменной карту вида «сервер -> адрес».
type MapValue map[string]Value

// SortedKeys возвращает ключи в устойчивом порядке.
// Обход map в Go случаен, а сценарий, печатающий разное при одинаковых
// данных, отлаживать невозможно.
func (v MapValue) SortedKeys() []string {
	keys := make([]string, 0, len(v))
	for key := range v {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (v MapValue) Text() string {
	var b strings.Builder
	for i, key := range v.SortedKeys() {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s=%s", key, v[key].Text())
	}
	return b.String()
}
func (v MapValue) Truthy() bool   { return len(v) > 0 }
func (MapValue) TypeName() string { return "словарь" }

// ConfigValue — разобранный конфиг.
//
// Главное отличие от строки с путём: файл читается и разбирается один раз.
// Раньше каждый вызов diff или param по имени файла парсил его заново,
// и сценарий из трёх обращений к одному конфигу делал три разбора.
type ConfigValue struct {
	// Name — источник, показывается в отчётах и сообщениях об ошибках.
	// Для живых источников не содержит пароля.
	Name string
	Data config.ConfigMap
	// Notes — замечания источника, например параметры, ждущие перезапуска.
	Notes []string
}

// Params возвращает число параметров во всех секциях.
func (v ConfigValue) Params() int {
	total := 0
	for _, section := range v.Data {
		total += len(section)
	}
	return total
}

// SortedKeys возвращает имена параметров в виде секция.ключ,
// отсортированные для устойчивого вывода.
func (v ConfigValue) SortedKeys() []string {
	var keys []string
	for section, params := range v.Data {
		for key := range params {
			if section == "" {
				keys = append(keys, key)
			} else {
				keys = append(keys, section+"."+key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// Lookup ищет параметр по имени.
//
// Имя вида "секция.ключ" адресует явно. Короткое имя ищется сначала
// среди параметров без секции, затем по всем секциям; совпадение
// в нескольких секциях — ошибка, а не случайный выбор одной из них.
func (v ConfigValue) Lookup(name string) (string, error) {
	if section, key, found := strings.Cut(name, "."); found {
		if value, ok := v.Data[section][key]; ok {
			return value, nil
		}
		return "", fmt.Errorf("параметр %q не найден в %s", name, v.Name)
	}

	if value, ok := v.Data[""][name]; ok {
		return value, nil
	}

	var (
		found    string
		sections []string
	)
	for section, params := range v.Data {
		if section == "" {
			continue
		}
		if value, ok := params[name]; ok {
			found = value
			sections = append(sections, section)
		}
	}

	switch len(sections) {
	case 0:
		return "", fmt.Errorf("параметр %q не найден в %s", name, v.Name)
	case 1:
		return found, nil
	default:
		sort.Strings(sections)
		return "", fmt.Errorf("параметр %q есть в секциях %s; уточните: \"%s.%s\"",
			name, strings.Join(sections, ", "), sections[0], name)
	}
}

// Set записывает параметр, создавая секцию при необходимости.
func (v ConfigValue) Set(name, value string) {
	section, key, found := strings.Cut(name, ".")
	if !found {
		section, key = "", name
	}
	if _, ok := v.Data[section]; !ok {
		v.Data[section] = make(map[string]string)
	}
	v.Data[section][key] = value
}

func (v ConfigValue) Text() string {
	var b strings.Builder
	for i, key := range v.SortedKeys() {
		value, err := v.Lookup(key)
		if err != nil {
			continue
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s = %s", key, value)
	}
	return b.String()
}
func (v ConfigValue) Truthy() bool   { return v.Params() > 0 }
func (ConfigValue) TypeName() string { return "конфиг" }

// StreamKind — куда пишет поток.
type StreamKind string

const (
	StreamStdout StreamKind = "stdout"
	StreamStderr StreamKind = "stderr"
	StreamFile   StreamKind = "file"
)

// StreamValue — поток вывода для print: консоль, поток ошибок или файл.
//
// Сам поток ничего не открывает: это адрес. Файл открывается при каждой
// печати, и запись в него — такое же действие сценария, как write: она
// попадает в план, проверяется песочницей и откатывается.
type StreamValue struct {
	Kind StreamKind
	// Path — путь файла, как его написали в сценарии.
	Path string
}

func (v StreamValue) Text() string {
	if v.Kind == StreamFile {
		return v.Path
	}
	return string(v.Kind)
}
func (StreamValue) Truthy() bool     { return true }
func (StreamValue) TypeName() string { return "поток" }

// FuncValue — функция как значение: её передают аргументом, кладут в
// список, возвращают из алиаса и вызывают.
//
// Ровно один из видов:
//   - лямбда: Params и Body или Expr, со снимком переменных env;
//   - ссылка на алиас (Decl) или встроенную функцию (Builtin);
//   - частичное применение: Base с первыми аргументами Bound;
//   - композиция: Chain, применяемая по порядку.
//
// Значение передаётся указателем: две функции равны, только если это
// одна и та же функция.
type FuncValue struct {
	Name       string
	Params     []string
	ParamTypes []TypeName
	Returns    TypeName
	Body       *Block
	Expr       Expr
	env        *scope

	Decl    *AliasDecl
	Builtin string

	Base  *FuncValue
	Bound []Value

	Chain []*FuncValue
}

func (f *FuncValue) Text() string {
	switch {
	case f.Decl != nil:
		if f.Decl.Module != "" {
			return "fn " + f.Decl.Module + "." + f.Decl.Name
		}
		return "fn " + f.Decl.Name
	case f.Builtin != "":
		return "fn " + f.Builtin
	case f.Base != nil:
		parts := []string{f.Base.Text()}
		for _, v := range f.Bound {
			parts = append(parts, Display(v))
		}
		return "partial(" + strings.Join(parts, ", ") + ")"
	case f.Chain != nil:
		parts := make([]string, len(f.Chain))
		for i, step := range f.Chain {
			parts[i] = step.Text()
		}
		return strings.Join(parts, " >> ")
	}
	name := "fn"
	if f.Name != "" {
		name = "fn " + f.Name
	}
	return name + "(" + strings.Join(f.Params, ", ") + ")"
}
func (*FuncValue) Truthy() bool     { return true }
func (*FuncValue) TypeName() string { return "функция" }

// RecordValue — запись: значения полей по объявлению. Передаётся
// указателем, как словарь: поле, изменённое в алиасе, изменено и у
// вызывающего.
type RecordValue struct {
	Decl   *RecordDecl
	Fields map[string]Value
}

func (r *RecordValue) Text() string { return Display(r) }
func (*RecordValue) Truthy() bool   { return true }
func (r *RecordValue) TypeName() string {
	return r.Decl.Qualified()
}

// Display — человекочитаемое представление для печати и отладки.
func Display(v Value) string {
	switch val := v.(type) {
	case NullValue:
		return "null"
	case StringValue:
		return string(val)
	case ListValue:
		parts := make([]string, len(val))
		for i, item := range val {
			parts[i] = Display(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case MapValue:
		keys := val.SortedKeys()
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = fmt.Sprintf("%q: %s", key, Display(val[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case ConfigValue:
		return fmt.Sprintf("конфиг %s (%d параметров)", val.Name, val.Params())
	case StreamValue:
		if val.Kind == StreamFile {
			return "поток в файл " + val.Path
		}
		return "поток " + string(val.Kind)
	case *RecordValue:
		parts := make([]string, len(val.Decl.Fields))
		for i, field := range val.Decl.Fields {
			item := val.Fields[field.Name]
			shown := Display(item)
			if _, isString := item.(StringValue); isString {
				shown = strconv.Quote(shown)
			}
			parts[i] = field.Name + ": " + shown
		}
		return val.Decl.Qualified() + "(" + strings.Join(parts, ", ") + ")"
	default:
		return v.Text()
	}
}

// numeric приводит значение к числу, если это возможно без потери смысла.
func numeric(v Value) (float64, bool) {
	switch val := v.(type) {
	case IntValue:
		return float64(val), true
	case FloatValue:
		return float64(val), true
	case BoolValue:
		if val {
			return 1, true
		}
		return 0, true
	case StringValue:
		f, err := strconv.ParseFloat(strings.TrimSpace(string(val)), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func isInt(v Value) bool {
	_, ok := v.(IntValue)
	return ok
}

// equalValues сравнивает значения: числа — численно, функции — по
// тождеству, остальное — по тексту.
func equalValues(a, b Value) bool {
	// Дельты равны, если требуют и меняют одно и то же; функции, морфизмы-
	// функции и функторы — только сами себе.
	if am, ok := a.(*MorphValue); ok {
		if bm, ok := b.(*MorphValue); ok && am.isDelta() && bm.isDelta() {
			return sameDelta(am, bm)
		}
		return a == b
	}
	if isCallable(a) || isCallable(b) {
		return a == b
	}
	if ar, ok := a.(*RecordValue); ok {
		br, ok := b.(*RecordValue)
		if !ok || ar.Decl != br.Decl {
			return false
		}
		for _, field := range ar.Decl.Fields {
			if !equalValues(ar.Fields[field.Name], br.Fields[field.Name]) {
				return false
			}
		}
		return true
	}
	if _, ok := b.(*RecordValue); ok {
		return false
	}
	if _, aNull := a.(NullValue); aNull {
		_, bNull := b.(NullValue)
		return bNull
	}
	if _, bNull := b.(NullValue); bNull {
		return false
	}

	if aList, ok := a.(ListValue); ok {
		bList, ok := b.(ListValue)
		if !ok || len(aList) != len(bList) {
			return false
		}
		for i := range aList {
			if !equalValues(aList[i], bList[i]) {
				return false
			}
		}
		return true
	}

	if an, aok := numeric(a); aok {
		if bn, bok := numeric(b); bok {
			return an == bn
		}
	}
	return a.Text() == b.Text()
}

// compareValues возвращает -1, 0 или 1. Ошибка означает несравнимые типы.
func compareValues(a, b Value) (int, error) {
	if an, aok := numeric(a); aok {
		if bn, bok := numeric(b); bok {
			switch {
			case an < bn:
				return -1, nil
			case an > bn:
				return 1, nil
			default:
				return 0, nil
			}
		}
	}

	as, aok := a.(StringValue)
	bs, bok := b.(StringValue)
	if aok && bok {
		return strings.Compare(string(as), string(bs)), nil
	}

	return 0, fmt.Errorf("нельзя сравнить %s и %s", a.TypeName(), b.TypeName())
}

// FromText превращает текст в значение, распознавая числа.
// Используется на границе с конвейером и встроенными функциями.
func FromText(s string) Value {
	trimmed := strings.TrimSpace(s)
	if trimmed != "" {
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return IntValue(n)
		}
	}
	return StringValue(s)
}
