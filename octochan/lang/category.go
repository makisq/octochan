package lang

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"octochan/config"
)

// Категория конфигураций.
//
// Объекты — конфигурации. Морфизм — преобразование одной конфигурации в
// другую, и он такое же значение, как число или функция: его хранят в
// переменной, печатают, композируют и проверяют до того, как что-то
// записано.
//
//	m = config("test.conf") -> config("prod.conf")   — что меняется и с чего
//	m(c)                    — применить; c обязан совпасть с началом стрелки
//	m1 >> m2                — сначала m1, потом m2; стыковка проверяется
//	inverse(m), id()        — обратный и тождественный
//	commutes(p, q[, объекты]) — два пути дают одно и то же
//
// Морфизм-дельта помнит только меняющиеся параметры: их прежние значения —
// условие применения, новые — результат. Поэтому его можно применить не
// только к тому конфигу, из которого он получен, но и к любому другому с
// теми же исходными значениями, а несовпадение — ошибка, а не тихая
// перезапись чужой правки. Композиция и коммутативность дельт решаются
// символьно, без конфигов; морфизм-функция проверяется на объектах.

// paramRef — параметр конфига: секция и ключ раздельно, как в diff.
type paramRef struct {
	section string
	key     string
}

func (p paramRef) String() string {
	if p.section == "" {
		return p.key
	}
	return p.section + "." + p.key
}

func parseParamRef(name string) paramRef {
	if section, key, ok := strings.Cut(name, "."); ok {
		return paramRef{section, key}
	}
	return paramRef{key: name}
}

// slot — значение параметра или его отсутствие.
type slot struct {
	present bool
	value   string
}

func (s slot) String() string {
	if !s.present {
		return "нет"
	}
	return s.value
}

// same сравнивает значения по смыслу, как diff: 8GB и 8192MB равны.
func (s slot) same(other slot) bool {
	if s.present != other.present {
		return false
	}
	return !s.present || config.NormalizeValue(s.value) == config.NormalizeValue(other.value)
}

// MorphValue — морфизм между конфигурациями.
//
// Ровно один из видов:
//   - дельта: pre — условия применения, post — результат;
//   - функция: fn, конфиг -> конфиг;
//   - цепочка: chain, где есть хотя бы одна функция (соседние дельты в ней
//     уже склеены).
type MorphValue struct {
	Name  string
	pre   map[paramRef]slot
	post  map[paramRef]slot
	fn    *FuncValue
	chain []*MorphValue
}

func (m *MorphValue) isDelta() bool { return m.fn == nil && m.chain == nil }

func newDelta(name string) *MorphValue {
	return &MorphValue{Name: name, pre: map[paramRef]slot{}, post: map[paramRef]slot{}}
}

// identityMorph — тождественный морфизм: ничего не требует и не меняет.
func identityMorph() *MorphValue { return newDelta("id") }

func (m *MorphValue) Text() string {
	switch {
	case m.fn != nil:
		return "морфизм " + m.fn.Text()
	case m.chain != nil:
		parts := make([]string, len(m.chain))
		for i, step := range m.chain {
			parts[i] = step.Text()
		}
		return strings.Join(parts, " >> ")
	}
	changes := m.changes()
	if len(changes) == 0 && len(m.pre) == 0 {
		return "морфизм id"
	}
	head := "морфизм"
	if m.Name != "" {
		head += " " + m.Name
	}
	if len(changes) == 0 {
		return head + ": ничего не меняет"
	}
	return head + ": " + strings.Join(changes, "; ")
}
func (*MorphValue) Truthy() bool     { return true }
func (*MorphValue) TypeName() string { return "морфизм" }

// changes — изменения дельты по порядку параметров.
func (m *MorphValue) changes() []string {
	var out []string
	for _, ref := range sortedRefs(m.post) {
		after := m.post[ref]
		before, known := m.pre[ref]
		switch {
		case !after.present:
			out = append(out, ref.String()+" удалить")
		case known && !before.present:
			out = append(out, ref.String()+" добавить = "+after.value)
		case known:
			out = append(out, ref.String()+" "+before.value+" -> "+after.value)
		default:
			out = append(out, ref.String()+" = "+after.value)
		}
	}
	return out
}

func sortedRefs(set map[paramRef]slot) []paramRef {
	refs := make([]paramRef, 0, len(set))
	for ref := range set {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].section != refs[j].section {
			return refs[i].section < refs[j].section
		}
		return refs[i].key < refs[j].key
	})
	return refs
}

// sameDelta — две дельты с одинаковыми условиями и результатом.
func sameDelta(a, b *MorphValue) bool {
	if len(a.pre) != len(b.pre) || len(a.post) != len(b.post) {
		return false
	}
	for ref, s := range a.pre {
		if other, ok := b.pre[ref]; !ok || !s.same(other) {
			return false
		}
	}
	for ref, s := range a.post {
		if other, ok := b.post[ref]; !ok || !s.same(other) {
			return false
		}
	}
	return true
}

// deltaBetween — морфизм a -> b: всё, чем b отличается от a.
func deltaBetween(a, b ConfigValue) *MorphValue {
	name := ""
	if a.Name != "" && b.Name != "" {
		name = a.Name + " -> " + b.Name
	}
	m := newDelta(name)
	for _, entry := range config.CompareConfigs(a.Data, b.Data, a.Name, b.Name).Entries {
		ref := paramRef{entry.Section, entry.Key}
		m.pre[ref] = slot{entry.LeftExists, entry.Left}
		m.post[ref] = slot{entry.RightExists, entry.Right}
	}
	return m
}

// apply применяет морфизм к копии конфига.
func (in *Interpreter) applyMorph(m *MorphValue, c ConfigValue) (ConfigValue, error) {
	switch {
	case m.fn != nil:
		result, err := in.callFunc(m.fn, []Value{copyValue(c)}, Position{})
		if err != nil {
			return ConfigValue{}, err
		}
		out, err := objectOf(in, result)
		if err != nil {
			return ConfigValue{}, fmt.Errorf("%s вернул не конфиг: %w", m.Text(), err)
		}
		return out, nil

	case m.chain != nil:
		for _, step := range m.chain {
			next, err := in.applyMorph(step, c)
			if err != nil {
				return ConfigValue{}, err
			}
			c = next
		}
		return c, nil
	}

	for _, ref := range sortedRefs(m.pre) {
		want := m.pre[ref]
		value, ok := c.Data[ref.section][ref.key]
		if have := (slot{ok, value}); !have.same(want) {
			return ConfigValue{}, fmt.Errorf("%s не применяется к %s: ожидается %s = %s, в конфиге %s",
				m.label(), objectName(c), ref, want, have)
		}
	}
	out := copyValue(c).(ConfigValue)
	for _, ref := range sortedRefs(m.post) {
		after := m.post[ref]
		if !after.present {
			delete(out.Data[ref.section], ref.key)
			continue
		}
		if out.Data[ref.section] == nil {
			out.Data[ref.section] = map[string]string{}
		}
		out.Data[ref.section][ref.key] = after.value
	}
	return out, nil
}

func (m *MorphValue) label() string {
	if m.Name != "" {
		return "морфизм " + m.Name
	}
	return "морфизм"
}

// composeMorph — сначала a, потом b. Соседние дельты склеиваются сразу,
// и несостыковка — ошибка здесь, а не при применении.
func composeMorph(a, b *MorphValue) (*MorphValue, error) {
	var steps []*MorphValue
	for _, m := range []*MorphValue{a, b} {
		if m.chain != nil {
			steps = append(steps, m.chain...)
		} else {
			steps = append(steps, m)
		}
	}

	var merged []*MorphValue
	for _, step := range steps {
		if n := len(merged); n > 0 && merged[n-1].isDelta() && step.isDelta() {
			joined, err := composeDeltas(merged[n-1], step)
			if err != nil {
				return nil, err
			}
			merged[n-1] = joined
			continue
		}
		merged = append(merged, step)
	}
	if len(merged) == 1 {
		return merged[0], nil
	}
	return &MorphValue{chain: merged}, nil
}

// composeDeltas склеивает две дельты: результат первой обязан совпасть с
// условиями второй.
func composeDeltas(a, b *MorphValue) (*MorphValue, error) {
	name := ""
	if a.Name != "" && b.Name != "" {
		name = a.Name + " >> " + b.Name
	}
	m := newDelta(name)
	for ref, s := range a.pre {
		m.pre[ref] = s
	}
	for ref, s := range a.post {
		m.post[ref] = s
	}

	for _, ref := range sortedRefs(b.pre) {
		want := b.pre[ref]
		if after, changed := a.post[ref]; changed {
			if !after.same(want) {
				return nil, fmt.Errorf("морфизмы не стыкуются: после первого %s = %s, второй ожидает %s",
					ref, after, want)
			}
			continue
		}
		if before, required := a.pre[ref]; required {
			if !before.same(want) {
				return nil, fmt.Errorf("морфизмы не стыкуются: первый ожидает %s = %s и его не меняет, второй ожидает %s",
					ref, before, want)
			}
			continue
		}
		m.pre[ref] = want
	}
	for ref, s := range b.post {
		m.post[ref] = s
	}

	// Параметр, вернувшийся к исходному значению, не меняется; условие на
	// него остаётся.
	for ref, after := range m.post {
		if before, ok := m.pre[ref]; ok && before.same(after) {
			delete(m.post, ref)
		}
	}
	return m, nil
}

// inverseMorph — обратная дельта.
func inverseMorph(m *MorphValue) (*MorphValue, error) {
	if !m.isDelta() {
		return nil, fmt.Errorf("у %s нет обратного: обращаются только дельты a -> b", m.Text())
	}
	name := ""
	if m.Name != "" {
		name = "inverse(" + m.Name + ")"
	}
	out := newDelta(name)
	for ref, before := range m.pre {
		out.pre[ref] = before
	}
	for _, ref := range sortedRefs(m.post) {
		before, known := m.pre[ref]
		if !known {
			return nil, fmt.Errorf("%s не обращается: прежнее значение %s неизвестно", m.label(), ref)
		}
		out.pre[ref] = m.post[ref]
		out.post[ref] = before
	}
	return out, nil
}

// objectName — как назвать объект в сообщении.
func objectName(c ConfigValue) string {
	if c.Name == "" {
		return "словарю"
	}
	return "конфигу " + c.Name
}

// shownName — имя объекта в начале строки отчёта.
func shownName(c ConfigValue) string {
	if c.Name == "" {
		return "словарь"
	}
	return c.Name
}

// objectOf — конфиг из значения: конфиг, словарь или источник.
//
// В словаре вложенный словарь — секция: {"server": {"port": 5432}}.
func objectOf(in *Interpreter, v Value) (ConfigValue, error) {
	if m, ok := v.(MapValue); ok {
		data := config.ConfigMap{"": {}}
		for _, name := range m.SortedKeys() {
			if section, ok := m[name].(MapValue); ok {
				params := make(map[string]string, len(section))
				for key, value := range section {
					params[key] = value.Text()
				}
				data[name] = params
				continue
			}
			data[""][name] = m[name].Text()
		}
		return ConfigValue{Data: data}, nil
	}
	return in.configOf(v)
}

// morphArg — морфизм из значения: морфизм, функция конфиг -> конфиг или
// словарь новых значений (без условий: задать, что бы ни стояло).
func morphArg(name string, v Value) (*MorphValue, error) {
	switch val := v.(type) {
	case *MorphValue:
		return val, nil
	case *FuncValue:
		if val == identity {
			return identityMorph(), nil
		}
		return &MorphValue{fn: val}, nil
	case MapValue:
		m := newDelta("")
		for _, key := range val.SortedKeys() {
			ref := parseParamRef(key)
			if _, isNull := val[key].(NullValue); isNull {
				m.post[ref] = slot{}
				continue
			}
			m.post[ref] = slot{true, val[key].Text()}
		}
		return m, nil
	}
	return nil, fmt.Errorf("%s: нужен морфизм — a -> b, morph(fn(c) => …) или morph({\"параметр\": значение}); получено %s",
		name, v.TypeName())
}

// ---------------------------------------------------------------------------
// Коммутативность
// ---------------------------------------------------------------------------

// conflicts перечисляет, чем расходятся два пути. Пустой список — пути
// коммутируют.
//
// Дельты сравниваются символьно: для каждого параметра, который хоть один
// путь требует или меняет, итог по обоим путям обязан совпасть на любом
// конфиге, к которому применимы оба. Морфизмы-функции так не сравнить, и
// для них нужны объекты: пути применяются к каждому, итоги сравниваются.
func (in *Interpreter) conflicts(p, q *MorphValue, objects []ConfigValue) ([]string, error) {
	if len(objects) > 0 {
		return in.conflictsOn(p, q, objects)
	}
	if !p.isDelta() || !q.isDelta() {
		return nil, errors.New("морфизмы-функции сравниваются на объектах: commutes(p, q, [конфиги])")
	}

	refs := map[paramRef]slot{}
	for _, set := range []map[paramRef]slot{p.pre, q.pre, p.post, q.post} {
		for ref := range set {
			refs[ref] = slot{}
		}
	}
	var out []string
	for _, ref := range sortedRefs(refs) {
		preP, hasP := p.pre[ref]
		preQ, hasQ := q.pre[ref]
		if hasP && hasQ && !preP.same(preQ) {
			out = append(out, fmt.Sprintf("%s: пути начинаются с разных значений — %s и %s", ref, preP, preQ))
			continue
		}
		pre, known := preP, hasP
		if !known {
			pre, known = preQ, hasQ
		}
		outP, setP := p.post[ref]
		outQ, setQ := q.post[ref]
		if !setP && known {
			outP, setP = pre, true
		}
		if !setQ && known {
			outQ, setQ = pre, true
		}
		switch {
		case !setP && !setQ:
		case !setP:
			out = append(out, fmt.Sprintf("%s: по первому пути не меняется, по второму — %s", ref, outQ))
		case !setQ:
			out = append(out, fmt.Sprintf("%s: по первому пути %s, по второму не меняется", ref, outP))
		case !outP.same(outQ):
			out = append(out, fmt.Sprintf("%s: по первому пути %s, по второму %s", ref, outP, outQ))
		}
	}
	return out, nil
}

func (in *Interpreter) conflictsOn(p, q *MorphValue, objects []ConfigValue) ([]string, error) {
	var out []string
	common := 0
	for _, object := range objects {
		left, errP := in.applyMorph(p, object)
		right, errQ := in.applyMorph(q, object)
		switch {
		case errP != nil && errQ != nil:
			// Объект вне начала обоих путей: сравнивать нечего.
			continue
		case errP != nil:
			out = append(out, fmt.Sprintf("%s: первый путь не применяется: %v", shownName(object), errP))
			continue
		case errQ != nil:
			out = append(out, fmt.Sprintf("%s: второй путь не применяется: %v", shownName(object), errQ))
			continue
		}
		common++
		for _, entry := range config.CompareConfigs(left.Data, right.Data, "", "").Entries {
			out = append(out, fmt.Sprintf("%s: %s: по первому пути %s, по второму %s", shownName(object), entry.Param(),
				slot{entry.LeftExists, entry.Left}, slot{entry.RightExists, entry.Right}))
		}
	}
	if common == 0 && len(out) == 0 {
		return nil, errors.New("ни один объект не подходит к началу путей: проверять не на чем")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Функторы
// ---------------------------------------------------------------------------

// FunctorValue — функтор: переводит конфиги в конфиги и морфизмы в
// морфизмы, сохраняя тождество и композицию.
//
// Функтор описывает соответствие на уровне протоколов и машин: тот же
// набор изменений для PostgreSQL и для пула соединений перед ним, для
// стенда и для боя. Задаётся переименованием параметров — тогда он
// переводит и дельты, — или функцией над конфигами и, если нужно,
// функцией над морфизмами.
type FunctorValue struct {
	Name   string
	rename map[paramRef]paramRef
	// drop — параметры, которые функтор забывает; dropRest — забыть все,
	// кроме перечисленных в rename ("*": null).
	drop     map[paramRef]bool
	dropRest bool
	objFn    *FuncValue
	morphFn  *FuncValue
	chain    []*FunctorValue
}

func (f *FunctorValue) Text() string {
	switch {
	case f.chain != nil:
		parts := make([]string, len(f.chain))
		for i, step := range f.chain {
			parts[i] = step.Text()
		}
		return strings.Join(parts, " >> ")
	case f.rename != nil:
		pairs := make([]string, 0, len(f.rename)+len(f.drop)+1)
		for from, to := range f.rename {
			pairs = append(pairs, from.String()+" -> "+to.String())
		}
		for ref := range f.drop {
			pairs = append(pairs, ref.String()+" -> null")
		}
		sort.Strings(pairs)
		if f.dropRest {
			pairs = append(pairs, "* -> null")
		}
		return "функтор {" + strings.Join(pairs, ", ") + "}"
	}
	return "функтор " + f.objFn.Text()
}
func (*FunctorValue) Truthy() bool     { return true }
func (*FunctorValue) TypeName() string { return "функтор" }

// renameRef — куда функтор переводит параметр; false — забывает его.
func (f *FunctorValue) renameRef(ref paramRef) (paramRef, bool) {
	if to, ok := f.rename[ref]; ok {
		return to, true
	}
	if f.drop[ref] || f.dropRest {
		return paramRef{}, false
	}
	return ref, true
}

// object — образ конфига.
func (in *Interpreter) functorObject(f *FunctorValue, c ConfigValue) (ConfigValue, error) {
	switch {
	case f.chain != nil:
		for _, step := range f.chain {
			next, err := in.functorObject(step, c)
			if err != nil {
				return ConfigValue{}, err
			}
			c = next
		}
		return c, nil

	case f.rename != nil:
		out := ConfigValue{Name: c.Name, Data: config.ConfigMap{}, Notes: c.Notes}
		from := map[paramRef]paramRef{}
		for section, params := range c.Data {
			for key, value := range params {
				ref, keep := f.renameRef(paramRef{section, key})
				if !keep {
					continue
				}
				if prev, taken := from[ref]; taken {
					return ConfigValue{}, fmt.Errorf("%s: %s и %s переходят в один параметр %s",
						f.Text(), prev, paramRef{section, key}, ref)
				}
				from[ref] = paramRef{section, key}
				if out.Data[ref.section] == nil {
					out.Data[ref.section] = map[string]string{}
				}
				out.Data[ref.section][ref.key] = value
			}
		}
		return out, nil
	}

	result, err := in.callFunc(f.objFn, []Value{copyValue(c)}, Position{})
	if err != nil {
		return ConfigValue{}, err
	}
	out, err := objectOf(in, result)
	if err != nil {
		return ConfigValue{}, fmt.Errorf("%s вернул не конфиг: %w", f.Text(), err)
	}
	return out, nil
}

// functorMorph — образ морфизма. at — объект, из которого выходит
// стрелка: без отображения морфизмов образ строится как F(at) -> F(m(at)).
func (in *Interpreter) functorMorph(f *FunctorValue, m *MorphValue, at *ConfigValue) (*MorphValue, error) {
	switch {
	case f.chain != nil:
		for _, step := range f.chain {
			next, err := in.functorMorph(step, m, at)
			if err != nil {
				return nil, err
			}
			if at != nil {
				image, err := in.functorObject(step, *at)
				if err != nil {
					return nil, err
				}
				at = &image
			}
			m = next
		}
		return m, nil

	case f.morphFn != nil:
		result, err := in.callFunc(f.morphFn, []Value{m}, Position{})
		if err != nil {
			return nil, err
		}
		return morphArg(f.Text(), result)

	case f.rename != nil && m.isDelta():
		out := newDelta(m.Name)
		for ref, s := range m.pre {
			if to, keep := f.renameRef(ref); keep {
				out.pre[to] = s
			}
		}
		for ref, s := range m.post {
			if to, keep := f.renameRef(ref); keep {
				out.post[to] = s
			}
		}
		return out, nil
	}

	if at == nil {
		return nil, fmt.Errorf("%s не знает, куда переводить морфизмы: задайте functor(объекты, морфизмы) "+
			"или передайте объект — F(m, c) даёт стрелку F(c) -> F(m(c))", f.Text())
	}
	target, err := in.applyMorph(m, *at)
	if err != nil {
		return nil, err
	}
	source, err := in.functorObject(f, *at)
	if err != nil {
		return nil, err
	}
	image, err := in.functorObject(f, target)
	if err != nil {
		return nil, err
	}
	return deltaBetween(source, image), nil
}

// functorLaws проверяет F(id) = id и F(f >> g) = F(f) >> F(g) на данных.
func (in *Interpreter) functorLaws(f *FunctorValue, morphisms []*MorphValue, objects []ConfigValue) ([]string, error) {
	if len(objects) == 0 {
		return nil, errors.New("functor_laws: нужен хотя бы один объект проверки")
	}
	var out []string
	// same перечисляет, чем got отличается от want.
	same := func(got, want ConfigValue) []string {
		var diffs []string
		for _, entry := range config.CompareConfigs(got.Data, want.Data, "", "").Entries {
			diffs = append(diffs, fmt.Sprintf("%s: получено %s, ожидалось %s", entry.Param(),
				slot{entry.LeftExists, entry.Left}, slot{entry.RightExists, entry.Right}))
		}
		return diffs
	}

	for _, object := range objects {
		image, err := in.functorObject(f, object)
		if err != nil {
			return nil, err
		}
		fid, err := in.functorMorph(f, identityMorph(), &object)
		if err != nil {
			return nil, err
		}
		got, err := in.applyMorph(fid, image)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: F(id) не применяется к F(объекта): %v", shownName(object), err))
		} else if diffs := same(got, image); len(diffs) > 0 {
			out = append(out, fmt.Sprintf("%s: F(id) не тождественен: %s", shownName(object), strings.Join(diffs, "; ")))
		}
	}

	for i, first := range morphisms {
		for j, second := range morphisms {
			both, err := composeMorph(first, second)
			if err != nil {
				continue // не стыкуются — закону проверять нечего
			}
			for _, object := range objects {
				middle, err := in.applyMorph(first, object)
				if err != nil {
					continue
				}
				if _, err := in.applyMorph(second, middle); err != nil {
					continue
				}
				image, err := in.functorObject(f, object)
				if err != nil {
					return nil, err
				}
				whole, err := in.functorMorph(f, both, &object)
				if err != nil {
					return nil, err
				}
				left, err := in.functorMorph(f, first, &object)
				if err != nil {
					return nil, err
				}
				right, err := in.functorMorph(f, second, &middle)
				if err != nil {
					return nil, err
				}
				parts, err := composeMorph(left, right)
				if err != nil {
					out = append(out, fmt.Sprintf("%s, морфизмы %d и %d: F(f) и F(g) не стыкуются: %v", shownName(object), i+1, j+1, err))
					continue
				}
				a, errA := in.applyMorph(whole, image)
				b, errB := in.applyMorph(parts, image)
				switch {
				case errA != nil || errB != nil:
					out = append(out, fmt.Sprintf("%s, морфизмы %d и %d: образ не применяется к F(объекта): %v",
						shownName(object), i+1, j+1, errors.Join(errA, errB)))
				default:
					if diffs := same(a, b); len(diffs) > 0 {
						out = append(out, fmt.Sprintf("%s, морфизмы %d и %d: F(f >> g) ≠ F(f) >> F(g): %s",
							shownName(object), i+1, j+1, strings.Join(diffs, "; ")))
					}
				}
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Встроенные функции
// ---------------------------------------------------------------------------

func categoryBuiltins() []Builtin {
	return []Builtin{
		{Name: "morph", MinArgs: 1, MaxArgs: 1, Returns: TypeMorph,
			Doc: "морфизм: morph({\"параметр\": значение}) — задать, что бы ни стояло (null — удалить); morph(fn(c) => …) — функция конфиг -> конфиг",
			Fn:  builtinMorph},
		{Name: "inverse", MinArgs: 1, MaxArgs: 1, Returns: TypeMorph,
			Doc: "обратный морфизм: inverse(a -> b) — это b -> a", Fn: builtinInverse},
		{Name: "commutes", MinArgs: 2, MaxArgs: 3, Returns: TypeBool,
			Doc: "дают ли два пути одно и то же: commutes(p, q[, [конфиги]]); дельты сравниваются без конфигов",
			Fn:  builtinCommutes},
		{Name: "conflicts", MinArgs: 2, MaxArgs: 3, Returns: TypeList,
			Doc: "чем расходятся два пути: список; пустой — пути коммутируют", Fn: builtinConflicts},
		{Name: "dom", MinArgs: 1, MaxArgs: 1, Returns: TypeMap,
			Doc: "условия морфизма: параметр -> ожидаемое значение (null — параметра быть не должно)", Fn: builtinDom},
		{Name: "cod", MinArgs: 1, MaxArgs: 1, Returns: TypeMap,
			Doc: "результат морфизма: параметр -> новое значение (null — параметр удаляется)", Fn: builtinCod},
		{Name: "functor", MinArgs: 1, MaxArgs: 2, Returns: TypeFunctor,
			Doc: "функтор: functor({\"старое\": \"новое\", \"лишнее\": null, \"*\": null}) — переименование и забывание параметров; functor(fn(c) => …[, fn(m) => …])",
			Fn:  builtinFunctor},
		{Name: "functor_laws", MinArgs: 3, MaxArgs: 3, Returns: TypeList,
			Doc: "нарушения законов функтора на данных: functor_laws(F, [морфизмы], [конфиги]); пустой список — законы выполняются",
			Fn:  builtinFunctorLaws},
	}
}

func builtinMorph(_ *Interpreter, args []Value) (Value, error) {
	return morphArg("morph", args[0])
}

func builtinInverse(_ *Interpreter, args []Value) (Value, error) {
	m, err := morphArg("inverse", args[0])
	if err != nil {
		return nil, err
	}
	return inverseMorph(m)
}

// pathsAndObjects разбирает аргументы commutes и conflicts.
func (in *Interpreter) pathsAndObjects(name string, args []Value) (*MorphValue, *MorphValue, []ConfigValue, error) {
	p, err := morphArg(name, args[0])
	if err != nil {
		return nil, nil, nil, err
	}
	q, err := morphArg(name, args[1])
	if err != nil {
		return nil, nil, nil, err
	}
	var objects []ConfigValue
	if len(args) == 3 {
		if objects, err = in.objectList(name, args[2]); err != nil {
			return nil, nil, nil, err
		}
	}
	return p, q, objects, nil
}

func (in *Interpreter) objectList(name string, v Value) ([]ConfigValue, error) {
	list, ok := v.(ListValue)
	if !ok {
		list = ListValue{v}
	}
	objects := make([]ConfigValue, 0, len(list))
	for _, item := range list {
		c, err := objectOf(in, item)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		objects = append(objects, c)
	}
	return objects, nil
}

func builtinCommutes(in *Interpreter, args []Value) (Value, error) {
	p, q, objects, err := in.pathsAndObjects("commutes", args)
	if err != nil {
		return nil, err
	}
	diffs, err := in.conflicts(p, q, objects)
	if err != nil {
		return nil, err
	}
	return BoolValue(len(diffs) == 0), nil
}

func builtinConflicts(in *Interpreter, args []Value) (Value, error) {
	p, q, objects, err := in.pathsAndObjects("conflicts", args)
	if err != nil {
		return nil, err
	}
	diffs, err := in.conflicts(p, q, objects)
	if err != nil {
		return nil, err
	}
	return stringList(diffs), nil
}

func slotMap(name string, v Value, pick func(*MorphValue) map[paramRef]slot) (Value, error) {
	m, err := morphArg(name, v)
	if err != nil {
		return nil, err
	}
	if !m.isDelta() {
		return nil, fmt.Errorf("%s: у %s нет условий и результата в виде параметров — это функция", name, m.Text())
	}
	out := MapValue{}
	for ref, s := range pick(m) {
		if s.present {
			out[ref.String()] = StringValue(s.value)
		} else {
			out[ref.String()] = NullValue{}
		}
	}
	return out, nil
}

func builtinDom(_ *Interpreter, args []Value) (Value, error) {
	return slotMap("dom", args[0], func(m *MorphValue) map[paramRef]slot { return m.pre })
}

func builtinCod(_ *Interpreter, args []Value) (Value, error) {
	return slotMap("cod", args[0], func(m *MorphValue) map[paramRef]slot { return m.post })
}

func builtinFunctor(_ *Interpreter, args []Value) (Value, error) {
	f := &FunctorValue{}
	switch objects := args[0].(type) {
	case MapValue:
		// Значение null — забыть параметр; "*": null — забыть все
		// неперечисленные.
		f.rename = make(map[paramRef]paramRef, len(objects))
		f.drop = map[paramRef]bool{}
		for _, from := range objects.SortedKeys() {
			_, forget := objects[from].(NullValue)
			switch {
			case from == "*" && forget:
				f.dropRest = true
			case from == "*":
				return nil, errors.New("functor: \"*\" означает все неперечисленные параметры и принимает только null")
			case forget:
				f.drop[parseParamRef(from)] = true
			default:
				f.rename[parseParamRef(from)] = parseParamRef(objects[from].Text())
			}
		}
	case *FuncValue:
		f.objFn = objects
	default:
		return nil, fmt.Errorf("functor: объекты задаются словарём переименований или функцией конфиг -> конфиг, получено %s",
			args[0].TypeName())
	}
	if len(args) == 2 {
		morphisms, ok := args[1].(*FuncValue)
		if !ok {
			return nil, fmt.Errorf("functor: морфизмы задаются функцией морфизм -> морфизм, получено %s", args[1].TypeName())
		}
		f.morphFn = morphisms
	}
	return f, nil
}

func builtinFunctorLaws(in *Interpreter, args []Value) (Value, error) {
	f, ok := args[0].(*FunctorValue)
	if !ok {
		return nil, fmt.Errorf("functor_laws: первый аргумент — функтор, получено %s", args[0].TypeName())
	}
	list, ok := args[1].(ListValue)
	if !ok {
		return nil, fmt.Errorf("functor_laws: морфизмы — список, получено %s", args[1].TypeName())
	}
	morphisms := make([]*MorphValue, 0, len(list))
	for _, item := range list {
		m, err := morphArg("functor_laws", item)
		if err != nil {
			return nil, err
		}
		morphisms = append(morphisms, m)
	}
	objects, err := in.objectList("functor_laws", args[2])
	if err != nil {
		return nil, err
	}
	violations, err := in.functorLaws(f, morphisms, objects)
	if err != nil {
		return nil, err
	}
	return stringList(violations), nil
}
