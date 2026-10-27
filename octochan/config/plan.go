package config

import (
	"fmt"
	"sort"
	"strings"
)

// ChangeKind — вид изменения.
type ChangeKind string

const (
	// ChangeSet — значение параметра меняется.
	ChangeSet ChangeKind = "set"
	// ChangeAdd — параметр появляется.
	ChangeAdd ChangeKind = "add"
	// ChangeRemove — параметр исчезает.
	ChangeRemove ChangeKind = "remove"
	// ChangeTask — будет создана задача во внешней системе.
	ChangeTask ChangeKind = "task"
	// ChangeWrite — файл будет записан целиком; Detail говорит, как
	// именно. Для правки конфига план перечисляет параметры, а запись
	// произвольного текста по параметрам не разложить.
	ChangeWrite ChangeKind = "write"
)

func (k ChangeKind) Russian() string {
	switch k {
	case ChangeSet:
		return "изменить"
	case ChangeAdd:
		return "добавить"
	case ChangeRemove:
		return "удалить"
	case ChangeTask:
		return "задача"
	case ChangeWrite:
		return "записать"
	default:
		return string(k)
	}
}

// Change — одно изменение в плане или в записи истории.
type Change struct {
	Kind ChangeKind `json:"kind"`
	// Target — сервер или файл, к которому относится изменение.
	Target string `json:"target,omitempty"`
	Name   string `json:"name"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Plan — описание того, что произойдёт, до того как это произошло.
//
// План и запись истории намеренно используют один набор изменений: это одно
// и то же описание, снятое до и после. Иначе «что собирались сделать»
// и «что сделали» пришлось бы сверять глазами.
type Plan struct {
	Command string   `json:"command"`
	Source  string   `json:"source,omitempty"`
	Target  string   `json:"target,omitempty"`
	Changes []Change `json:"changes"`
	// Warnings — то, что должно попасться на глаза до выполнения:
	// параметры, ждущие перезапуска, неполнота плана и тому подобное.
	Warnings []string `json:"warnings,omitempty"`
	// Destructive помечает план, выполнение которого прерывает обслуживание.
	Destructive bool `json:"destructive,omitempty"`
}

// Len возвращает количество изменений.
func (p *Plan) Len() int {
	if p == nil {
		return 0
	}
	return len(p.Changes)
}

// Add добавляет изменение в план.
func (p *Plan) Add(c Change) { p.Changes = append(p.Changes, c) }

// Warn добавляет предупреждение.
func (p *Plan) Warn(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

// Sort упорядочивает изменения: сначала по цели, затем по имени.
// Порядок должен быть устойчивым, иначе два запуска дают разные планы
// и их нельзя сравнить глазами.
func (p *Plan) Sort() {
	sort.SliceStable(p.Changes, func(i, j int) bool {
		if p.Changes[i].Target != p.Changes[j].Target {
			return p.Changes[i].Target < p.Changes[j].Target
		}
		return p.Changes[i].Name < p.Changes[j].Name
	})
}

// PlanFromDiff строит план приведения левой стороны к правой.
//
// Именно это делает patch: значения правого источника переносятся в левый.
// В план попадают только настоящие изменения, а не все параметры подряд.
func PlanFromDiff(diff *Diff, target string) *Plan {
	plan := &Plan{Target: target}
	if diff == nil {
		return plan
	}

	for _, entry := range diff.Entries {
		// Параметр, которого нет справа, patch не трогает: он переносит
		// значения, а не удаляет их.
		if !entry.RightExists {
			continue
		}

		kind := ChangeSet
		if !entry.LeftExists {
			kind = ChangeAdd
		}

		plan.Add(Change{
			Kind:   kind,
			Target: target,
			Name:   entry.Param(),
			From:   entry.Left,
			To:     entry.Right,
		})
	}

	plan.Sort()
	return plan
}

// Render печатает план в читаемом виде.
func (p *Plan) Render(width int) string {
	var b strings.Builder

	if p.Source != "" || p.Target != "" {
		fmt.Fprintf(&b, "План: %s -> %s\n", orDash(p.Source), orDash(p.Target))
	}

	for _, warning := range p.Warnings {
		fmt.Fprintf(&b, "внимание: %s\n", warning)
	}
	if p.Destructive {
		b.WriteString("ВНИМАНИЕ: план прерывает обслуживание\n")
	}

	if len(p.Changes) == 0 {
		b.WriteString("Изменений нет.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\nИзменений: %d\n", len(p.Changes))

	currentTarget := "\x00"
	for _, change := range p.Changes {
		if change.Target != currentTarget {
			currentTarget = change.Target
			if currentTarget != "" {
				fmt.Fprintf(&b, "\n  %s\n", currentTarget)
			}
		}

		switch change.Kind {
		case ChangeTask, ChangeWrite:
			fmt.Fprintf(&b, "    %-10s %s", change.Kind.Russian(), change.Name)
			if change.Detail != "" {
				fmt.Fprintf(&b, " — %s", change.Detail)
			}
			b.WriteByte('\n')
		case ChangeAdd:
			fmt.Fprintf(&b, "    %-10s %s = %s\n",
				change.Kind.Russian(), truncateRunes(change.Name, width), change.To)
		case ChangeRemove:
			fmt.Fprintf(&b, "    %-10s %s (было %s)\n",
				change.Kind.Russian(), truncateRunes(change.Name, width), change.From)
		default:
			fmt.Fprintf(&b, "    %-10s %s: %s -> %s\n",
				change.Kind.Russian(), truncateRunes(change.Name, width), change.From, change.To)
		}
	}

	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ApplyPlan переносит изменения плана в конфиг.
func ApplyPlan(cfg ConfigMap, plan *Plan) ConfigMap {
	if cfg == nil {
		cfg = make(ConfigMap)
	}

	for _, change := range plan.Changes {
		section, key := splitParamName(change.Name)

		switch change.Kind {
		case ChangeRemove:
			delete(cfg[section], key)
		case ChangeSet, ChangeAdd:
			if _, ok := cfg[section]; !ok {
				cfg[section] = make(map[string]string)
			}
			cfg[section][key] = change.To
		}
	}
	return cfg
}

// splitParamName разбирает "секция.ключ"; имя без точки относится
// к параметрам без секции.
// ParamValue — значение параметра по имени из плана: «секция.ключ» или
// «ключ» без секции. Так же имя разбирает ApplyPlan.
func ParamValue(cfg ConfigMap, name string) (string, bool) {
	section, key := splitParamName(name)
	value, ok := cfg[section][key]
	return value, ok
}

func splitParamName(name string) (section, key string) {
	if idx := strings.LastIndex(name, "."); idx > 0 {
		return name[:idx], name[idx+1:]
	}
	return "", name
}

// DiffPlans сравнивает согласованный план с построенным заново: что в нём
// пропало, что появилось и изменилось ли то, что прерывает обслуживание.
// Пустой ответ — планы совпадают. Сравниваются изменения, а не заголовок
// и предупреждения: выполняется то, что в изменениях.
func DiffPlans(approved, now *Plan) []string {
	count := func(p *Plan) map[Change]int {
		m := map[Change]int{}
		if p != nil {
			for _, c := range p.Changes {
				m[c]++
			}
		}
		return m
	}
	was, is := count(approved), count(now)

	var diffs []string
	for _, c := range sortedChanges(was) {
		if extra := was[c] - is[c]; extra > 0 {
			diffs = append(diffs, "пропало: "+c.describe())
		}
	}
	for _, c := range sortedChanges(is) {
		if extra := is[c] - was[c]; extra > 0 {
			diffs = append(diffs, "появилось: "+c.describe())
		}
	}
	if approved != nil && now != nil && approved.Destructive != now.Destructive {
		diffs = append(diffs, fmt.Sprintf("прерывает обслуживание: было %v, стало %v", approved.Destructive, now.Destructive))
	}
	return diffs
}

func sortedChanges(m map[Change]int) []Change {
	out := make([]Change, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].describe() < out[j].describe() })
	return out
}

// describe — изменение одной строкой для сообщений о расхождении.
func (c Change) describe() string {
	text := c.Kind.Russian()
	if c.Target != "" {
		text += " " + c.Target + ":"
	}
	text += " " + c.Name
	if c.From != "" || c.To != "" {
		text += fmt.Sprintf(" %s -> %s", c.From, c.To)
	}
	if c.Detail != "" {
		text += " (" + c.Detail + ")"
	}
	return text
}
