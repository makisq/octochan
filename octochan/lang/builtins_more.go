package lang

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// Встроенные функции второй очереди: форматирование, математика, время,
// случайность, сериализация, eval и чтение по HTTP.
//
// Два правила держат их в рамках языка, где план обязан совпасть с
// выполнением:
//
//   - время и случайность детерминированы: now() — время запуска сценария,
//     rand() — от зерна; в заявке оба фиксируются, и выполнение видит то же,
//     что план;
//   - наружу — только чтение: fetch и curl умеют GET и HEAD, а запись во
//     внешние системы — дело исполнителей и заявок, а не встроенной функции,
//     которая в режиме плана выполнилась бы по-настоящему.
func moreBuiltins() []Builtin {
	return []Builtin{
		{Name: "format", MinArgs: 1, MaxArgs: -1, Returns: TypeString,
			Doc: "строка по образцу: format(\"id=%d, %s\", n, имя); %d %f %.2f %s %v %q %x %t %%",
			Fn:  builtinFormat},
		{Name: "float", MinArgs: 1, MaxArgs: 1, Returns: TypeFloat,
			Doc: "привести к дробному", Fn: builtinFloat},
		{Name: "sqrt", MinArgs: 1, MaxArgs: 1, Returns: TypeFloat,
			Doc: "квадратный корень", Fn: mathFn("sqrt", math.Sqrt, true)},
		{Name: "floor", MinArgs: 1, MaxArgs: 1, Returns: TypeInt,
			Doc: "округлить вниз до целого", Fn: roundFn(math.Floor)},
		{Name: "ceil", MinArgs: 1, MaxArgs: 1, Returns: TypeInt,
			Doc: "округлить вверх до целого", Fn: roundFn(math.Ceil)},
		{Name: "round", MinArgs: 1, MaxArgs: 2,
			Doc: "округлить: round(x) — до целого, round(x, 2) — до сотых", Fn: builtinRound},
		{Name: "abs", MinArgs: 1, MaxArgs: 1,
			Doc: "модуль числа", Fn: builtinAbs},
		{Name: "pow", MinArgs: 2, MaxArgs: 2, Returns: TypeFloat,
			Doc: "степень: pow(2, 10)", Fn: builtinPow},
		{Name: "min", MinArgs: 1, MaxArgs: -1,
			Doc: "наименьшее: min(a, b, …) или min(список)", Fn: extremum(-1)},
		{Name: "max", MinArgs: 1, MaxArgs: -1,
			Doc: "наибольшее: max(a, b, …) или max(список)", Fn: extremum(1)},
		{Name: "rand", MinArgs: 0, MaxArgs: 2,
			Doc: "случайное: rand() — дробное от 0 до 1, rand(n) — целое от 0 до n-1, rand(a, b) — от a до b; в заявке зерно фиксируется",
			Fn:  builtinRand},
		{Name: "choice", MinArgs: 1, MaxArgs: 1,
			Doc: "случайный элемент списка", Fn: builtinChoice},
		{Name: "shuffle", MinArgs: 1, MaxArgs: 1, Returns: TypeList,
			Doc: "список в случайном порядке", Fn: builtinShuffle},
		{Name: "now", MinArgs: 0, MaxArgs: 0, Returns: TypeString,
			Doc: "время запуска сценария (RFC 3339); в заявке — время построения плана", Fn: builtinNow},
		{Name: "sleep", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "пауза: sleep(2) — секунды, sleep(\"500ms\"); в режиме плана не ждёт", Fn: builtinSleep},
		{Name: "time.now", MinArgs: 0, MaxArgs: 0, Returns: TypeString,
			Doc: "то же, что now()", Fn: builtinNow},
		{Name: "time.unix", MinArgs: 0, MaxArgs: 1, Returns: TypeInt,
			Doc: "секунды Unix: времени запуска или переданного времени", Fn: builtinUnix},
		{Name: "time.format", MinArgs: 1, MaxArgs: 2, Returns: TypeString,
			Doc: "время по образцу %Y-%m-%d %H:%M:%S: time.format(образец[, время])", Fn: builtinTimeFormat},
		{Name: "time.add", MinArgs: 2, MaxArgs: 2, Returns: TypeString,
			Doc: "время плюс длительность: time.add(now(), \"1h30m\")", Fn: builtinTimeAdd},
		{Name: "time.sleep", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "то же, что sleep()", Fn: builtinSleep},
		{Name: "to_json", MinArgs: 1, MaxArgs: 2, Returns: TypeString,
			Doc: "значение в JSON; to_json(x, true) — с отступами", Fn: builtinToJSON},
		{Name: "from_json", MinArgs: 1, MaxArgs: 1,
			Doc: "разобрать JSON", Fn: builtinFromJSON},
		{Name: "to_yaml", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "значение в YAML", Fn: builtinToYAML},
		{Name: "from_yaml", MinArgs: 1, MaxArgs: 1,
			Doc: "разобрать YAML", Fn: builtinFromYAML},
		{Name: "to_toml", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "словарь в TOML", Fn: builtinToTOML},
		{Name: "from_toml", MinArgs: 1, MaxArgs: 1, Returns: TypeMap,
			Doc: "разобрать TOML", Fn: builtinFromTOML},
		{Name: "eval", MinArgs: 1, MaxArgs: 1,
			Doc: "вычислить выражение языка из строки: eval(\"2 + 3 * 4\"); проверяется перед вычислением, алиасы недоступны", Fn: builtinEval},
		{Name: "fetch", MinArgs: 1, MaxArgs: 2, Returns: TypeString,
			Doc: "тело ответа HTTP GET: fetch(url[, {\"headers\": {…}, \"timeout\": \"10s\"}]); в песочнице — только хосты из --allow-net",
			Fn:  builtinFetch},
		{Name: "curl", MinArgs: 1, MaxArgs: 2, Returns: TypeMap,
			Doc: "ответ HTTP целиком: {status, headers, body}; method — GET или HEAD", Fn: builtinCurl},
	}
}

// ---------------------------------------------------------------------------
// format
// ---------------------------------------------------------------------------

func builtinFormat(_ *Interpreter, args []Value) (Value, error) {
	layout, ok := args[0].(StringValue)
	if !ok {
		return nil, fmt.Errorf("первый аргумент — образец строкой, получено %s", args[0].TypeName())
	}
	text, err := formatValues(string(layout), args[1:])
	if err != nil {
		return nil, err
	}
	return StringValue(text), nil
}

// formatValues — printf с проверкой: каждому глаголу — значение подходящего
// типа, лишних и недостающих значений нет. Иначе Go напечатал бы
// %!d(string=…) прямо в отчёт.
func formatValues(layout string, args []Value) (string, error) {
	var b strings.Builder
	next := 0
	for i := 0; i < len(layout); i++ {
		if layout[i] != '%' {
			b.WriteByte(layout[i])
			continue
		}
		j := i + 1
		for j < len(layout) && strings.IndexByte("-+# 0", layout[j]) >= 0 {
			j++
		}
		for j < len(layout) && layout[j] >= '0' && layout[j] <= '9' {
			j++
		}
		if j < len(layout) && layout[j] == '.' {
			j++
			for j < len(layout) && layout[j] >= '0' && layout[j] <= '9' {
				j++
			}
		}
		if j >= len(layout) {
			return "", errors.New("образец обрывается на %: двойной %% — сам знак процента")
		}
		verb, spec := layout[j], layout[i:j+1]
		i = j
		if verb == '%' {
			b.WriteByte('%')
			continue
		}
		if next >= len(args) {
			return "", fmt.Errorf("для %s не хватает значения: передано %d", spec, len(args))
		}
		arg, err := formatArg(verb, args[next])
		if err != nil {
			return "", fmt.Errorf("%s, значение %d: %w", spec, next+1, err)
		}
		next++
		b.WriteString(fmt.Sprintf(spec, arg))
	}
	if next < len(args) {
		return "", fmt.Errorf("лишние значения: в образце %d, передано %d", next, len(args))
	}
	return b.String(), nil
}

func formatArg(verb byte, v Value) (any, error) {
	switch verb {
	case 'd', 'x', 'X', 'o', 'b':
		switch val := v.(type) {
		case IntValue:
			return int64(val), nil
		case FloatValue:
			if float64(val) == math.Trunc(float64(val)) {
				return int64(val), nil
			}
		case StringValue:
			if verb == 'x' || verb == 'X' {
				return string(val), nil
			}
		}
		return nil, fmt.Errorf("нужно целое, получено %s", v.TypeName())
	case 'f', 'F', 'e', 'E', 'g', 'G':
		switch val := v.(type) {
		case IntValue:
			return float64(val), nil
		case FloatValue:
			return float64(val), nil
		}
		return nil, fmt.Errorf("нужно число, получено %s", v.TypeName())
	case 't':
		if val, ok := v.(BoolValue); ok {
			return bool(val), nil
		}
		return nil, fmt.Errorf("нужно логическое, получено %s", v.TypeName())
	case 's', 'v':
		return Display(v), nil
	case 'q':
		return Display(v), nil
	}
	return nil, fmt.Errorf("неизвестный глагол %%%c: доступны %%d %%f %%e %%g %%s %%v %%q %%x %%o %%b %%t", verb)
}

// ---------------------------------------------------------------------------
// Математика
// ---------------------------------------------------------------------------

func number(v Value) (float64, error) {
	switch val := v.(type) {
	case IntValue:
		return float64(val), nil
	case FloatValue:
		return float64(val), nil
	}
	return 0, fmt.Errorf("нужно число, получено %s", v.TypeName())
}

func builtinFloat(_ *Interpreter, args []Value) (Value, error) {
	if n, ok := numeric(args[0]); ok {
		return FloatValue(n), nil
	}
	return nil, fmt.Errorf("%q не число", args[0].Text())
}

func mathFn(name string, fn func(float64) float64, nonNegative bool) func(*Interpreter, []Value) (Value, error) {
	return func(_ *Interpreter, args []Value) (Value, error) {
		x, err := number(args[0])
		if err != nil {
			return nil, err
		}
		if nonNegative && x < 0 {
			return nil, fmt.Errorf("%s от отрицательного числа %v", name, x)
		}
		return FloatValue(fn(x)), nil
	}
}

func roundFn(fn func(float64) float64) func(*Interpreter, []Value) (Value, error) {
	return func(_ *Interpreter, args []Value) (Value, error) {
		x, err := number(args[0])
		if err != nil {
			return nil, err
		}
		return IntValue(int64(fn(x))), nil
	}
}

func builtinRound(_ *Interpreter, args []Value) (Value, error) {
	x, err := number(args[0])
	if err != nil {
		return nil, err
	}
	if len(args) == 1 {
		return IntValue(int64(math.Round(x))), nil
	}
	digits, ok := args[1].(IntValue)
	if !ok || digits < 0 || digits > 15 {
		return nil, errors.New("знаков после запятой — целое от 0 до 15")
	}
	scale := math.Pow(10, float64(digits))
	return FloatValue(math.Round(x*scale) / scale), nil
}

func builtinAbs(_ *Interpreter, args []Value) (Value, error) {
	switch val := args[0].(type) {
	case IntValue:
		if val < 0 {
			return -val, nil
		}
		return val, nil
	case FloatValue:
		return FloatValue(math.Abs(float64(val))), nil
	}
	return nil, fmt.Errorf("нужно число, получено %s", args[0].TypeName())
}

func builtinPow(_ *Interpreter, args []Value) (Value, error) {
	x, err := number(args[0])
	if err != nil {
		return nil, err
	}
	y, err := number(args[1])
	if err != nil {
		return nil, err
	}
	result := math.Pow(x, y)
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return nil, fmt.Errorf("pow(%v, %v) не определена", x, y)
	}
	return FloatValue(result), nil
}

// extremum — min (sign -1) или max (sign 1) по числам или строкам.
func extremum(sign int) func(*Interpreter, []Value) (Value, error) {
	return func(_ *Interpreter, args []Value) (Value, error) {
		items := args
		if list, ok := args[0].(ListValue); ok && len(args) == 1 {
			items = list
		}
		if len(items) == 0 {
			return nil, errors.New("пустой список: выбирать не из чего")
		}
		best := items[0]
		for _, item := range items[1:] {
			cmp, err := compareValues(item, best)
			if err != nil {
				return nil, err
			}
			if cmp*sign > 0 {
				best = item
			}
		}
		return best, nil
	}
}

// ---------------------------------------------------------------------------
// Случайность
// ---------------------------------------------------------------------------

func builtinRand(in *Interpreter, args []Value) (Value, error) {
	switch len(args) {
	case 0:
		return FloatValue(in.rng.Float64()), nil
	case 1:
		n, ok := args[0].(IntValue)
		if !ok || n <= 0 {
			return nil, errors.New("rand(n): n — целое больше нуля")
		}
		return IntValue(in.rng.Int63n(int64(n))), nil
	}
	lo, ok1 := args[0].(IntValue)
	hi, ok2 := args[1].(IntValue)
	if !ok1 || !ok2 || hi < lo {
		return nil, errors.New("rand(a, b): целые a ≤ b")
	}
	return IntValue(int64(lo) + in.rng.Int63n(int64(hi-lo)+1)), nil
}

func builtinChoice(in *Interpreter, args []Value) (Value, error) {
	list, ok := args[0].(ListValue)
	if !ok || len(list) == 0 {
		return nil, errors.New("choice: нужен непустой список")
	}
	return list[in.rng.Intn(len(list))], nil
}

func builtinShuffle(in *Interpreter, args []Value) (Value, error) {
	list, ok := args[0].(ListValue)
	if !ok {
		return nil, fmt.Errorf("shuffle: нужен список, получено %s", args[0].TypeName())
	}
	shuffled := append(ListValue(nil), list...)
	in.rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	return shuffled, nil
}

// ---------------------------------------------------------------------------
// Время
// ---------------------------------------------------------------------------

func builtinNow(in *Interpreter, _ []Value) (Value, error) {
	return StringValue(in.clock.Format(time.RFC3339)), nil
}

// timeArg разбирает время из аргумента (RFC 3339); без аргумента — время
// запуска.
func (in *Interpreter) timeArg(args []Value, i int) (time.Time, error) {
	if i >= len(args) {
		return in.clock, nil
	}
	t, err := time.Parse(time.RFC3339, args[i].Text())
	if err != nil {
		return time.Time{}, fmt.Errorf("время %q: ожидалось как у now(), 2026-10-23T12:00:00+03:00", args[i].Text())
	}
	return t, nil
}

func builtinUnix(in *Interpreter, args []Value) (Value, error) {
	t, err := in.timeArg(args, 0)
	if err != nil {
		return nil, err
	}
	return IntValue(t.Unix()), nil
}

func builtinTimeFormat(in *Interpreter, args []Value) (Value, error) {
	t, err := in.timeArg(args, 1)
	if err != nil {
		return nil, err
	}
	text, err := strftime(t, args[0].Text())
	if err != nil {
		return nil, err
	}
	return StringValue(text), nil
}

func builtinTimeAdd(in *Interpreter, args []Value) (Value, error) {
	t, err := in.timeArg(args, 0)
	if err != nil {
		return nil, err
	}
	d, err := durationArg(args[1])
	if err != nil {
		return nil, err
	}
	return StringValue(t.Add(d).Format(time.RFC3339)), nil
}

// durationArg — длительность: число секунд или строка вида 1h30m.
func durationArg(v Value) (time.Duration, error) {
	switch val := v.(type) {
	case IntValue:
		return time.Duration(val) * time.Second, nil
	case FloatValue:
		return time.Duration(float64(val) * float64(time.Second)), nil
	case StringValue:
		d, err := time.ParseDuration(strings.TrimSpace(string(val)))
		if err != nil {
			return 0, fmt.Errorf("длительность %q: число секунд или строка вида 500ms, 2s, 1h30m", string(val))
		}
		return d, nil
	}
	return 0, fmt.Errorf("длительность — число секунд или строка, получено %s", v.TypeName())
}

// maxSleep — пауза дольше часа в сценарии — почти наверняка ошибка единиц.
const maxSleep = time.Hour

func builtinSleep(in *Interpreter, args []Value) (Value, error) {
	d, err := durationArg(args[0])
	if err != nil {
		return nil, err
	}
	if d < 0 || d > maxSleep {
		return nil, fmt.Errorf("пауза %s: от 0 до %s", d, maxSleep)
	}
	// План не ждёт: пауза ничего не меняет, а план строится сразу.
	if in.dry == nil && d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-in.ctx.Done():
			return nil, in.ctx.Err()
		}
	}
	return StringValue(d.String()), nil
}

// strftime — образец времени: %Y %m %d %H %M %S %y %j %F %T %z %Z %%.
func strftime(t time.Time, layout string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(layout); i++ {
		if layout[i] != '%' {
			b.WriteByte(layout[i])
			continue
		}
		i++
		if i >= len(layout) {
			return "", errors.New("образец времени обрывается на %")
		}
		switch layout[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'F':
			b.WriteString(t.Format("2006-01-02"))
		case 'T':
			b.WriteString(t.Format("15:04:05"))
		case 'z':
			b.WriteString(t.Format("-0700"))
		case 'Z':
			b.WriteString(t.Format("MST"))
		case '%':
			b.WriteByte('%')
		default:
			return "", fmt.Errorf("неизвестный знак %%%c: доступны %%Y %%m %%d %%H %%M %%S %%y %%j %%F %%T %%z %%Z", layout[i])
		}
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// Сериализация
// ---------------------------------------------------------------------------

// plain переводит значение в обычные Go-значения для кодировщиков.
func plain(v Value) (any, error) {
	switch val := v.(type) {
	case NullValue:
		return nil, nil
	case BoolValue:
		return bool(val), nil
	case IntValue:
		return int64(val), nil
	case FloatValue:
		return float64(val), nil
	case StringValue:
		return string(val), nil
	case ListValue:
		items := make([]any, len(val))
		for i, item := range val {
			p, err := plain(item)
			if err != nil {
				return nil, err
			}
			items[i] = p
		}
		return items, nil
	case MapValue:
		out := make(map[string]any, len(val))
		for key, item := range val {
			p, err := plain(item)
			if err != nil {
				return nil, err
			}
			out[key] = p
		}
		return out, nil
	case ConfigValue:
		out := make(map[string]any)
		for section, params := range val.Data {
			target := out
			if section != "" {
				inner := make(map[string]any, len(params))
				out[section] = inner
				target = inner
			}
			for key, value := range params {
				target[key] = value
			}
		}
		return out, nil
	}
	if p, ok := v.(interface{ plain() (any, error) }); ok {
		return p.plain()
	}
	return nil, fmt.Errorf("значение типа %s не сериализуется", v.TypeName())
}

// fromPlain — обратно: объект — словарь, массив — список, число — целое,
// если оно целое.
func fromPlain(v any) Value {
	switch val := v.(type) {
	case nil:
		return NullValue{}
	case bool:
		return BoolValue(val)
	case string:
		return StringValue(val)
	case json.Number:
		if n, err := val.Int64(); err == nil {
			return IntValue(n)
		}
		f, _ := val.Float64()
		return FloatValue(f)
	case int:
		return IntValue(val)
	case int64:
		return IntValue(val)
	case uint64:
		return IntValue(int64(val))
	case float64:
		if val == math.Trunc(val) && math.Abs(val) < 1<<53 {
			return IntValue(int64(val))
		}
		return FloatValue(val)
	case time.Time:
		return StringValue(val.Format(time.RFC3339))
	case []any:
		list := make(ListValue, len(val))
		for i, item := range val {
			list[i] = fromPlain(item)
		}
		return list
	case map[string]any:
		m := make(MapValue, len(val))
		for key, item := range val {
			m[key] = fromPlain(item)
		}
		return m
	case map[any]any:
		m := make(MapValue, len(val))
		for key, item := range val {
			m[fmt.Sprint(key)] = fromPlain(item)
		}
		return m
	}
	return StringValue(fmt.Sprint(v))
}

func builtinToJSON(_ *Interpreter, args []Value) (Value, error) {
	p, err := plain(args[0])
	if err != nil {
		return nil, err
	}
	var data []byte
	if len(args) == 2 && args[1].Truthy() {
		data, err = json.MarshalIndent(p, "", "  ")
	} else {
		data, err = json.Marshal(p)
	}
	if err != nil {
		return nil, err
	}
	return StringValue(data), nil
}

func builtinFromJSON(_ *Interpreter, args []Value) (Value, error) {
	decoder := json.NewDecoder(strings.NewReader(args[0].Text()))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return nil, fmt.Errorf("JSON не разбирается: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("JSON не разбирается: после значения лишний текст")
	}
	return fromPlain(v), nil
}

func builtinToYAML(_ *Interpreter, args []Value) (Value, error) {
	p, err := plain(args[0])
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(p)
	if err != nil {
		return nil, err
	}
	return StringValue(data), nil
}

func builtinFromYAML(_ *Interpreter, args []Value) (Value, error) {
	var v any
	if err := yaml.Unmarshal([]byte(args[0].Text()), &v); err != nil {
		return nil, fmt.Errorf("YAML не разбирается: %w", err)
	}
	return fromPlain(v), nil
}

func builtinToTOML(_ *Interpreter, args []Value) (Value, error) {
	p, err := plain(args[0])
	if err != nil {
		return nil, err
	}
	if _, ok := p.(map[string]any); !ok {
		return nil, fmt.Errorf("TOML — это таблица: нужен словарь, получено %s", args[0].TypeName())
	}
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(p); err != nil {
		return nil, err
	}
	return StringValue(b.String()), nil
}

func builtinFromTOML(_ *Interpreter, args []Value) (Value, error) {
	var v map[string]any
	if err := toml.Unmarshal([]byte(args[0].Text()), &v); err != nil {
		return nil, fmt.Errorf("TOML не разбирается: %w", err)
	}
	return fromPlain(v), nil
}

// ---------------------------------------------------------------------------
// eval
// ---------------------------------------------------------------------------

// builtinEval вычисляет одно выражение языка. Проверка до запуска текст
// внутри строки не видит, поэтому выражение проверяется здесь, перед
// вычислением, той же проверкой. Песочница, режим плана и действия — те же,
// что у сценария: eval — не лазейка.
//
// Алиасы в eval недоступны: заявка запоминает алиасы, от которых зависит
// сценарий, по его тексту, а вызов из строки в этот список не попал бы.
func builtinEval(in *Interpreter, args []Value) (Value, error) {
	text, ok := args[0].(StringValue)
	if !ok {
		return nil, fmt.Errorf("eval: нужна строка, получено %s", args[0].TypeName())
	}
	file, err := Parse("<eval>", string(text))
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	if len(file.Aliases) > 0 || len(file.Stmts) != 1 {
		return nil, errors.New("eval вычисляет одно выражение, без объявлений и инструкций")
	}
	stmt, ok := file.Stmts[0].(*ExprStmt)
	if !ok {
		return nil, errors.New("eval вычисляет одно выражение, без объявлений и инструкций")
	}
	if problems := Check(file, NewRegistry()); len(problems) > 0 {
		return nil, fmt.Errorf("eval: %s", problems[0])
	}
	if len(in.stack) >= maxCallDepth {
		return nil, errors.New("слишком глубокая вложенность eval")
	}
	in.stack = append(in.stack, "eval")
	defer func() { in.stack = in.stack[:len(in.stack)-1] }()
	// Пустой реестр и при вычислении: алиас с именем встроенной функции
	// её перекрыл бы.
	saved := in.registry
	in.registry = NewRegistry()
	defer func() { in.registry = saved }()

	value, err := in.eval(stmt.Expr, newScope(nil))
	if err != nil {
		return nil, err
	}
	if _, ok := value.(*FuncValue); ok {
		return nil, errors.New("eval возвращает значение, а не функцию")
	}
	return value, nil
}

// ---------------------------------------------------------------------------
// HTTP: только чтение
// ---------------------------------------------------------------------------

// maxFetch — предел ответа: сценарию нужны документы и JSON, а не образы.
const maxFetch = 10 << 20

// maxFetchTimeout — дольше ждать ответа сценарию незачем.
const maxFetchTimeout = 5 * time.Minute

type httpRequest struct {
	method  string
	url     *url.URL
	headers map[string]string
	timeout time.Duration
}

func (in *Interpreter) httpRequest(args []Value) (*httpRequest, error) {
	u, err := url.Parse(strings.TrimSpace(args[0].Text()))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("адрес %q: нужен http:// или https://", args[0].Text())
	}
	req := &httpRequest{method: http.MethodGet, url: u, headers: map[string]string{}, timeout: 30 * time.Second}
	if len(args) < 2 {
		return req, nil
	}
	options, ok := args[1].(MapValue)
	if !ok {
		return nil, fmt.Errorf("параметры запроса — словарь, получено %s", args[1].TypeName())
	}
	for _, key := range options.SortedKeys() {
		value := options[key]
		switch key {
		case "method":
			req.method = strings.ToUpper(value.Text())
			if req.method != http.MethodGet && req.method != http.MethodHead {
				return nil, fmt.Errorf("метод %s: сценарий только читает (GET, HEAD); "+
					"менять внешние системы — дело исполнителя и заявки", req.method)
			}
		case "headers":
			headers, ok := value.(MapValue)
			if !ok {
				return nil, errors.New("headers — словарь")
			}
			for name, v := range headers {
				req.headers[name] = v.Text()
			}
		case "timeout":
			d, err := durationArg(value)
			if err != nil {
				return nil, err
			}
			if d <= 0 || d > maxFetchTimeout {
				return nil, fmt.Errorf("timeout %s: от 0 до %s", d, maxFetchTimeout)
			}
			req.timeout = d
		default:
			return nil, fmt.Errorf("неизвестный параметр запроса %q: method, headers, timeout", key)
		}
	}
	return req, nil
}

// do выполняет запрос и записывает обращение в действия: план показывает,
// что сценарий ходит в сеть, как и к живому источнику.
func (in *Interpreter) do(req *httpRequest) (*http.Response, []byte, error) {
	if in.sandbox != nil {
		if err := in.sandbox.checkNet(req.url); err != nil {
			return nil, nil, err
		}
	}
	ctx := in.ctx
	client := &http.Client{
		Timeout: req.timeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("слишком много перенаправлений")
			}
			if in.sandbox != nil {
				return in.sandbox.checkNet(next.URL)
			}
			return nil
		},
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, req.url.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("User-Agent", "ochan")
	for name, value := range req.headers {
		httpReq.Header.Set(name, value)
	}

	shown := *req.url
	shown.User = nil
	in.effects = append(in.effects, Effect{Kind: EffectLive, Path: req.method + " " + shown.String(), Calls: in.activeCalls()})

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", req.method, shown.String(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetch+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxFetch {
		return nil, nil, fmt.Errorf("ответ %s больше %d МиБ", shown.String(), maxFetch>>20)
	}
	return resp, body, nil
}

func builtinFetch(in *Interpreter, args []Value) (Value, error) {
	req, err := in.httpRequest(args)
	if err != nil {
		return nil, err
	}
	resp, body, err := in.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s ответил %s", req.url.Host, resp.Status)
	}
	return StringValue(body), nil
}

func builtinCurl(in *Interpreter, args []Value) (Value, error) {
	req, err := in.httpRequest(args)
	if err != nil {
		return nil, err
	}
	resp, body, err := in.do(req)
	if err != nil {
		return nil, err
	}
	headers := make(MapValue, len(resp.Header))
	names := make([]string, 0, len(resp.Header))
	for name := range resp.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		headers[strings.ToLower(name)] = StringValue(resp.Header.Get(name))
	}
	return MapValue{
		"status":  IntValue(resp.StatusCode),
		"headers": headers,
		"body":    StringValue(body),
	}, nil
}
