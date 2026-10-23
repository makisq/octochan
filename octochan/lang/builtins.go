package lang

import (
	"fmt"
	"strings"

	"octochan/config"
)

// Builtin — встроенная функция языка.
//
// Все они — тонкие обёртки над core, тем же пакетом, на который опираются
// команды CLI. Благодаря этому diff в языке и diff в командной строке
// считают расхождения одинаково.
type Builtin struct {
	Name string
	// MinArgs и MaxArgs задают арность; MaxArgs = -1 означает «сколько угодно».
	MinArgs int
	MaxArgs int
	Doc     string
	// Returns — тип результата, если он не зависит от аргументов. Нужен
	// проверке до выполнения; пустое значение — «узнаем при выполнении».
	Returns TypeName
	Fn      func(in *Interpreter, args []Value) (Value, error)
}

func (b Builtin) checkArity(n int) error {
	if n < b.MinArgs {
		return fmt.Errorf("нужно минимум %d %s, передано %d",
			b.MinArgs, plural(b.MinArgs, "аргумент", "аргумента", "аргументов"), n)
	}
	if b.MaxArgs >= 0 && n > b.MaxArgs {
		return fmt.Errorf("принимает не более %d %s, передано %d",
			b.MaxArgs, plural(b.MaxArgs, "аргумента", "аргументов", "аргументов"), n)
	}
	return nil
}

// Signature возвращает описание вызова для справки.
func (b Builtin) Signature() string {
	switch {
	case b.MaxArgs < 0:
		return fmt.Sprintf("%s(...)", b.Name)
	case b.MinArgs == b.MaxArgs:
		return fmt.Sprintf("%s/%d", b.Name, b.MinArgs)
	default:
		return fmt.Sprintf("%s/%d..%d", b.Name, b.MinArgs, b.MaxArgs)
	}
}

// DefaultBuiltins возвращает набор встроенных функций.
func DefaultBuiltins() map[string]Builtin {
	list := []Builtin{
		{
			Name: "diff", MinArgs: 2, MaxArgs: 2, Returns: TypeInt,
			Doc: "количество расхождений между двумя конфигами",
			Fn:  builtinDiff,
		},
		{
			Name: "diff_list", MinArgs: 2, MaxArgs: 2, Returns: TypeList,
			Doc: "список имён расходящихся параметров",
			Fn:  builtinDiffList,
		},
		{
			Name: "read", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "содержимое файла",
			Fn:  builtinRead,
		},
		{
			Name: "write", MinArgs: 2, MaxArgs: 2, Returns: TypeInt,
			Doc: "записать текст в файл",
			Fn:  builtinWrite,
		},
		{
			Name: "exists", MinArgs: 1, MaxArgs: 1, Returns: TypeBool,
			Doc: "существует ли файл",
			Fn:  builtinExists,
		},
		{
			Name: "param", MinArgs: 2, MaxArgs: 3, Returns: TypeString,
			Doc: "значение параметра конфига: param(файл, имя) или param(файл, секция, имя)",
			Fn:  builtinParam,
		},
		{
			Name: "params", MinArgs: 1, MaxArgs: 1, Returns: TypeInt,
			Doc: "количество параметров в конфиге",
			Fn:  builtinParams,
		},
		{
			Name: "validate", MinArgs: 1, MaxArgs: 1, Returns: TypeInt,
			Doc: "проверить конфиг, вернуть число параметров",
			Fn:  builtinValidate,
		},
		{
			Name: "print", MinArgs: 0, MaxArgs: -1, Returns: TypeString,
			Doc: "напечатать значения; первым аргументом можно передать поток: print(file(\"отчёт.txt\"), …)",
			Fn:  builtinPrint,
		},
		{
			Name: "stdout", MinArgs: 0, MaxArgs: 0, Returns: TypeStream,
			Doc: "поток вывода на экран — туда печатает print по умолчанию",
			Fn:  func(*Interpreter, []Value) (Value, error) { return StreamValue{Kind: StreamStdout}, nil },
		},
		{
			Name: "stderr", MinArgs: 0, MaxArgs: 0, Returns: TypeStream,
			Doc: "поток ошибок",
			Fn:  func(*Interpreter, []Value) (Value, error) { return StreamValue{Kind: StreamStderr}, nil },
		},
		{
			Name: "file", MinArgs: 1, MaxArgs: 1, Returns: TypeStream,
			Doc: "поток в файл: print дописывает в него строку; запись — как write, видна в плане",
			Fn:  builtinFile,
		},
		{
			Name: "fail", MinArgs: 0, MaxArgs: -1,
			Doc: "прервать сценарий с сообщением",
			Fn:  builtinFail,
		},
		{
			Name: "len", MinArgs: 1, MaxArgs: 1, Returns: TypeInt,
			Doc: "длина списка или строки",
			Fn:  builtinLen,
		},
		{
			Name: "lines", MinArgs: 1, MaxArgs: 1, Returns: TypeList,
			Doc: "разбить текст на список строк",
			Fn:  builtinLines,
		},
		{
			Name: "join", MinArgs: 1, MaxArgs: 2, Returns: TypeString,
			Doc: "склеить список в строку",
			Fn:  builtinJoin,
		},
		{
			Name: "str", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "привести к строке",
			Fn:  builtinStr,
		},
		{
			Name: "int", MinArgs: 1, MaxArgs: 1, Returns: TypeInt,
			Doc: "привести к целому",
			Fn:  builtinInt,
		},
		{
			Name: "list", MinArgs: 0, MaxArgs: -1, Returns: TypeList,
			Doc: "собрать список из аргументов",
			Fn:  builtinList,
		},
		{
			Name: "env", MinArgs: 1, MaxArgs: 2,
			Doc: "значение переменной окружения",
			Fn:  builtinEnv,
		},
		{
			Name: "normalize", MinArgs: 1, MaxArgs: 1, Returns: TypeString,
			Doc: "каноническая форма значения конфига: normalize(\"8GB\") == normalize(\"8192MB\")",
			Fn:  builtinNormalize,
		},
		{
			Name: "config", MinArgs: 1, MaxArgs: 2, Returns: TypeConfig,
			Doc: "разобрать источник один раз: файл, текст или postgres://user@host/db",
			Fn:  builtinConfig,
		},
		{
			Name: "map", MinArgs: 0, MaxArgs: -1, Returns: TypeMap,
			Doc: "собрать словарь из пар ключ, значение",
			Fn:  builtinMap,
		},
		{
			Name: "keys", MinArgs: 1, MaxArgs: 1, Returns: TypeList,
			Doc: "ключи словаря или имена параметров конфига",
			Fn:  builtinKeys,
		},
		{
			Name: "values", MinArgs: 1, MaxArgs: 1, Returns: TypeList,
			Doc: "значения словаря или конфига",
			Fn:  builtinValues,
		},
		{
			Name: "has", MinArgs: 2, MaxArgs: 2, Returns: TypeBool,
			Doc: "есть ли ключ в словаре или параметр в конфиге",
			Fn:  builtinHas,
		},
		{
			Name: "get", MinArgs: 2, MaxArgs: 3,
			Doc: "значение по ключу либо значение по умолчанию",
			Fn:  builtinGet,
		},
		{
			Name: "put", MinArgs: 3, MaxArgs: 3,
			Doc: "записать значение по ключу и вернуть контейнер",
			Fn:  builtinPut,
		},
		{
			Name: "merge", MinArgs: 2, MaxArgs: 2,
			Doc: "объединить два словаря или конфига; правый перекрывает левый",
			Fn:  builtinMerge,
		},
		{
			Name: "save", MinArgs: 2, MaxArgs: 2, Returns: TypeInt,
			Doc: "записать конфиг в файл",
			Fn:  builtinSave,
		},
	}

	builtins := make(map[string]Builtin, len(list))
	for _, b := range list {
		builtins[b.Name] = b
	}
	return builtins
}

// ---------------------------------------------------------------------------
// Работа с конфигами
// ---------------------------------------------------------------------------

// configOf принимает уже разобранный конфиг, путь к файлу или текст.
//
// Готовый ConfigValue проходит без повторного разбора — ради этого он
// и заведён: сценарий из трёх обращений к одному файлу раньше парсил его
// трижды.
func (in *Interpreter) configOf(v Value) (ConfigValue, error) {
	return in.configWith(v, config.SourceOptions{})
}

func (in *Interpreter) configWith(v Value, opts config.SourceOptions) (ConfigValue, error) {
	if cfg, ok := v.(ConfigValue); ok {
		return cfg, nil
	}

	s, ok := v.(StringValue)
	if !ok {
		return ConfigValue{}, fmt.Errorf("ожидался конфиг, путь к файлу или текст, получено %s", v.TypeName())
	}

	// Ссылка на живой источник: postgres://user@host:5432/db и что угодно
	// другое, что зарегистрирует свою схему.
	if scheme := config.SourceScheme(string(s)); scheme != "" {
		loaded, err := in.loadLive(string(s), scheme, opts)
		if err != nil {
			return ConfigValue{}, err
		}
		return ConfigValue{Name: loaded.Name, Data: loaded.Data, Notes: loaded.Notes}, nil
	}

	text, name, err := in.textOf(v)
	if err != nil {
		return ConfigValue{}, err
	}

	data, err := config.ParseConfig(text)
	if err != nil {
		return ConfigValue{}, fmt.Errorf("%s: %w", name, err)
	}
	return ConfigValue{Name: name, Data: data}, nil
}

// textOf возвращает текст и человекочитаемое имя источника.
func (in *Interpreter) textOf(v Value) (string, string, error) {
	s, ok := v.(StringValue)
	if !ok {
		return "", "", fmt.Errorf("ожидался путь к файлу или текст, получено %s", v.TypeName())
	}

	path := string(s)

	// Многострочное значение считаем содержимым, а не путём.
	if strings.ContainsAny(path, "\n") {
		return path, "текст", nil
	}

	data, err := in.readFile(path)
	if err != nil {
		return "", "", fmt.Errorf("не удалось прочитать %s: %w", path, err)
	}
	return string(data), path, nil
}

// builtinConfig принимает необязательный второй аргумент "all", чтобы
// у живого сервера читать и параметры со значением по умолчанию.
func builtinConfig(in *Interpreter, args []Value) (Value, error) {
	var opts config.SourceOptions
	if len(args) == 2 {
		mode := strings.ToLower(strings.TrimSpace(args[1].Text()))
		switch mode {
		case "all", "все", "всё":
			opts.IncludeDefaults = true
		case "", "set", "заданные":
		default:
			return nil, fmt.Errorf("неизвестный режим %q; допустимо \"all\"", args[1].Text())
		}
	}

	cfg, err := in.configWith(args[0], opts)
	if err != nil {
		return nil, err
	}

	// Замечания источника показываем сразу: параметр, ждущий перезапуска,
	// меняет смысл всего сравнения.
	for _, note := range cfg.Notes {
		fmt.Fprintf(in.Out(), "внимание: %s\n", note)
	}
	return cfg, nil
}

func builtinDiff(in *Interpreter, args []Value) (Value, error) {
	left, right, err := in.twoConfigs(args)
	if err != nil {
		return nil, err
	}
	return IntValue(config.CompareConfigs(left.Data, right.Data, left.Name, right.Name).Len()), nil
}

func builtinDiffList(in *Interpreter, args []Value) (Value, error) {
	left, right, err := in.twoConfigs(args)
	if err != nil {
		return nil, err
	}

	diff := config.CompareConfigs(left.Data, right.Data, left.Name, right.Name)
	names := make(ListValue, 0, diff.Len())
	for _, entry := range diff.Entries {
		names = append(names, StringValue(entry.Param()))
	}
	return names, nil
}

func (in *Interpreter) twoConfigs(args []Value) (ConfigValue, ConfigValue, error) {
	left, err := in.configOf(args[0])
	if err != nil {
		return ConfigValue{}, ConfigValue{}, err
	}
	right, err := in.configOf(args[1])
	if err != nil {
		return ConfigValue{}, ConfigValue{}, err
	}
	return left, right, nil
}

func builtinParam(in *Interpreter, args []Value) (Value, error) {
	cfg, err := in.configOf(args[0])
	if err != nil {
		return nil, err
	}

	name := args[1].Text()
	if len(args) == 3 {
		name = args[1].Text() + "." + args[2].Text()
	}

	value, err := cfg.Lookup(name)
	if err != nil {
		return nil, err
	}
	return StringValue(value), nil
}

func builtinParams(in *Interpreter, args []Value) (Value, error) {
	cfg, err := in.configOf(args[0])
	if err != nil {
		return nil, err
	}
	return IntValue(cfg.Params()), nil
}

func builtinValidate(in *Interpreter, args []Value) (Value, error) {
	cfg, err := in.configOf(args[0])
	if err != nil {
		return nil, err
	}
	if cfg.Params() == 0 {
		return nil, fmt.Errorf("%s: не найдено ни одного параметра", cfg.Name)
	}
	return IntValue(cfg.Params()), nil
}

func builtinNormalize(_ *Interpreter, args []Value) (Value, error) {
	return StringValue(config.NormalizeValue(args[0].Text())), nil
}

// ---------------------------------------------------------------------------
// Словари и конфиги
// ---------------------------------------------------------------------------

func builtinMap(_ *Interpreter, args []Value) (Value, error) {
	if len(args)%2 != 0 {
		return nil, fmt.Errorf("map ожидает пары ключ, значение — получено нечётное число аргументов (%d)", len(args))
	}

	entries := make(MapValue, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		entries[args[i].Text()] = args[i+1]
	}
	return entries, nil
}

func builtinKeys(_ *Interpreter, args []Value) (Value, error) {
	switch container := args[0].(type) {
	case MapValue:
		return stringList(container.SortedKeys()), nil
	case ConfigValue:
		return stringList(container.SortedKeys()), nil
	default:
		return nil, fmt.Errorf("keys применим к словарю или конфигу, получено %s", args[0].TypeName())
	}
}

func builtinValues(_ *Interpreter, args []Value) (Value, error) {
	switch container := args[0].(type) {
	case MapValue:
		keys := container.SortedKeys()
		values := make(ListValue, len(keys))
		for i, key := range keys {
			values[i] = container[key]
		}
		return values, nil

	case ConfigValue:
		keys := container.SortedKeys()
		values := make(ListValue, 0, len(keys))
		for _, key := range keys {
			value, err := container.Lookup(key)
			if err != nil {
				return nil, err
			}
			values = append(values, StringValue(value))
		}
		return values, nil

	default:
		return nil, fmt.Errorf("values применим к словарю или конфигу, получено %s", args[0].TypeName())
	}
}

func builtinHas(_ *Interpreter, args []Value) (Value, error) {
	key := args[1].Text()

	switch container := args[0].(type) {
	case MapValue:
		_, ok := container[key]
		return BoolValue(ok), nil
	case ConfigValue:
		_, err := container.Lookup(key)
		return BoolValue(err == nil), nil
	default:
		return nil, fmt.Errorf("has применим к словарю или конфигу, получено %s", args[0].TypeName())
	}
}

func builtinGet(_ *Interpreter, args []Value) (Value, error) {
	var fallback Value = NullValue{}
	if len(args) == 3 {
		fallback = args[2]
	}

	key := args[1].Text()

	switch container := args[0].(type) {
	case MapValue:
		if value, ok := container[key]; ok {
			return value, nil
		}
		return fallback, nil

	case ConfigValue:
		if value, err := container.Lookup(key); err == nil {
			return StringValue(value), nil
		}
		return fallback, nil

	case ListValue:
		n, ok := numeric(args[1])
		if !ok {
			return nil, fmt.Errorf("для списка нужен числовой индекс, получено %s", args[1].TypeName())
		}
		index := int(n)
		if index < 0 {
			index += len(container)
		}
		if index < 0 || index >= len(container) {
			return fallback, nil
		}
		return container[index], nil

	default:
		return nil, fmt.Errorf("get применим к словарю, конфигу или списку, получено %s", args[0].TypeName())
	}
}

// builtinPut изменяет контейнер на месте и возвращает его же.
// Словарь и конфиг — ссылочные значения, копия здесь только запутала бы:
// половина изменений уходила бы в никуда.
func builtinPut(_ *Interpreter, args []Value) (Value, error) {
	key := args[1].Text()

	switch container := args[0].(type) {
	case MapValue:
		container[key] = args[2]
		return container, nil
	case ConfigValue:
		container.Set(key, args[2].Text())
		return container, nil
	default:
		return nil, fmt.Errorf("put применим к словарю или конфигу, получено %s", args[0].TypeName())
	}
}

func builtinMerge(in *Interpreter, args []Value) (Value, error) {
	switch left := args[0].(type) {
	case MapValue:
		right, ok := args[1].(MapValue)
		if !ok {
			return nil, fmt.Errorf("merge ожидает два словаря, справа %s", args[1].TypeName())
		}
		// Результат новый: merge не должен незаметно менять исходные данные.
		merged := make(MapValue, len(left)+len(right))
		for key, value := range left {
			merged[key] = value
		}
		for key, value := range right {
			merged[key] = value
		}
		return merged, nil

	case ConfigValue:
		right, err := in.configOf(args[1])
		if err != nil {
			return nil, err
		}

		merged := ConfigValue{
			Name: left.Name + "+" + right.Name,
			Data: make(config.ConfigMap, len(left.Data)),
		}
		for _, source := range []ConfigValue{left, right} {
			for section, params := range source.Data {
				if _, ok := merged.Data[section]; !ok {
					merged.Data[section] = make(map[string]string, len(params))
				}
				for key, value := range params {
					merged.Data[section][key] = value
				}
			}
		}
		return merged, nil

	default:
		return nil, fmt.Errorf("merge применим к словарям или конфигам, получено %s", args[0].TypeName())
	}
}

func builtinSave(in *Interpreter, args []Value) (Value, error) {
	cfg, err := in.configOf(args[0])
	if err != nil {
		return nil, err
	}

	path := args[1].Text()
	if err := in.writeFile(EffectSave, path, []byte(config.FormatConfig(cfg.Data))); err != nil {
		return nil, fmt.Errorf("не удалось записать конфиг %s: %w", path, err)
	}
	return IntValue(cfg.Params()), nil
}

func stringList(items []string) ListValue {
	values := make(ListValue, len(items))
	for i, item := range items {
		values[i] = StringValue(item)
	}
	return values
}

// ---------------------------------------------------------------------------
// Файлы
// ---------------------------------------------------------------------------

func builtinRead(in *Interpreter, args []Value) (Value, error) {
	data, err := in.readFile(args[0].Text())
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать %s: %w", args[0].Text(), err)
	}
	return StringValue(data), nil
}

func builtinWrite(in *Interpreter, args []Value) (Value, error) {
	path := args[0].Text()
	content := args[1].Text()

	if err := in.writeFile(EffectWrite, path, []byte(content)); err != nil {
		return nil, fmt.Errorf("не удалось записать %s: %w", path, err)
	}
	return IntValue(len(content)), nil
}

func builtinExists(in *Interpreter, args []Value) (Value, error) {
	exists, err := in.fileExists(args[0].Text())
	if err != nil {
		return nil, err
	}
	return BoolValue(exists), nil
}

// ---------------------------------------------------------------------------
// Общего назначения
// ---------------------------------------------------------------------------

// builtinPrint печатает значения через пробел и переводит строку.
//
// Если первый аргумент — поток, печать идёт в него: на экран, в поток
// ошибок или в конец файла. Запись в файл — действие сценария, как
// write: в режиме плана она только попадает в план.
func builtinPrint(in *Interpreter, args []Value) (Value, error) {
	stream := StreamValue{Kind: StreamStdout}
	if len(args) > 0 {
		if s, ok := args[0].(StreamValue); ok {
			stream, args = s, args[1:]
		}
	}

	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = Display(arg)
	}
	line := strings.Join(parts, " ")

	switch stream.Kind {
	case StreamStderr:
		fmt.Fprintln(in.errOut, line)
	case StreamFile:
		if err := in.appendFile(stream.Path, []byte(line+"\n")); err != nil {
			return nil, fmt.Errorf("не удалось дописать %s: %w", stream.Path, err)
		}
	default:
		fmt.Fprintln(in.Out(), line)
	}
	return StringValue(line), nil
}

// builtinFile открывает поток в файл. Путь проверяется сразу: поток за
// пределы песочницы отвергается там, где его создали, а не при печати.
func builtinFile(in *Interpreter, args []Value) (Value, error) {
	s, ok := args[0].(StringValue)
	if !ok {
		return nil, fmt.Errorf("file: ожидался путь строкой, получено %s", args[0].TypeName())
	}
	if _, err := in.path(string(s)); err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}
	return StreamValue{Kind: StreamFile, Path: string(s)}, nil
}

func builtinFail(_ *Interpreter, args []Value) (Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("сценарий прерван")
	}

	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = Display(arg)
	}
	return nil, fmt.Errorf("%s", strings.Join(parts, " "))
}

func builtinLen(_ *Interpreter, args []Value) (Value, error) {
	switch v := args[0].(type) {
	case ListValue:
		return IntValue(len(v)), nil
	case MapValue:
		return IntValue(len(v)), nil
	case ConfigValue:
		return IntValue(v.Params()), nil
	case StringValue:
		return IntValue(len([]rune(v))), nil
	case NullValue:
		return IntValue(0), nil
	default:
		return nil, fmt.Errorf("len неприменим к %s", args[0].TypeName())
	}
}

func builtinLines(_ *Interpreter, args []Value) (Value, error) {
	text := args[0].Text()
	if strings.TrimSpace(text) == "" {
		return ListValue{}, nil
	}

	raw := strings.Split(strings.TrimRight(text, "\n"), "\n")
	lines := make(ListValue, len(raw))
	for i, line := range raw {
		lines[i] = StringValue(strings.TrimRight(line, "\r"))
	}
	return lines, nil
}

func builtinJoin(_ *Interpreter, args []Value) (Value, error) {
	list, ok := args[0].(ListValue)
	if !ok {
		return nil, fmt.Errorf("join ожидает список, получено %s", args[0].TypeName())
	}

	sep := ", "
	if len(args) == 2 {
		sep = args[1].Text()
	}

	parts := make([]string, len(list))
	for i, item := range list {
		parts[i] = item.Text()
	}
	return StringValue(strings.Join(parts, sep)), nil
}

func builtinStr(_ *Interpreter, args []Value) (Value, error) {
	return StringValue(args[0].Text()), nil
}

func builtinInt(_ *Interpreter, args []Value) (Value, error) {
	if n, ok := numeric(args[0]); ok {
		return IntValue(int64(n)), nil
	}
	return nil, fmt.Errorf("не удалось привести %s %q к целому",
		args[0].TypeName(), args[0].Text())
}

func builtinList(_ *Interpreter, args []Value) (Value, error) {
	items := make(ListValue, len(args))
	copy(items, args)
	return items, nil
}

func builtinEnv(in *Interpreter, args []Value) (Value, error) {
	value, ok, err := in.lookupEnv(args[0].Text())
	if err != nil {
		return nil, err
	}
	if ok {
		return StringValue(value), nil
	}
	if len(args) == 2 {
		return args[1], nil
	}
	return StringValue(""), nil
}
