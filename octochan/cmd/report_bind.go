package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"octochan/arbiter"
	"octochan/config"
	"octochan/fp"
	"octochan/lang"

	"github.com/spf13/cobra"
)

// Отчёт модели и план.
//
// Отчёт — единственное окно в решения модели, которые промпт оставил
// открытыми (раздел 6.5 манифеста). Пока его сверяли только с промптом,
// модель могла написать в отчёте одно, а в сценарии сделать другое или не
// сказать о решении вовсе. Здесь отчёт сверяется с планом.
//
// Решение модели — значение параметра конфига, которое план записывает и
// которое зависит от того, что модель вписала в сценарий: оно совпадает с
// литералом сценария или меняется, когда числа сценария сдвинуты
// (lang/literals.go). Значение из предпосылок — файла, живого источника,
// инвентаря — решением модели не является. Значение, которое задаёт ячейка
// промпта, — решение оператора, и план обязан его держать.
//
// Каждое решение модели — в отчёте: вопрос — цель и параметр через точку
// (prod_conf.max_connections) или параметр (max_connections), выбор —
// записанное значение. Решение без отчёта и отчёт, который расходится с
// планом, — нарушение: план не проходит.
//
// Значения слотов обобщений решениями сверх CheckSlots не считаются:
// домен, диапазон и частные случаи — граница, которую задал оператор. Но
// если отчёт называет слот, его выбор обязан совпасть с планом.

// planValue — значение параметра, которое план записывает.
type planValue struct {
	key, target, attr, value string
}

// planValues — параметры конфигов, которые план добавляет или меняет.
func planValues(name string, effects []lang.Effect) map[string]planValue {
	out := map[string]planValue{}
	for _, e := range effects {
		if !e.Writes() {
			continue
		}
		for _, c := range lang.BuildPlan(name, []lang.Effect{e}).Changes {
			if c.Kind != config.ChangeAdd && c.Kind != config.ChangeSet {
				continue
			}
			target := arbiter.TargetName(c.Target)
			key := target + "." + c.Name
			out[key] = planValue{key: key, target: target, attr: c.Name, value: c.To}
		}
	}
	return out
}

// slotBounded — значение прошло через слот обобщения: вызов его операции
// передал это значение, а если слоты вызова называют какой-то параметр,
// который пишет план, — то именно этот. Границу такого значения задал
// оператор, и отдельным решением модели оно не считается.
func slotBounded(v planValue, slots []arbiter.SlotCall, written map[string]planValue) bool {
	attrs := map[string]bool{}
	for _, w := range written {
		attrs[w.attr] = true
	}
	byCall := map[int][]arbiter.SlotCall{}
	for _, s := range slots {
		byCall[s.CallID] = append(byCall[s.CallID], s)
	}
	for _, call := range byCall {
		value, named, namesAny := false, false, false
		for _, s := range call {
			value = value || arbiter.SameValue(v.value, s.Value)
			named = named || s.Value == v.attr
			namesAny = namesAny || attrs[s.Value]
		}
		if value && (named || !namesAny) {
			return true
		}
	}
	return false
}

// bindReport сверяет отчёт модели с планом; печатает итог и возвращает
// нарушения. main — трасса основного плана с литералами сценария.
func bindReport(cmd *cobra.Command, registry *lang.Registry, file *lang.File, setup scriptSetup, prompt *fp.Result,
	main scriptTrace) []string {
	if prompt == nil {
		return nil
	}
	prog := prompt.Program
	sig := func(call lang.Invocation) []string {
		decl, err := registry.Lookup(call.Module, call.Name)
		if err != nil {
			return nil
		}
		return decl.Params
	}
	slots := arbiter.CallSlots(prog, main.calls, sig)
	values := planValues(file.Name, main.effects)

	// Второй план — с числами сценария, сдвинутыми на единицу.
	shifted := setup
	shifted.watch, shifted.perturb = true, true
	trace, shiftErr := traceScript(cmd, registry, file, shifted, true, io.Discard)
	moved := planValues(file.Name, trace.effects)

	cells := map[string]string{}
	for _, cell := range prog.Cells {
		for _, set := range cell.Sets {
			cells[set.Target] = set.Value.Text
		}
	}
	report := map[string]*fp.ReportEntry{}
	for _, e := range prog.Report {
		report[e.Question] = e
	}

	var lines, violations []string
	add := func(line string, violation bool) {
		lines = append(lines, line)
		if violation {
			violations = append(violations, strings.TrimSpace(line))
		}
	}
	if shiftErr != nil {
		add(fmt.Sprintf("  НЕ ПРОВЕРЕНО  план со сдвинутыми числами сценария прервался (%v): какие значения "+
			"выбрала модель, не установить — каждое записанное значение должно быть в отчёте или в промпте", shiftErr), false)
	}
	used := map[string]bool{}
	for _, key := range sortedKeys(values) {
		v := values[key]
		if fixed, ok := cellFor(cells, v); ok {
			if arbiter.SameValue(v.value, fixed) {
				continue
			}
			add(fmt.Sprintf("  РАСХОДИТСЯ  %s = %s, а промпт задаёт %s", key, v.value, fixed), true)
			continue
		}
		why := ""
		switch {
		case slotBounded(v, slots, values):
		case shiftErr != nil:
			why = "происхождение не проверено"
		case literalOf(v.value, main.literals):
			why = "литерал сценария"
		case moved[key].value != v.value:
			why = "зависит от чисел сценария"
		}
		entry, q := reportFor(report, v)
		if entry != nil {
			used[q] = true
			if entry.Choice != nil && !arbiter.SameValue(v.value, entry.Choice.Text) {
				add(fmt.Sprintf("  РАСХОДИТСЯ  отчёт: %s = %s, а план пишет %s = %s", q, entry.Choice.Text, key, v.value), true)
				continue
			}
			if why != "" {
				add(fmt.Sprintf("  в отчёте    %s = %s (%s)", key, v.value, why), false)
			}
			continue
		}
		if why != "" {
			add(fmt.Sprintf("  НЕТ В ОТЧЁТЕ %s = %s — %s: решение модели; в отчёт — q: %s choice: %s, "+
				"или оператор задаёт значение ячейкой промпта", key, v.value, why, key, v.value), true)
		}
	}

	// Слоты, которые называет отчёт, — как в плане.
	for _, s := range slots {
		for _, q := range []string{s.Slot, s.Generalization + "." + s.Slot, s.Generalization + "_" + s.Slot} {
			entry, ok := report[q]
			if !ok || entry.Choice == nil {
				continue
			}
			used[q] = true
			if !arbiter.SameValue(s.Value, entry.Choice.Text) {
				add(fmt.Sprintf("  РАСХОДИТСЯ  отчёт: %s = %s, а план передаёт %s.%s = %s", q, entry.Choice.Text,
					s.Generalization, s.Slot, s.Value), true)
			}
		}
	}

	// Отчёт об объекте плана, которого план не меняет, — выдуманное решение.
	for _, e := range prog.Report {
		if strings.Contains(e.Question, ".") && !used[e.Question] {
			if isSlotRef(prog, e.Question) {
				continue
			}
			add(fmt.Sprintf("  РАСХОДИТСЯ  отчёт говорит о %s, а план его не записывает", e.Question), true)
		}
	}

	if len(lines) > 0 {
		cmd.Println("\nОтчёт модели и план:")
		for _, l := range lines {
			cmd.Println(l)
		}
	}
	return violations
}

// cellFor — значение, которое ячейка промпта задаёт параметру: по цели и
// параметру, по классу и параметру или по одному параметру.
func cellFor(cells map[string]string, v planValue) (string, bool) {
	if value, ok := cells[v.key]; ok {
		return value, true
	}
	for target, value := range cells {
		parts := strings.Split(target, ".")
		if len(parts) == 2 && parts[1] == v.attr && fp.IsClassName(parts[0]) {
			return value, true
		}
	}
	return "", false
}

// reportFor — запись отчёта о значении: по цели и параметру, иначе по
// параметру.
func reportFor(report map[string]*fp.ReportEntry, v planValue) (*fp.ReportEntry, string) {
	if e, ok := report[v.key]; ok {
		return e, v.key
	}
	if e, ok := report[v.attr]; ok {
		return e, v.attr
	}
	return nil, ""
}

// literalOf — значение — литерал сценария.
func literalOf(value string, literals []string) bool {
	for _, l := range literals {
		if l == value || (len(l) > 1 && arbiter.SameValue(value, l)) {
			return true
		}
	}
	return false
}

func isSlotRef(prog *fp.Program, q string) bool {
	g, slot, _ := strings.Cut(q, ".")
	for _, gen := range prog.Generalizations {
		if gen.Name == g {
			for _, p := range gen.Params {
				if p.Name == slot {
					return true
				}
			}
		}
	}
	return false
}

func sortedKeys(m map[string]planValue) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
