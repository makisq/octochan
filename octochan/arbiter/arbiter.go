// Package arbiter проверяет план сценария по жёстким правилам формального
// промпта — до выполнения.
//
// Правило — сигнал модели и предикат над планом (раздел 4.4 манифеста).
// Сигнал модель получает из промпта, а предикат проверяет здесь
// детерминированный разбор: проверка после того, как действие ушло на
// серверы, — запись в журнале, а не защита.
//
// План — это трасса выполнения сценария в режиме плана: какие файлы он
// записал и какие алиасы вызвал. Действие правила — встроенная операция
// записи (write, save) или имя алиаса. Исключения и область правила
// сравниваются с именами целей: файл prod.conf — это prod_conf.
package arbiter

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"octochan/fp"
	"octochan/lang"
)

// Builtins — встроенные действия, которые видно в плане напрямую.
var Builtins = []string{"save", "write"}

// Outcome — итог проверки одного правила.
type Outcome struct {
	Rule *fp.Rule
	// Violations пусто, если правило соблюдено.
	Violations []string
}

// OK сообщает, что правило соблюдено.
func (o Outcome) OK() bool { return len(o.Violations) == 0 }

// Describe — правило в той записи, что в промпте.
func Describe(rule *fp.Rule) string {
	var args []string
	for _, arg := range rule.Args {
		args = append(args, fmt.Sprintf("%s = {%s}", arg.Name, strings.Join(arg.Values, ", ")))
	}
	return fmt.Sprintf("hard %s %s(%s)", rule.Kind, rule.Action, strings.Join(args, ", "))
}

// Hint — законный путь для нарушенного правила: что правило допускает.
// Правило задаёт оператор, и подсказка не предлагает его менять: только
// как написать сценарий, который его соблюдает.
func Hint(rule *fp.Rule) string {
	except := values(rule, "except")
	scope := values(rule, "scope")
	where := ""
	if len(scope) > 0 {
		where = " в области {" + strings.Join(scope, ", ") + "}"
	}
	const ref = " (протокол, «Hard rules»)"
	switch rule.Kind {
	case fp.Forbid:
		if len(except) > 0 {
			return fmt.Sprintf("%s%s допустимо только для исключений except = {%s}: пишите только в эти цели "+
				"(файл prod.conf — цель prod_conf)%s", rule.Action, where, strings.Join(except, ", "), ref)
		}
		return fmt.Sprintf("%s%s запрещено без исключений: сценарий не должен его вызывать%s", rule.Action, where, ref)
	case fp.Require:
		if len(scope) > 0 {
			return fmt.Sprintf("вызовите %s для каждой цели области: %s — записью или аргументом%s",
				rule.Action, strings.Join(scope, ", "), ref)
		}
		return fmt.Sprintf("сценарий обязан вызвать %s хотя бы раз%s", rule.Action, ref)
	}
	return ""
}

// Check проверяет правила по трассе плана.
func Check(rules []*fp.Rule, effects []lang.Effect, calls []lang.Invocation) []Outcome {
	t := newTrace(effects, calls)
	outcomes := make([]Outcome, 0, len(rules))
	for _, rule := range rules {
		var violations []string
		switch rule.Kind {
		case fp.Forbid:
			violations = t.forbid(rule)
		case fp.Require:
			violations = t.require(rule)
		}
		outcomes = append(outcomes, Outcome{Rule: rule, Violations: violations})
	}
	return outcomes
}

// TargetName — имя цели в языке промпта: базовое имя файла, где всё,
// кроме букв и цифр, стало «_», в нижнем регистре. prod.conf — prod_conf,
// /srv/web-1.conf — web_1_conf.
func TargetName(path string) string {
	base := filepath.Base(path)
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return '_'
	}, base)
}

type trace struct {
	effects []lang.Effect
	calls   []lang.Invocation
	// written[ID вызова] — имена целей, записанных внутри вызова.
	written map[int][]string
}

func newTrace(effects []lang.Effect, calls []lang.Invocation) *trace {
	t := &trace{effects: effects, calls: calls, written: map[int][]string{}}
	for _, e := range effects {
		if e.Kind == lang.EffectLive {
			continue
		}
		for _, id := range e.Calls {
			t.written[id] = append(t.written[id], TargetName(e.Path))
		}
	}
	return t
}

func values(rule *fp.Rule, name string) []string {
	var out []string
	for _, arg := range rule.Args {
		if arg.Name == name {
			out = append(out, arg.Values...)
		}
	}
	return out
}

func isBuiltin(action string) bool {
	for _, b := range Builtins {
		if b == action {
			return true
		}
	}
	return false
}

func effectKind(action string) lang.EffectKind {
	if action == "save" {
		return lang.EffectSave
	}
	return lang.EffectWrite
}

func within(name string, set []string) bool {
	for _, v := range set {
		if v == name {
			return true
		}
	}
	return false
}

// forbid — «никогда X, кроме исключений». Исключение оправдывает только
// то, что действие записало: вызов, который ничего не записал, оправдать
// нечем, и он нарушение — правило абсолютное (Ж2).
func (t *trace) forbid(rule *fp.Rule) []string {
	except := values(rule, "except")
	scope := values(rule, "scope")
	inScope := func(names []string) bool {
		if len(scope) == 0 {
			return true
		}
		for _, n := range names {
			if within(n, scope) {
				return true
			}
		}
		return false
	}

	var violations []string
	if isBuiltin(rule.Action) {
		for _, e := range t.effects {
			if e.Kind != effectKind(rule.Action) {
				continue
			}
			name := TargetName(e.Path)
			if inScope([]string{name}) && !within(name, except) {
				violations = append(violations, fmt.Sprintf("%s в %s (%s)", rule.Action, e.Path, name))
			}
		}
		return violations
	}

	for _, call := range t.calls {
		if call.Name != rule.Action {
			continue
		}
		targets := t.written[call.ID]
		if !inScope(append(argNames(call), targets...)) {
			continue
		}
		switch {
		case len(except) == 0:
			violations = append(violations, fmt.Sprintf("вызов %s(%s)", call.Qualified(), strings.Join(call.Args, ", ")))
		case len(targets) == 0:
			violations = append(violations, fmt.Sprintf("вызов %s(%s): ничего не записал, исключение применить не к чему",
				call.Qualified(), strings.Join(call.Args, ", ")))
		default:
			var outside []string
			for _, name := range dedupe(targets) {
				if !within(name, except) {
					outside = append(outside, name)
				}
			}
			if len(outside) > 0 {
				violations = append(violations, fmt.Sprintf("вызов %s(%s) пишет вне исключений: %s",
					call.Qualified(), strings.Join(call.Args, ", "), strings.Join(outside, ", ")))
			}
		}
	}
	return violations
}

// require — «всегда X на области». Область покрыта, если X записал эту
// цель или получил её аргументом: проверка вроде verify_config ничего не
// пишет, но вызывается для цели.
func (t *trace) require(rule *fp.Rule) []string {
	scope := values(rule, "scope")

	covered := map[string]bool{}
	happened := false
	if isBuiltin(rule.Action) {
		for _, e := range t.effects {
			if e.Kind == effectKind(rule.Action) {
				happened = true
				covered[TargetName(e.Path)] = true
			}
		}
	} else {
		for _, call := range t.calls {
			if call.Name != rule.Action {
				continue
			}
			happened = true
			for _, name := range append(argNames(call), t.written[call.ID]...) {
				covered[name] = true
			}
		}
	}

	if len(scope) == 0 {
		if !happened {
			return []string{fmt.Sprintf("в плане нет %s", rule.Action)}
		}
		return nil
	}
	var missing []string
	for _, s := range scope {
		if !covered[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%s не выполняется для: %s", rule.Action, strings.Join(missing, ", "))}
}

// argNames — аргументы вызова как имена языка промпта.
func argNames(call lang.Invocation) []string {
	var names []string
	for _, arg := range call.Args {
		if arg != "" && !strings.ContainsAny(arg, "\n") {
			names = append(names, TargetName(arg))
		}
	}
	return names
}

func dedupe(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}
