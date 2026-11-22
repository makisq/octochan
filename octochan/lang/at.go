package lang

// Общий множитель: f @ (a | b | c).
//
// Как a·(b + c) = a·b + a·c, вызов f @ (b | c) — это f(b), затем f(c).
// Вложенная скобка — тоже множитель, и она приоритетнее: сначала
// выполняется она, потом внешняя функция — над её операндами:
//
//	a @ (a @ (b | c) | d)   →   a(b), a(c), a(b), a(c), a(d)
//
// Вызовов столько, сколько написано: a² над (b + c) — это по два вызова на
// каждый из двух операндов, и решать, нужно ли админу четыре вызова, — не
// дело языка. Скобка без функции — просто группа: a @ ((b | c) | d) —
// a(b), a(c), a(d). Вложенность — любая.
//
// Общие строки. Операнд — плейбук (файл .yaml, .yml, .json или словарь) —
// может ждать значение, которого у пишущего нет: string_i: ?. Значение
// придёт в ответе на другой вызов того же выражения. ochan собирает,
// какие общие строки нужны операндам, и после каждого вызова ищет их в
// ответе: в словаре — по ключу (и в теле body, если это JSON), в тексте —
// строкой «ключ: значение» или «"ключ": "значение"». Вызов, которому строки
// ещё не хватает, ждёт: он выполнится сразу, как только она найдётся, даже
// если между ними ещё n вызовов. Порядок остальных вызовов не меняется.
// Строка, которой не нашлось ни в одном ответе, — ошибка с её именем.
//
// Плейбук-файл с заполненной строкой — копия рядом с ним: c.yaml →
// c.at.yaml (запись — действие сценария, её видно в плане); сам c.yaml не
// меняется. Словарь заполняется копией в памяти.
//
// Функция слева — алиас, встроенная функция, функция-значение или команда
// ochan: approve @ ("1cc5ae2d97" | "2ab3c4d5e6") согласует первую заявку,
// затем вторую, по очереди. Команда модуля сама называет свои общие строки
// аннотацией shared («string_i» или «параметр=ключ_ответа», через
// запятую): тогда ? в плейбуке ставить не нужно — ochan поставит его сам.
//
// Значение выражения — список ответов в порядке вызовов.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AtExpr — f @ (операнды). Fn пусто у простой группы (a | b) внутри
// другой: она только объединяет операнды.
type AtExpr struct {
	Pos   Position
	Fn    Expr
	Items []Expr
}

func (e *AtExpr) Position() Position { return e.Pos }
func (*AtExpr) exprNode()            {}

// SharedAnnotation — аннотация команды модуля с её общими строками:
// cmd.Annotations = map[string]string{"shared": "node_tune:string_i"}.
const SharedAnnotation = "shared"

// sharedPlaceholder — значение общей строки, которую ждут: string_i: ?.
const sharedPlaceholder = "?"

// SharedKey — общая строка, которую объявила команда модуля: Param — куда
// в плейбуке её поставить, From — какой ключ искать в ответе. Service —
// для плейбуков какого сервиса: ? ставится в Param, даже если его в
// плейбуке нет. Без сервиса — в плейбуки, где Param уже есть.
type SharedKey struct {
	Service string
	Param   string
	From    string
}

// SharedCommands — команды ochan, которые называют свои общие строки
// (аннотация shared у команды модуля).
type SharedCommands interface {
	Shared(argv []string) []SharedKey
}

// ParseShared разбирает аннотацию shared: «[сервис:]параметр[=ключ_ответа]»
// через запятую: «string_i», «node_tune:token=session.token».
func ParseShared(text string) []SharedKey {
	var keys []SharedKey
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		service := ""
		if s, rest, found := strings.Cut(part, ":"); found {
			service, part = strings.TrimSpace(s), rest
		}
		param, from, found := strings.Cut(strings.TrimSpace(part), "=")
		param, from = strings.TrimSpace(param), strings.TrimSpace(from)
		if param == "" {
			continue
		}
		if !found || from == "" {
			from = param
		}
		keys = append(keys, SharedKey{Service: service, Param: param, From: from})
	}
	return keys
}

// parseAtGroup разбирает скобку после @: операнды через |. Операнд в
// скобках — вложенная группа, операнд f @ (…) — вложенный множитель.
func (p *Parser) parseAtGroup(pos Position, fn Expr) (*AtExpr, error) {
	if _, err := p.expect(TokenLParen, "после @ — скобка с операндами: f @ (a | b)"); err != nil {
		return nil, err
	}
	group := &AtExpr{Pos: pos, Fn: fn}
	for {
		p.skipNewlines()
		var item Expr
		if p.check(TokenLParen) {
			inner, err := p.parseAtGroup(p.peek().Pos, nil)
			if err != nil {
				return nil, err
			}
			item = inner
		} else {
			// Уровень выше конвейера: | здесь разделяет операнды.
			expr, err := p.parseCompose()
			if err != nil {
				return nil, err
			}
			item = expr
		}
		group.Items = append(group.Items, item)
		p.skipNewlines()
		if p.check(TokenPipe) {
			p.advance()
			continue
		}
		if _, err := p.expect(TokenRParen, "— операнды @ разделяются | и закрываются )"); err != nil {
			return nil, err
		}
		return group, nil
	}
}

// atCall — один вызов: функция над операндом.
type atCall struct {
	fn   *atFn
	leaf *atLeaf
}

// atFn — функция множителя.
type atFn struct {
	name  string
	value Value    // функция языка; nil — команда ochan
	argv  []string // команда ochan: слова команды
	keys  []SharedKey
}

// atLeaf — операнд: значение и общие строки, которых он ждёт.
type atLeaf struct {
	expr  Expr
	value Value
	name  string
	// path — плейбук-файл; data — его содержимое.
	path string
	data []byte
	// needs — общие строки: параметр плейбука → ключ в ответе.
	needs map[string]string
	// service — поле service плейбука; keys — ключи, которые в нём есть.
	service string
	keys    map[string]bool
}

// evalAt вычисляет f @ (…).
func (in *Interpreter) evalAt(e *AtExpr, env *scope) (Value, error) {
	calls, _, err := in.atPlan(e, env)
	if err != nil {
		return nil, err
	}

	// Какие общие строки нужны операндам: их и ищем в ответах.
	wanted := map[string]bool{}
	for _, c := range calls {
		for _, from := range c.needs() {
			wanted[from] = true
		}
	}

	found := map[string]string{}
	results := make(ListValue, 0, len(calls))
	var waiting []atCall
	run := func(c atCall) error {
		value, err := in.atInvoke(c, found)
		if err != nil {
			return err
		}
		results = append(results, value)
		for key := range wanted {
			if v, ok := sharedIn(value, key); ok {
				found[key] = v
			}
		}
		return nil
	}
	// ready — вызову хватает общих строк. В плане вызовы, которые что-то
	// меняют, не выполняются и ответа не дают: ждать нечего, и плейбук
	// уходит в план как есть.
	ready := func(c atCall) bool {
		if in.dry != nil {
			return true
		}
		for _, from := range c.needs() {
			if _, ok := found[from]; !ok {
				return false
			}
		}
		return true
	}
	flush := func() error {
		for progress := true; progress; {
			progress = false
			for i := 0; i < len(waiting); i++ {
				if ready(waiting[i]) {
					c := waiting[i]
					waiting = append(waiting[:i], waiting[i+1:]...)
					if err := run(c); err != nil {
						return err
					}
					progress = true
					break
				}
			}
		}
		return nil
	}

	for _, c := range calls {
		if err := in.ctx.Err(); err != nil {
			return nil, in.errorf(e.Pos, "выполнение прервано: %v", err)
		}
		if !ready(c) {
			waiting = append(waiting, c)
			continue
		}
		if err := run(c); err != nil {
			return nil, err
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if len(waiting) > 0 {
		var missing []string
		for _, c := range waiting {
			for param, from := range c.leaf.needs {
				if _, ok := found[from]; !ok {
					missing = append(missing, fmt.Sprintf("%s(%s): %s", c.fn.name, c.leaf.name, describeShared(param, from)))
				}
			}
		}
		sort.Strings(missing)
		return nil, in.errorf(e.Pos, "общая строка не найдена ни в одном ответе — вызовы ждут её: %s",
			strings.Join(uniqueSorted(missing), "; "))
	}
	return results, nil
}

func describeShared(param, from string) string {
	if param == from {
		return param
	}
	return param + " (в ответе — " + from + ")"
}

func uniqueSorted(list []string) []string {
	var out []string
	for i, s := range list {
		if i == 0 || s != list[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// needs — ключи ответа, которых ждёт вызов: свои у плейбука и те, что
// объявила команда.
func (c atCall) needs() []string {
	var keys []string
	for _, from := range c.leaf.needs {
		keys = append(keys, from)
	}
	sort.Strings(keys)
	return keys
}

// atPlan раскрывает скобки в список вызовов: глубже — раньше. Возвращает
// вызовы и операнды группы — внешний множитель применяется к ним.
func (in *Interpreter) atPlan(e *AtExpr, env *scope) ([]atCall, []*atLeaf, error) {
	var fn *atFn
	if e.Fn != nil {
		f, err := in.atFunc(e.Fn, env)
		if err != nil {
			return nil, nil, err
		}
		fn = f
	}
	var calls []atCall
	var leaves []*atLeaf
	for _, item := range e.Items {
		if inner, ok := item.(*AtExpr); ok {
			innerCalls, innerLeaves, err := in.atPlan(inner, env)
			if err != nil {
				return nil, nil, err
			}
			calls = append(calls, innerCalls...)
			if fn != nil {
				for _, leaf := range innerLeaves {
					calls = append(calls, atCall{fn: fn, leaf: leaf.forFn(fn)})
				}
			}
			leaves = append(leaves, innerLeaves...)
			continue
		}
		leaf, err := in.atLeaf(item, env)
		if err != nil {
			return nil, nil, err
		}
		if fn != nil {
			calls = append(calls, atCall{fn: fn, leaf: leaf.forFn(fn)})
		}
		leaves = append(leaves, leaf)
	}
	return calls, leaves, nil
}

// forFn — операнд для вызова этой функцией: к общим строкам плейбука
// добавляются те, что объявила команда.
func (l *atLeaf) forFn(fn *atFn) *atLeaf {
	if len(fn.keys) == 0 || (l.path == "" && !isMap(l.value)) {
		return l
	}
	copied := *l
	copied.needs = map[string]string{}
	for k, v := range l.needs {
		copied.needs[k] = v
	}
	for _, key := range fn.keys {
		if (key.Service != "" && key.Service == l.service) || (key.Service == "" && l.keys[key.Param]) {
			copied.needs[key.Param] = key.From
		}
	}
	return &copied
}

func isMap(v Value) bool {
	_, ok := v.(MapValue)
	return ok
}

// atFunc — функция множителя: что-то вызываемое в языке, иначе команда
// ochan с таким именем (approve, модуль.команда) или строка с командой.
func (in *Interpreter) atFunc(expr Expr, env *scope) (*atFn, error) {
	if id, ok := expr.(*Ident); ok {
		value, err := in.funcRef(id, env)
		if err == nil {
			return &atFn{name: id.Qualified(), value: value}, nil
		}
		words := []string{id.Name}
		if id.Module != "" {
			words = []string{id.Module, id.Name}
		}
		if fn, ok := in.atCommand(words); ok {
			return fn, nil
		}
		return nil, err
	}
	value, err := in.eval(expr, env)
	if err != nil {
		return nil, err
	}
	if text, ok := value.(StringValue); ok {
		if fn, ok := in.atCommand(strings.Fields(string(text))); ok {
			return fn, nil
		}
		return nil, in.errorf(expr.Position(), "слева от @ — %q: такой команды ochan нет", string(text))
	}
	if !isCallable(value) {
		return nil, in.errorf(expr.Position(), "слева от @ должна быть функция или команда ochan, получено %s (%s)",
			value.TypeName(), Display(value))
	}
	return &atFn{name: Display(value), value: value}, nil
}

// atCommand — команда ochan как функция множителя.
func (in *Interpreter) atCommand(words []string) (*atFn, bool) {
	if in.commands == nil || in.sandbox != nil || len(words) == 0 {
		return nil, false
	}
	if _, _, _, err := in.commands.Resolve(words); err != nil {
		return nil, false
	}
	fn := &atFn{name: strings.Join(words, " "), argv: words}
	if shared, ok := in.commands.(SharedCommands); ok {
		fn.keys = shared.Shared(words)
	}
	return fn, true
}

// atLeaf вычисляет операнд и находит общие строки, которых он ждёт.
func (in *Interpreter) atLeaf(expr Expr, env *scope) (*atLeaf, error) {
	value, err := in.eval(expr, env)
	if err != nil {
		return nil, err
	}
	leaf := &atLeaf{expr: expr, value: value, name: Display(value), needs: map[string]string{}}
	leaf.keys = map[string]bool{}
	switch v := value.(type) {
	case MapValue:
		collectPlaceholders(v, leaf.needs)
		collectKeys(v, leaf.keys)
		if s, ok := v["service"].(StringValue); ok {
			leaf.service = string(s)
		}
	case StringValue:
		name := string(v)
		switch strings.ToLower(filepath.Ext(name)) {
		case ".yaml", ".yml", ".json":
			if ok, _ := in.fileExists(name); !ok {
				break
			}
			data, err := in.readFile(name)
			if err != nil {
				return nil, in.wrap(expr.Position(), err)
			}
			data = quotePlaceholders(data)
			leaf.path, leaf.data, leaf.name = name, data, filepath.Base(name)
			var doc any
			if err := yaml.Unmarshal(data, &doc); err != nil {
				return nil, in.errorf(expr.Position(), "плейбук %s не разбирается: %v", name, err)
			}
			collectPlaceholders(doc, leaf.needs)
			collectKeys(doc, leaf.keys)
			if m, ok := doc.(map[string]any); ok {
				leaf.service, _ = m["service"].(string)
			}
		}
	}
	return leaf, nil
}

// barePlaceholder — «ключ: ?» в YAML: ? без кавычек YAML читает как начало
// сложного ключа, а пишут его именно так.
var barePlaceholder = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?[^\s#:][^#\n]*?:[ \t]+)\?([ \t]*(?:#.*)?)$`)

// quotePlaceholders — «ключ: ?» → «ключ: "?"»: так плейбук разбирается.
func quotePlaceholders(data []byte) []byte {
	return barePlaceholder.ReplaceAll(data, []byte(`${1}"?"${2}`))
}

// collectKeys — все ключи плейбука на любой глубине.
func collectKeys(v any, keys map[string]bool) {
	switch x := v.(type) {
	case MapValue:
		for key, item := range x {
			keys[key] = true
			collectKeys(item, keys)
		}
	case ListValue:
		for _, item := range x {
			collectKeys(item, keys)
		}
	case map[string]any:
		for key, item := range x {
			keys[key] = true
			collectKeys(item, keys)
		}
	case []any:
		for _, item := range x {
			collectKeys(item, keys)
		}
	}
}

// collectPlaceholders находит ключи со значением ?.
func collectPlaceholders(v any, needs map[string]string) {
	switch x := v.(type) {
	case MapValue:
		for key, item := range x {
			if s, ok := item.(StringValue); ok && string(s) == sharedPlaceholder {
				needs[key] = key
				continue
			}
			collectPlaceholders(item, needs)
		}
	case ListValue:
		for _, item := range x {
			collectPlaceholders(item, needs)
		}
	case map[string]any:
		for key, item := range x {
			if s, ok := item.(string); ok && s == sharedPlaceholder {
				needs[key] = key
				continue
			}
			collectPlaceholders(item, needs)
		}
	case []any:
		for _, item := range x {
			collectPlaceholders(item, needs)
		}
	}
}

// atInvoke выполняет вызов с заполненными общими строками.
func (in *Interpreter) atInvoke(c atCall, found map[string]string) (Value, error) {
	arg, err := in.atFill(c.leaf, found)
	if err != nil {
		return nil, err
	}
	pos := c.leaf.expr.Position()
	var value Value
	if c.fn.value != nil {
		value, err = in.callValue(c.fn.value, []Value{arg}, pos)
	} else {
		args := make([]Value, 0, len(c.fn.argv)+1)
		for _, w := range c.fn.argv {
			args = append(args, StringValue(w))
		}
		if list, ok := arg.(ListValue); ok {
			args = append(args, list...)
		} else {
			args = append(args, arg)
		}
		value, err = builtinOchan(in, args)
		if err != nil {
			err = in.wrap(pos, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s @ %s: %w", c.fn.name, c.leaf.name, err)
	}
	return value, nil
}

// atFill — операнд с подставленными общими строками.
func (in *Interpreter) atFill(leaf *atLeaf, found map[string]string) (Value, error) {
	if len(leaf.needs) == 0 {
		return leaf.value, nil
	}
	values := map[string]string{}
	for param, from := range leaf.needs {
		value, ok := found[from]
		if !ok {
			// Только в плане: ответа, где была бы строка, нет — плейбук
			// уходит в план как есть.
			return leaf.value, nil
		}
		values[param] = value
	}
	if m, ok := leaf.value.(MapValue); ok {
		return fillMap(m, values, true).(MapValue), nil
	}
	if leaf.path == "" {
		return leaf.value, nil
	}
	data, err := fillPlaybook(leaf.data, values)
	if err != nil {
		return nil, fmt.Errorf("плейбук %s: %w", leaf.path, err)
	}
	ext := filepath.Ext(leaf.path)
	filled := strings.TrimSuffix(leaf.path, ext) + ".at" + ext
	if err := in.writeFile(EffectWrite, filled, data); err != nil {
		return nil, fmt.Errorf("плейбук %s: копия с общими строками: %w", leaf.path, err)
	}
	return StringValue(filled), nil
}

// fillMap — копия словаря с общими строками: и на месте ?, и в ключах,
// которые объявила команда. top — верхний уровень: объявленный параметр,
// которого нет нигде, добавляется в parameters или в сам словарь.
func fillMap(v Value, values map[string]string, top bool) Value {
	switch x := v.(type) {
	case MapValue:
		out := make(MapValue, len(x))
		for key, item := range x {
			if value, ok := values[key]; ok {
				if _, nested := item.(MapValue); !nested {
					out[key] = StringValue(value)
					continue
				}
			}
			out[key] = fillMap(item, values, false)
		}
		if top {
			for key, value := range values {
				if !hasKey(out, key) {
					if params, ok := out["parameters"].(MapValue); ok {
						params[key] = StringValue(value)
					} else {
						out[key] = StringValue(value)
					}
				}
			}
		}
		return out
	case ListValue:
		out := make(ListValue, len(x))
		for i, item := range x {
			out[i] = fillMap(item, values, false)
		}
		return out
	}
	return v
}

func hasKey(v Value, key string) bool {
	switch x := v.(type) {
	case MapValue:
		if _, ok := x[key]; ok {
			return true
		}
		for _, item := range x {
			if hasKey(item, key) {
				return true
			}
		}
	case ListValue:
		for _, item := range x {
			if hasKey(item, key) {
				return true
			}
		}
	}
	return false
}

// fillPlaybook ставит общие строки в плейбук, сохраняя его вид:
// комментарии и порядок ключей остаются. Параметр, которого в плейбуке
// нет (его объявила команда), добавляется в parameters.
func fillPlaybook(data []byte, values map[string]string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("плейбук — не словарь")
	}
	set := map[string]bool{}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i], n.Content[i+1]
				if v, ok := values[key.Value]; ok && value.Kind == yaml.ScalarNode {
					value.Value, value.Tag, value.Style = v, "!!str", yaml.DoubleQuotedStyle
					set[key.Value] = true
					continue
				}
				walk(value)
			}
		case yaml.SequenceNode, yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c)
			}
		}
	}
	walk(&doc)

	root := doc.Content[0]
	var missing []string
	for key := range values {
		if !set[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		params := mappingValue(root, "parameters")
		if params == nil {
			params = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "parameters"}, params)
		}
		for _, key := range missing {
			params.Content = append(params.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: values[key], Style: yaml.DoubleQuotedStyle})
		}
	}

	if json.Valid(bytes.TrimSpace(data)) {
		// JSON остаётся JSON: тот же документ, заполненный.
		var plain any
		if err := doc.Decode(&plain); err != nil {
			return nil, err
		}
		out, err := json.MarshalIndent(plain, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(out, '\n'), nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key && n.Content[i+1].Kind == yaml.MappingNode {
			return n.Content[i+1]
		}
	}
	return nil
}

// sharedIn ищет общую строку в ответе. Ключ с точкой — путь: session.token
// — поле token словаря session; не нашлось по пути — ищется как есть.
func sharedIn(v Value, key string) (string, bool) {
	if first, rest, dotted := strings.Cut(key, "."); dotted {
		if m, ok := v.(MapValue); ok {
			if inner, ok := m[first]; ok {
				if s, ok := sharedIn(inner, rest); ok {
					return s, true
				}
			}
		}
		if text, ok := v.(StringValue); ok {
			var doc any
			if json.Unmarshal([]byte(strings.TrimSpace(string(text))), &doc) == nil {
				if s, ok := jsonPath(doc, key); ok {
					return s, true
				}
			}
		}
	}
	switch x := v.(type) {
	case MapValue:
		if item, ok := x[key]; ok {
			if s, ok := scalarText(item); ok {
				return s, true
			}
		}
		for _, k := range x.SortedKeys() {
			if s, ok := sharedIn(x[k], key); ok {
				return s, true
			}
		}
	case ListValue:
		for _, item := range x {
			if s, ok := sharedIn(item, key); ok {
				return s, true
			}
		}
	case ConfigValue:
		if s, err := x.Lookup(key); err == nil && s != "" && s != sharedPlaceholder {
			return s, true
		}
	case StringValue:
		return sharedInText(string(x), key)
	}
	return "", false
}

func scalarText(v Value) (string, bool) {
	switch v.(type) {
	case StringValue, IntValue, FloatValue, BoolValue:
		if s := v.Text(); s != "" && s != sharedPlaceholder {
			return s, true
		}
	}
	return "", false
}

// sharedInText — общая строка в тексте ответа: в JSON — по ключу на любой
// глубине, иначе строкой «ключ: значение», «ключ = значение» или
// «"ключ": "значение"».
func sharedInText(text, key string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var doc any
		if err := json.Unmarshal([]byte(trimmed), &doc); err == nil {
			if s, ok := sharedInJSON(doc, key); ok {
				return s, true
			}
		}
	}
	pattern := regexp.MustCompile(`(?m)["']?` + regexp.QuoteMeta(key) + `["']?\s*[:=]\s*(?:"([^"\n]*)"|'([^'\n]*)'|([^\s,;}\]]+))`)
	for _, m := range pattern.FindAllStringSubmatchIndex(text, -1) {
		// Ключ — отдельное слово: string_i не найдётся в my_string_i.
		if m[0] > 0 {
			prev := text[m[0]-1]
			if prev == '_' || prev == '-' || prev == '.' || isWordByte(prev) {
				continue
			}
		}
		for g := 1; g <= 3; g++ {
			if m[2*g] >= 0 {
				if s := text[m[2*g]:m[2*g+1]]; s != "" && s != sharedPlaceholder {
					return s, true
				}
			}
		}
	}
	return "", false
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b >= 0x80
}

// jsonPath — значение по пути a.b.c в разобранном JSON.
func jsonPath(v any, path string) (string, bool) {
	for _, part := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return "", false
		}
		if v, ok = m[part]; !ok {
			return "", false
		}
	}
	switch s := v.(type) {
	case string:
		return s, s != "" && s != sharedPlaceholder
	case float64, bool:
		return fmt.Sprint(s), true
	}
	return "", false
}

func sharedInJSON(v any, key string) (string, bool) {
	if strings.Contains(key, ".") {
		if s, ok := jsonPath(v, key); ok {
			return s, true
		}
	}
	switch x := v.(type) {
	case map[string]any:
		if item, ok := x[key]; ok {
			switch s := item.(type) {
			case string:
				if s != "" && s != sharedPlaceholder {
					return s, true
				}
			case float64, bool:
				return fmt.Sprint(s), true
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := sharedInJSON(x[k], key); ok {
				return s, true
			}
		}
	case []any:
		for _, item := range x {
			if s, ok := sharedInJSON(item, key); ok {
				return s, true
			}
		}
	case string:
		return sharedInText(x, key)
	}
	return "", false
}
