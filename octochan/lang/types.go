package lang

import (
	"fmt"
	"sort"
	"strings"
)

// TypeName — имя объявленного типа.
//
// Аннотации необязательны: без них поведение прежнее, а написанный тип
// проверяется при каждом присваивании и при передаче аргумента. Смысл
// в том, чтобы ошибка всплывала на входе в алиас, а не в его середине —
// особенно когда алиас пришёл из чужого пакета и его тело читать неудобно.
type TypeName string

const (
	TypeAny     TypeName = "any"
	TypeInt     TypeName = "int"
	TypeFloat   TypeName = "float"
	TypeNumber  TypeName = "number"
	TypeString  TypeName = "string"
	TypeBool    TypeName = "bool"
	TypeList    TypeName = "list"
	TypeMap     TypeName = "map"
	TypeConfig  TypeName = "config"
	TypeStream  TypeName = "stream"
	TypeFunc    TypeName = "fn"
	TypeMorph   TypeName = "morph"
	TypeFunctor TypeName = "functor"
)

// typeAliases позволяет писать имена типов и латиницей, и по-русски:
// ключевые слова языка латинские, но идентификаторы чаще русские,
// и заставлять переключать раскладку посреди строки незачем.
var typeAliases = map[string]TypeName{
	"any":        TypeAny,
	"любой":      TypeAny,
	"int":        TypeInt,
	"целое":      TypeInt,
	"float":      TypeFloat,
	"дробное":    TypeFloat,
	"number":     TypeNumber,
	"число":      TypeNumber,
	"string":     TypeString,
	"строка":     TypeString,
	"bool":       TypeBool,
	"логическое": TypeBool,
	"list":       TypeList,
	"список":     TypeList,
	"map":        TypeMap,
	"dict":       TypeMap,
	"словарь":    TypeMap,
	"config":     TypeConfig,
	"конфиг":     TypeConfig,
	"stream":     TypeStream,
	"поток":      TypeStream,
	"fn":         TypeFunc,
	"func":       TypeFunc,
	"функция":    TypeFunc,
	"morph":      TypeMorph,
	"морфизм":    TypeMorph,
	"functor":    TypeFunctor,
	"функтор":    TypeFunctor,
}

// ParseTypeName распознаёт имя типа.
func ParseTypeName(name string) (TypeName, error) {
	if t, ok := typeAliases[strings.ToLower(name)]; ok {
		return t, nil
	}
	return "", fmt.Errorf("неизвестный тип %q; доступны: %s", name, strings.Join(knownTypeNames(), ", "))
}

func knownTypeNames() []string {
	seen := make(map[TypeName]bool)
	var names []string
	for name, t := range typeAliases {
		// Показываем по одному написанию на тип, чтобы список не двоился.
		if !seen[t] {
			seen[t] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Russian возвращает имя типа для сообщений об ошибках.
func (t TypeName) Russian() string {
	switch t {
	case TypeAny:
		return "любой"
	case TypeInt:
		return "целое"
	case TypeFloat:
		return "дробное"
	case TypeNumber:
		return "число"
	case TypeString:
		return "строка"
	case TypeBool:
		return "логическое"
	case TypeList:
		return "список"
	case TypeMap:
		return "словарь"
	case TypeConfig:
		return "конфиг"
	case TypeStream:
		return "поток"
	case TypeFunc:
		return "функция"
	case TypeMorph:
		return "морфизм"
	case TypeFunctor:
		return "функтор"
	default:
		return string(t)
	}
}

// Accepts сообщает, подходит ли значение объявленному типу.
//
// Единственное послабление — целое там, где объявлено дробное или число:
// расширение без потери данных. Строка числом не считается, иначе
// аннотация не ловила бы ровно ту ошибку, ради которой её пишут.
func (t TypeName) Accepts(v Value) bool {
	if t == TypeAny || t == "" {
		return true
	}

	switch t {
	case TypeInt:
		_, ok := v.(IntValue)
		return ok
	case TypeFloat:
		switch v.(type) {
		case FloatValue, IntValue:
			return true
		}
		return false
	case TypeNumber:
		switch v.(type) {
		case IntValue, FloatValue:
			return true
		}
		return false
	case TypeString:
		_, ok := v.(StringValue)
		return ok
	case TypeBool:
		_, ok := v.(BoolValue)
		return ok
	case TypeList:
		_, ok := v.(ListValue)
		return ok
	case TypeMap:
		_, ok := v.(MapValue)
		return ok
	case TypeConfig:
		_, ok := v.(ConfigValue)
		return ok
	case TypeStream:
		_, ok := v.(StreamValue)
		return ok
	case TypeFunc:
		_, ok := v.(*FuncValue)
		return ok
	case TypeMorph:
		_, ok := v.(*MorphValue)
		return ok
	case TypeFunctor:
		_, ok := v.(*FunctorValue)
		return ok
	}
	return false
}

// typeOf — тип значения: им становится тип переменной, объявленной без
// аннотации. null типа не задаёт: такая переменная — any.
func typeOf(v Value) TypeName {
	switch v.(type) {
	case IntValue:
		return TypeInt
	case FloatValue:
		return TypeFloat
	case StringValue:
		return TypeString
	case BoolValue:
		return TypeBool
	case ListValue:
		return TypeList
	case MapValue:
		return TypeMap
	case ConfigValue:
		return TypeConfig
	case StreamValue:
		return TypeStream
	case *FuncValue:
		return TypeFunc
	case *MorphValue:
		return TypeMorph
	case *FunctorValue:
		return TypeFunctor
	}
	return TypeAny
}

// zeroValue — значение переменной, объявленной без него: `var n: int`.
func zeroValue(t TypeName) (Value, error) {
	switch t {
	case TypeInt, TypeNumber:
		return IntValue(0), nil
	case TypeFloat:
		return FloatValue(0), nil
	case TypeString:
		return StringValue(""), nil
	case TypeBool:
		return BoolValue(false), nil
	case TypeList:
		return ListValue{}, nil
	case TypeMap:
		return MapValue{}, nil
	case TypeAny:
		return NullValue{}, nil
	}
	return nil, fmt.Errorf("у типа %s нет нулевого значения: объявите переменную со значением", t.Russian())
}

// reassignError — значение не того типа для объявленной переменной.
func reassignError(name string, declared TypeName, inferred bool, v Value) error {
	return fmt.Errorf("переменная %s: %s, присваивается значение типа %s (%s)%s",
		name, typeOrigin(declared, inferred), v.TypeName(), Display(v), reassignHint(name, declared, typeOf(v)))
}

func typeOrigin(declared TypeName, inferred bool) string {
	if inferred {
		return "тип " + declared.Russian() + " задан первым значением"
	}
	return "объявлен тип " + declared.Russian()
}

// reassignHint подсказывает, как объявить переменную, если ей нужны
// значения разных типов: целое и дробное — number, остальное — any.
func reassignHint(name string, declared, actual TypeName) string {
	want := TypeAny
	if (declared == TypeInt || declared == TypeFloat) && (actual == TypeInt || actual == TypeFloat) {
		want = TypeNumber
	}
	return fmt.Sprintf(": тип переменной не меняется; для таких значений объявите её как var %s: %s", name, want)
}

// checkType проверяет значение и возвращает понятную ошибку.
func checkType(declared TypeName, what string, v Value) error {
	if declared.Accepts(v) {
		return nil
	}
	// Формулировка нейтральна по роду: what бывает и переменной,
	// и параметром, и элементом цикла.
	return fmt.Errorf("%s: объявлен тип %s, получено значение типа %s (%s)",
		what, declared.Russian(), v.TypeName(), Display(v))
}
