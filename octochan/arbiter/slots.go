package arbiter

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"octochan/fp"
	"octochan/lang"
)

// Слоты обобщений промпта на плане.
//
// Обобщение g(x : τ, …) : R = op(…) говорит модели, какие значения она
// вправе подставить в операцию op: домен слота, значение, заданное ячейкой
// (set g.x = v), частные случаи (instance). Проверка промпта до модели
// смотрит, что сам промпт с собой согласен; здесь — что план держится
// этих границ. Без неё разрешённая операция принимала бы любое значение.

// SlotOutcome — итог проверки одного обобщения по плану.
type SlotOutcome struct {
	Generalization *fp.Generalization
	// Calls — сколько вызовов операции обобщения написала модель.
	Calls      int
	Violations []string
}

// OK сообщает, что вызовы держатся границ обобщения.
func (o SlotOutcome) OK() bool { return len(o.Violations) == 0 }

// Signature — имена параметров алиаса, который вызван; nil — неизвестны.
type Signature func(call lang.Invocation) []string

// DescribeGeneralization — обобщение в той записи, что в промпте.
func DescribeGeneralization(g *fp.Generalization) string {
	params := make([]string, len(g.Params))
	for i, p := range g.Params {
		switch {
		case len(p.Domain) > 0:
			params[i] = fmt.Sprintf("%s : {%s}", p.Name, strings.Join(p.Domain, ", "))
		case p.Range != nil:
			params[i] = p.Name + " : " + p.Range.Text
		default:
			params[i] = p.Name + " : " + p.Class
		}
	}
	return fmt.Sprintf("generalization %s(%s) = %s", g.Name, strings.Join(params, ", "), g.Op.Text)
}

// CheckSlots проверяет вызовы операций обобщений промпта. Вызов внутри
// скилла — реализация из библиотеки, а не решение модели: он не
// проверяется. Слот сопоставляется с аргументом по имени параметра
// алиаса, а если такого нет — по месту в записи op(…) обобщения. Слот,
// который не сопоставить ни так, ни так, — нарушение: границу, которую
// нельзя проверить, план не проходит.
func CheckSlots(prog *fp.Program, calls []lang.Invocation, sig Signature) []SlotOutcome {
	if prog == nil {
		return nil
	}
	fixed := cellValues(prog)
	var out []SlotOutcome
	for _, g := range prog.Generalizations {
		var instances []*fp.Instance
		for _, inst := range prog.Instances {
			if inst.Of.Text == g.Name {
				instances = append(instances, inst)
			}
		}
		o := SlotOutcome{Generalization: g}
		for _, call := range modelCalls(calls) {
			if call.Name != g.Op.Text {
				continue
			}
			o.Calls++
			o.Violations = append(o.Violations, checkCall(g, call, sig, fixed, instances)...)
		}
		out = append(out, o)
	}
	return out
}

// modelCalls — вызовы, которые написала модель: не изнутри скиллов.
func modelCalls(calls []lang.Invocation) []lang.Invocation {
	byID := make(map[int]lang.Invocation, len(calls))
	for _, c := range calls {
		byID[c.ID] = c
	}
	var out []lang.Invocation
	for _, c := range calls {
		if !slices.ContainsFunc(c.Outer, func(id int) bool { return byID[id].Module != "" }) {
			out = append(out, c)
		}
	}
	return out
}

func checkCall(g *fp.Generalization, call lang.Invocation, sig Signature, fixed map[string]string,
	instances []*fp.Instance) []string {
	where := fmt.Sprintf("%s(%s)", call.Qualified(), strings.Join(call.Args, ", "))
	var params []string
	if sig != nil {
		params = sig(call)
	}
	values := map[string]string{}
	var out []string
	for _, slot := range g.Params {
		value, ok := slotValue(slot.Name, g, call, params)
		if !ok {
			out = append(out, fmt.Sprintf("%s: слот %s не сопоставить с аргументами — назовите параметр операции "+
				"как слот или передавайте аргументы в порядке %s(%s)", where, slot.Name, g.Op.Text, joinNames(g.Args)))
			continue
		}
		values[slot.Name] = value
		if len(slot.Domain) > 0 && !slices.ContainsFunc(slot.Domain, func(d string) bool { return sameValue(value, d) }) {
			out = append(out, fmt.Sprintf("%s: %s = %s вне домена {%s}", where, slot.Name, value,
				strings.Join(slot.Domain, ", ")))
		}
		if r := slot.Range; r != nil {
			if n, unit, ok := fp.ParseNumber(value); !ok || !r.Contains(n, unit) {
				out = append(out, fmt.Sprintf("%s: %s = %s вне диапазона %s", where, slot.Name, value, r.Text))
			}
		}
		if want, ok := fixed[g.Name+"."+slot.Name]; ok && !sameValue(value, want) {
			out = append(out, fmt.Sprintf("%s: %s = %s, а промпт задаёт %s.%s = %s", where, slot.Name, value,
				g.Name, slot.Name, want))
		}
	}
	if len(instances) > 0 && len(out) == 0 && !matchesInstance(values, instances) {
		names := make([]string, len(instances))
		for i, inst := range instances {
			names[i] = inst.Name
		}
		out = append(out, fmt.Sprintf("%s: не совпадает ни с одним частным случаем промпта (%s)", where,
			strings.Join(names, ", ")))
	}
	return out
}

// slotValue — значение слота в вызове: по имени параметра алиаса, иначе по
// месту в op(…), если число аргументов то же.
func slotValue(slot string, g *fp.Generalization, call lang.Invocation, params []string) (string, bool) {
	if i := slices.Index(params, slot); i >= 0 && i < len(call.Args) {
		return strings.TrimSpace(call.Args[i]), true
	}
	if len(call.Args) != len(g.Args) {
		return "", false
	}
	for i, arg := range g.Args {
		if arg.Text == slot {
			return strings.TrimSpace(call.Args[i]), true
		}
	}
	return "", false
}

func matchesInstance(values map[string]string, instances []*fp.Instance) bool {
	for _, inst := range instances {
		ok := true
		for _, b := range inst.Bindings {
			if !sameValue(values[b.Slot], b.Value) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// cellValues — значения слотов, заданные ячейками: set g.x = v.
func cellValues(prog *fp.Program) map[string]string {
	out := map[string]string{}
	for _, cell := range prog.Cells {
		for _, set := range cell.Sets {
			if strings.Count(set.Target, ".") == 1 {
				out[set.Target] = set.Value.Text
			}
		}
	}
	return out
}

// sameValue — значение из плана то же, что имя в промпте. Как и цели
// правил, имена сравниваются без регистра и знаков: файл prod.conf — это
// prod_conf, 30km/h — 30 km/h. Путь сравнивается и по базовому имени:
// /srv/prod.conf — тоже prod_conf.
func sameValue(value, name string) bool {
	want := squash(name)
	if want == "" {
		return value == name
	}
	return squash(value) == want || squash(filepath.Base(value)) == want
}

func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

func joinNames(names []fp.Name) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n.Text
	}
	return strings.Join(out, ", ")
}
