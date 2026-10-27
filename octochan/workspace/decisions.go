package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"octochan/rings"
)

// Вердикты арбитра и оператора в журнале решений.
const (
	ArbiterPassed   = "passed"
	ArbiterRejected = "rejected"

	OperatorApproved   = "approved"
	OperatorRejected   = "rejected"
	OperatorRolledBack = "rolled_back"
)

// Decision — запись журнала решений: план, построенный по формальному
// промпту, что о каждом его утверждении решили кольца, что сказал арбитр и
// что потом сделал оператор (раздел 7 манифеста ФПм).
//
// Журнал — единственный источник чисел колец: надёжность правила — доля
// планов, где оно использовалось и которые прошли арбитра без правок
// оператора.
type Decision struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// Mode — режим колец: protocol или embedded. Гарантии в них разные.
	Mode string `json:"mode"`
	// Purpose — зачем строился план: plan (показать) или request (заявка).
	Purpose string `json:"purpose"`
	// Prompt и PromptSum — промпт и сумма его текста: одна задача —
	// одна сумма. ScriptSum — сумма сценария: один план — одна пара сумм.
	Prompt    string `json:"prompt"`
	PromptSum string `json:"prompt_sum"`
	Script    string `json:"script"`
	ScriptSum string `json:"script_sum"`
	// WellTrue — порог кольца 2, с которым принималось решение.
	WellTrue float64           `json:"well_true"`
	Packages []DecisionPackage `json:"packages"`
	// Arbiter — вердикт арбитра по плану целиком; он смотрит план и
	// тогда, когда кольца уже отвергли утверждения.
	Arbiter        string   `json:"arbiter,omitempty"`
	ArbiterReasons []string `json:"arbiter_reasons,omitempty"`
	// Operator — решение оператора по заявке на этот план.
	Operator string `json:"operator,omitempty"`
	Request  string `json:"request,omitempty"`
	// Role — роль, которая построила план: llm — это модель. Нарушение
	// запрета ложится на неё и на промпт (Blame), а не на правила скиллов.
	Role string `json:"role,omitempty"`
}

// DecisionPackage — утверждение плана и то, что о нём решили кольца.
// Сырой счёт — интервал поддержки — хранится рядом с бинарным решением
// кольца 2: по нему разбирают инциденты.
type DecisionPackage struct {
	Statement string   `json:"statement"`
	Rules     []string `json:"rules"`
	L         float64  `json:"l"`
	U         float64  `json:"u"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason,omitempty"`
	// Trusted — пакет не прошёл кольцо 2, и оператор принял его сам.
	Trusted bool `json:"trusted,omitempty"`
}

// Outcome — исход плана целиком: верен ли он и вынесен ли вердикт вообще.
// По нему калибруется поддержка плана; на правила его раскладывает
// PackageOutcome.
//
// Верный план — прошедший арбитра и не исправленный оператором. Отказ
// кольца 2 исходом не считается: он зависит от самой оценки, и журнал
// подтверждал бы сам себя — слабое правило отвергнуто, отказ засчитан как
// неудача, правило ещё слабее. Исход должен быть слепым к оценке (раздел
// 7.3): арбитр и кольцо 3 — булевы проверки и оценок не видят, оператор
// их по умолчанию тоже не видит.
//
// Успех засчитывает только оператор: план, который никто не согласовывал,
// верным не назван, и модель не накрутит надёжность, показывая планы.
func (d *Decision) Outcome() (correct, decided bool) {
	switch {
	case d.Arbiter == ArbiterRejected, d.contradiction():
		return false, true
	case d.Operator == OperatorRejected, d.Operator == OperatorRolledBack:
		return false, true
	case d.Operator == OperatorApproved:
		return true, true
	}
	return false, false
}

func (d *Decision) contradiction() bool {
	for _, p := range d.Packages {
		if p.Verdict == string(rings.Contradiction) {
			return true
		}
	}
	return false
}

// requireFailed — среди нарушенных арбитру правил есть «всегда».
// Нарушенные правила хранятся в записи промпта: hard forbid … и
// hard require ….
func (d *Decision) requireFailed() bool {
	if len(d.ArbiterReasons) == 0 {
		// Причина не записана: неизвестно, что нарушено, — как прежде,
		// на все утверждения.
		return true
	}
	for _, reason := range d.ArbiterReasons {
		if !strings.HasPrefix(reason, "hard forbid ") {
			return true
		}
	}
	return false
}

// Violated — план нарушил запрет промпта: кольцо 3 нашло противоречие
// или арбитр отверг план за «никогда».
func (d *Decision) Violated() bool {
	if d.contradiction() {
		return true
	}
	for _, reason := range d.ArbiterReasons {
		if strings.HasPrefix(reason, "hard forbid ") {
			return true
		}
	}
	return false
}

// Blame — нарушения запретов на счёт модели и промпта: сколько планов
// построено и в скольких из них нарушен запрет.
type Blame struct {
	Plans      int
	Violations int
}

// Blames считает нарушения запретов по промптам и по ролям, построившим
// планы. План считается один раз — последняя его запись, как в Decided.
// Ключ промпта — имя и начало суммы текста: одно имя у разных текстов —
// разные задачи.
func Blames(decisions []*Decision) (byPrompt, byRole map[string]Blame) {
	latest := map[string]*Decision{}
	for _, d := range decisions {
		key := d.PromptSum + "/" + d.ScriptSum
		if prev, ok := latest[key]; !ok || !d.Time.Before(prev.Time) {
			latest[key] = d
		}
	}
	byPrompt, byRole = map[string]Blame{}, map[string]Blame{}
	add := func(m map[string]Blame, key string, violated bool) {
		b := m[key]
		b.Plans++
		if violated {
			b.Violations++
		}
		m[key] = b
	}
	for _, d := range latest {
		violated := d.Violated()
		sum := d.PromptSum
		if len(sum) > 8 {
			sum = sum[:8]
		}
		add(byPrompt, d.Prompt+" ("+sum+")", violated)
		role := d.Role
		if role == "" {
			role = "—"
		}
		add(byRole, role, violated)
	}
	return byPrompt, byRole
}

// Rejected сообщает, что план не прошёл: кольца или арбитр.
func (d *Decision) Rejected() bool {
	if d.Arbiter == ArbiterRejected {
		return true
	}
	for _, p := range d.Packages {
		if p.Verdict != string(rings.WellTrue) {
			return true
		}
	}
	return false
}

// Reasons — почему план не прошёл, одной строкой: по ней видно, что модель
// второй раз упёрлась в одно и то же.
func (d *Decision) Reasons() string {
	var parts []string
	for _, p := range d.Packages {
		if p.Verdict != string(rings.WellTrue) {
			parts = append(parts, p.Statement+" — "+p.Verdict+" "+p.Reason)
		}
	}
	parts = append(parts, d.ArbiterReasons...)
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// Counts — сколько планов с правилом получили вердикт и сколько из них
// оказались верными.
type Counts struct {
	Uses      int
	Successes int
}

// Decided — планы с исходом, каждый по последнему вердикту, от старых к
// новым. Один и тот же план (промпт и сценарий те же) считается один раз:
// модель, которая показывает план снова и снова, не должна умножать его
// исход.
func Decided(decisions []*Decision) []*Decision {
	latest := map[string]*Decision{}
	for _, d := range decisions {
		if _, decided := d.Outcome(); !decided {
			continue
		}
		key := d.PromptSum + "/" + d.ScriptSum
		if prev, ok := latest[key]; !ok || !d.Time.Before(prev.Time) {
			latest[key] = d
		}
	}
	out := make([]*Decision, 0, len(latest))
	for _, d := range latest {
		out = append(out, d)
	}
	sortDecisions(out)
	return out
}

// PackageOutcome — исход утверждения плана для правил его скиллов.
//
// Нарушение запрета — противоречие кольца 3 или отказ арбитра по «никогда» —
// говорит о том, что модель применила операцию там, где промпт её
// запретил, а не о том, что операция ненадёжна. Оно ложится на модель и
// промпт (Blame), а правилам скиллов исходом не считается: иначе модель,
// показывая планы с нарушениями, опускала бы надёжность нужного оператору
// скилла ниже порога. Правила соседних утверждений план с чужой ошибкой
// тоже не позорит — их план просто не дошёл до суда.
//
// Неудача плана, которую к утверждению не привязать, — нарушенное
// «всегда», отказ или откат оператора — ложится на все утверждения,
// прошедшие кольца. Отказ кольца 2 — не исход.
func (d *Decision) PackageOutcome(i int) (correct, decided bool) {
	switch p := d.Packages[i]; {
	case p.Verdict != string(rings.WellTrue), d.contradiction():
		return false, false
	case d.Arbiter == ArbiterRejected && !d.requireFailed():
		// Арбитр отверг план только за запреты: это на счёт модели.
		return false, false
	case d.Arbiter == ArbiterRejected:
		return false, true
	}
	switch d.Operator {
	case OperatorRejected, OperatorRolledBack:
		return false, true
	case OperatorApproved:
		return true, true
	}
	return false, false
}

// RuleCounts считает надёжность правил по журналу: по планам из Decided.
// Правило в плане — одно применение: неверное, если неверно хоть одно его
// утверждение, верное, если верны все, у которых есть исход.
func RuleCounts(decisions []*Decision) map[string]Counts {
	counts := map[string]Counts{}
	for _, d := range Decided(decisions) {
		outcome := map[string]bool{}
		for i, p := range d.Packages {
			correct, decided := d.PackageOutcome(i)
			if !decided {
				continue
			}
			for _, rule := range p.Rules {
				if prev, ok := outcome[rule]; !ok || prev {
					outcome[rule] = correct
				}
			}
		}
		for rule, correct := range outcome {
			c := counts[rule]
			c.Uses++
			if correct {
				c.Successes++
			}
			counts[rule] = c
		}
	}
	return counts
}

// Decisions — журнал решений на диске: запись — файл.
type Decisions struct {
	dir string
}

// NewDecisions открывает журнал решений.
func NewDecisions(dir string) (*Decisions, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("не удалось создать журнал решений: %w", err)
	}
	return &Decisions{dir: dir}, nil
}

func (j *Decisions) path(id string) string { return filepath.Join(j.dir, id+".json") }

// Append добавляет запись, проставляя идентификатор и время.
func (j *Decisions) Append(d *Decision) error {
	if d.ID == "" {
		id, err := generateID()
		if err != nil {
			return err
		}
		d.ID = id
	}
	if d.Time.IsZero() {
		d.Time = time.Now().UTC()
	}
	return j.save(d)
}

func (j *Decisions) save(d *Decision) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("не удалось сериализовать запись журнала решений: %w", err)
	}
	if err := os.WriteFile(j.path(d.ID), data, filePerm); err != nil {
		return fmt.Errorf("не удалось записать журнал решений: %w", err)
	}
	return nil
}

// Get читает запись.
func (j *Decisions) Get(id string) (*Decision, error) {
	data, err := os.ReadFile(j.path(id))
	if err != nil {
		return nil, fmt.Errorf("запись журнала решений %s: %w", id, err)
	}
	var d Decision
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("запись журнала решений %s испорчена: %w", id, err)
	}
	return &d, nil
}

// SetOperator записывает решение оператора по заявке на план.
func (j *Decisions) SetOperator(id, verdict, request string) error {
	d, err := j.Get(id)
	if err != nil {
		return err
	}
	d.Operator = verdict
	if request != "" {
		d.Request = request
	}
	return j.save(d)
}

// All возвращает все записи от старых к новым.
func (j *Decisions) All() ([]*Decision, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("не удалось прочитать журнал решений: %w", err)
	}
	var out []*Decision
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		d, err := j.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sortDecisions(out)
	return out, nil
}

// sortDecisions — от старых к новым. Порядок полный: времени мало, записи
// в одну миллисекунду бывают.
func sortDecisions(out []*Decision) {
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Time.Equal(out[b].Time) {
			return out[a].Time.Before(out[b].Time)
		}
		return out[a].ID < out[b].ID
	})
}

// ForPrompt — записи одной задачи (одного промпта) от старых к новым.
func ForPrompt(decisions []*Decision, promptSum string) []*Decision {
	var out []*Decision
	for _, d := range decisions {
		if d.PromptSum == promptSum {
			out = append(out, d)
		}
	}
	return out
}
