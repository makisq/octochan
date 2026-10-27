// Package scenario — описание того, что нужно сделать, и реестр исполнителей.
//
// Сценарий говорит «какой сервис» и «над чем», но ничего не знает о том,
// как это будет выполнено. Исполнителя выбирает реестр по имени сервиса.
//
// Раньше сценарий был неотделим от RLM: команда apply требовала флаг --rlm,
// то есть внешняя система была не одним из способов выполнения, а корнем
// всей операции. Здесь RLM — обычный зарегистрированный исполнитель, рядом
// с которым может встать любой другой.
package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"octochan/config"
	"octochan/inventory"
)

// Target — цель сценария: свободный набор полей.
//
// svm_ci, ci_list и ip — поля, которые понимает RLM; у других исполнителей
// свои: namespace и pod, host, region. Ядро полей не толкует, кроме host и
// group: такая цель — выбор из инвентаря (ResolveTargets), и вместо неё
// встают подходящие хосты со своими полями.
type Target struct {
	SVMCI  string
	CIList []string
	IP     string
	// Fields — остальные поля цели, текстом.
	Fields map[string]string
}

// GetCIs возвращает идентификаторы цели.
func (t *Target) GetCIs() []string {
	if len(t.CIList) > 0 {
		return t.CIList
	}
	if t.SVMCI != "" {
		return []string{t.SVMCI}
	}
	return nil
}

// ID — чем цель называется в плане: svm_ci, иначе host, иначе ip, иначе
// поля через запятую.
func (t *Target) ID() string {
	if cis := t.GetCIs(); len(cis) > 0 {
		return strings.Join(cis, ",")
	}
	if host := t.Fields["host"]; host != "" {
		return host
	}
	if t.IP != "" {
		return t.IP
	}
	keys := make([]string, 0, len(t.Fields))
	for k := range t.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + t.Fields[k]
	}
	return strings.Join(parts, ",")
}

// Labels — поля цели для селектора --limit.
func (t *Target) Labels() inventory.Labels {
	fields := map[string]string{}
	for k, v := range t.Fields {
		fields[k] = v
	}
	if t.SVMCI != "" {
		fields["svm_ci"] = t.SVMCI
	}
	if t.IP != "" {
		fields["ip"] = t.IP
	}
	var groups []string
	if g := t.Fields["groups"]; g != "" {
		groups = strings.Split(g, ",")
	} else if g := t.Fields["group"]; g != "" {
		groups = []string{g}
	}
	return inventory.Labels{Fields: fields, Groups: groups}
}

// MarshalJSON пишет цель плоским объектом: svm_ci, ci_list, ip и прочие
// поля рядом — так её читает исполнитель модуля на любом языке.
func (t Target) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, v := range t.Fields {
		m[k] = v
	}
	if t.SVMCI != "" {
		m["svm_ci"] = t.SVMCI
	}
	if len(t.CIList) > 0 {
		m["ci_list"] = t.CIList
	}
	if t.IP != "" {
		m["ip"] = t.IP
	}
	return json.Marshal(m)
}

// UnmarshalJSON читает цель так же, как её пишет сценарий.
func (t *Target) UnmarshalJSON(data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*t = targetFromMap(m)
	return nil
}

// Scenario — разобранное описание сценария.
type Scenario struct {
	Service    string           `yaml:"service" json:"service"`
	Parameters map[string]any   `yaml:"parameters" json:"parameters"`
	Targets    []Target         `yaml:"targets" json:"targets"`
	Items      []map[string]any `yaml:"items" json:"items"`
	// Source — откуда сценарий прочитан; попадает в план и в журнал.
	Source string `yaml:"-" json:"-"`
}

// NotStarted помечает ошибку, после которой исполнитель ничего не сделал:
// отказал до первого обращения к системе. Заявка тогда остаётся
// согласованной, а не упавшей — упасть может только начатое выполнение.
func NotStarted(err error) error {
	if err == nil {
		return nil
	}
	return notStarted{err}
}

// IsNotStarted сообщает, что выполнение не начиналось.
func IsNotStarted(err error) bool {
	var ns notStarted
	return errors.As(err, &ns)
}

type notStarted struct{ error }

func (e notStarted) Unwrap() error { return e.error }

// Result — что получилось после выполнения.
type Result struct {
	// Refs — следы выполнения: идентификаторы задач, имена файлов и прочее,
	// по чему потом можно найти сделанное во внешней системе.
	Refs []string
	// Summary — короткое описание для журнала.
	Summary string
}

// Runner выполняет сценарии определённого вида.
//
// Plan обязан ничего не менять. Если исполнитель не может построить полный
// план, не обращаясь к внешней системе, он говорит об этом предупреждением
// в плане, а не выполняет запрос втихую.
//
// Apply получает план, на который сказали «да», и обязан применить ровно
// его: не пересчитывать задачу заново и не делать ничего сверх плана.
// Ядро перед вызовом строит план заново и сверяет с согласованным, так что
// исполнитель получает план, который совпадает с его же Plan. То, что
// разошлось бы между сверкой и выполнением, исполнитель обязан найти сам
// и отказать до первого изменения.
type Runner interface {
	// Kind — как называется способ выполнения; показывается пользователю.
	Kind() string
	Plan(ctx context.Context, s *Scenario) (*config.Plan, error)
	Apply(ctx context.Context, s *Scenario, plan *config.Plan) (*Result, error)
}

// Compensator — необязательная возможность исполнителя: обратное действие
// во внешней системе.
//
// Откат файлов (ochan rollback) точный: прежнее содержимое известно. Во
// внешней системе прежнего состояния у ochan нет, и обратное действие —
// отдельная операция со своим планом, которую согласуют так же, как
// прямую: ochan compensate оформляет её заявкой. Если обратного действия
// нет (задача уже отработала, система его не умеет), исполнитель отвечает
// ошибкой с причиной — честный отказ, а не видимость отката.
type Compensator interface {
	// PlanCompensation строит план обратного действия для выполненного
	// плана applied; refs — следы выполнения. Ничего не меняет.
	PlanCompensation(ctx context.Context, s *Scenario, applied *config.Plan, refs []string) (*config.Plan, error)
	// Compensate применяет ровно согласованный план обратного действия.
	Compensate(ctx context.Context, s *Scenario, plan *config.Plan, refs []string) (*Result, error)
}

// Waiter — необязательная возможность исполнителя: дождаться завершения
// того, что он создал.
//
// Реализуют её не все: у RLM задачи имеют опрашиваемый статус, а локальное
// применение завершается сразу и ждать там нечего. Команда apply проверяет
// поддержку и честно говорит, если ожидание невозможно, вместо того чтобы
// делать вид, будто подождала.
type Waiter interface {
	Wait(ctx context.Context, ref string) (string, error)
}

// StatusReporter — необязательная возможность исполнителя: показать
// состояние того, что он создал, — задачи RLM, задания на машине, процесса.
//
// Команда status — общая, ядро её не знает ни про RLM, ни про Linux: она
// спрашивает исполнителя, выбранного флагом, и печатает, что он ответил.
type StatusReporter interface {
	Status(ctx context.Context, ref string) (*Status, error)
}

// Status — состояние одной ссылки исполнителя.
type Status struct {
	Ref   string
	State string
	// Fields — прочие поля по порядку: «Создана», «Прогресс»…
	Fields []StatusField
	// Events — журнал выполнения, если исполнитель его ведёт.
	Events []StatusEvent
}

// StatusField — поле статуса.
type StatusField struct{ Name, Value string }

// StatusEvent — запись журнала выполнения.
type StatusEvent struct{ Time, Level, Message string }

var (
	runnersMu sync.RWMutex
	runners   = make(map[string]Runner)
)

// ExecutorName — имя исполнителя для флагов и выбора: его вид в нижнем
// регистре, пробелы заменены дефисами. RLM → rlm.
func ExecutorName(r Runner) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(r.Kind())), " ", "-")
}

// Executors — зарегистрированные исполнители по имени, каждый один раз:
// у одного исполнителя бывает много сервисов.
func Executors() map[string]Runner {
	runnersMu.RLock()
	defer runnersMu.RUnlock()
	executors := make(map[string]Runner)
	for _, runner := range runners {
		executors[ExecutorName(runner)] = runner
	}
	return executors
}

// Register связывает имя сервиса с исполнителем.
func Register(service string, runner Runner) {
	runnersMu.Lock()
	defer runnersMu.Unlock()
	runners[service] = runner
}

// Unregister убирает сервис из реестра: модуль, который его выполнял,
// выгружен.
func Unregister(service string) {
	runnersMu.Lock()
	defer runnersMu.Unlock()
	delete(runners, service)
}

// moved — сервисы, которые выполнял встроенный исполнитель, а теперь
// приносит модуль: сервис → модуль.
var moved = make(map[string]string)

// Moved объявляет, что сервис теперь приносит модуль: отказ Resolve
// назовёт, что поставить, вместо голого «не зарегистрирован».
func Moved(service, module string) {
	runnersMu.Lock()
	defer runnersMu.Unlock()
	moved[service] = module
}

// Resolve находит исполнителя для сервиса.
func Resolve(service string) (Runner, error) {
	runnersMu.RLock()
	runner, ok := runners[service]
	module, wasBuiltin := moved[service]
	runnersMu.RUnlock()

	if ok {
		return runner, nil
	}
	available := strings.Join(Services(), ", ")
	if available == "" {
		available = "—"
	}
	if wasBuiltin {
		return nil, fmt.Errorf("сервис %q не зарегистрирован: его выполнял встроенный исполнитель, а теперь приносит "+
			"модуль %s — соберите его (plugins/%s) и установите: ochan '!install' %s.hcplugin; доступны: %s",
			service, module, module, module, available)
	}
	return nil, fmt.Errorf("сервис %q не зарегистрирован; доступны: %s. Исполнители приходят модулями: "+
		"ochan '!install' <модуль>.hcplugin, установленные — ochan modules", service, available)
}

// Services перечисляет зарегистрированные сервисы.
func Services() []string {
	runnersMu.RLock()
	defer runnersMu.RUnlock()

	names := make([]string, 0, len(runners))
	for name := range runners {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ErrNotScenario означает, что данные не являются описанием сценария.
//
// Отдельная ошибка нужна команде apply: конфигурационный файл, поданный
// вместо сценария, — это частая ошибка, и сказать о ней надо по существу,
// а не «не удалось разобрать».
var ErrNotScenario = errors.New("это не сценарий: не указано имя сервиса (service)")

// Parse разбирает сценарий в формате JSON или YAML.
func Parse(name string, data []byte) (*Scenario, error) {
	if len(data) == 0 {
		return nil, errors.New("пустые данные сценария")
	}

	// Промежуточная структура нужна из-за того, что targets исторически
	// принимается и списком, и одиночным объектом.
	type rawScenario struct {
		Service    string           `json:"service" yaml:"service"`
		Parameters map[string]any   `json:"parameters" yaml:"parameters"`
		Targets    any              `json:"targets" yaml:"targets"`
		Items      []map[string]any `json:"items" yaml:"items"`
	}

	var raw rawScenario
	jsonErr := json.Unmarshal(data, &raw)
	if jsonErr != nil {
		raw = rawScenario{}
		if yamlErr := yaml.Unmarshal(data, &raw); yamlErr != nil {
			return nil, fmt.Errorf("не удалось разобрать сценарий: как JSON — %v; как YAML — %v", jsonErr, yamlErr)
		}
	}

	if raw.Service == "" {
		return nil, ErrNotScenario
	}

	s := &Scenario{
		Service:    raw.Service,
		Parameters: raw.Parameters,
		Items:      raw.Items,
		Source:     name,
	}

	targets, err := parseTargets(raw.Targets)
	if err != nil {
		return nil, err
	}
	s.Targets = targets

	if len(s.Targets) == 0 && len(s.Items) == 0 {
		return nil, errors.New("не указаны целевые серверы (targets) или items")
	}
	if s.Parameters == nil {
		s.Parameters = make(map[string]any)
	}

	return s, nil
}

// WithOverrides возвращает копию сценария с переопределёнными параметрами.
func (s *Scenario) WithOverrides(overrides map[string]string) *Scenario {
	if len(overrides) == 0 {
		return s
	}

	params := make(map[string]any, len(s.Parameters)+len(overrides))
	for key, value := range s.Parameters {
		params[key] = value
	}
	for key, value := range overrides {
		params[key] = value
	}

	clone := *s
	clone.Parameters = params
	return &clone
}

// AllCIs возвращает плоский список идентификаторов всех целей.
func (s *Scenario) AllCIs() []string {
	var cis []string
	for i := range s.Targets {
		cis = append(cis, s.Targets[i].GetCIs()...)
	}
	return cis
}

func parseTargets(raw any) ([]Target, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case []any:
		targets := make([]Target, 0, len(v))
		for i, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("target #%d: ожидался объект, получено %T", i+1, item)
			}
			targets = append(targets, targetFromMap(m))
		}
		return targets, nil
	case map[string]any:
		return []Target{targetFromMap(v)}, nil
	default:
		return nil, fmt.Errorf("неподдерживаемый формат targets: %T", raw)
	}
}

func targetFromMap(m map[string]any) Target {
	var target Target
	for key, value := range m {
		switch key {
		case "svm_ci", "ip", "IP":
		case "ci_list":
			for _, item := range asList(value) {
				if s, ok := item.(string); ok && s != "" {
					target.CIList = append(target.CIList, s)
				}
			}
		default:
			if target.Fields == nil {
				target.Fields = map[string]string{}
			}
			target.Fields[key] = inventory.Text(value)
		}
	}

	switch ci := m["svm_ci"].(type) {
	case string:
		target.SVMCI = ci
		if ci != "" {
			target.CIList = []string{ci}
		}
	case []any:
		for _, item := range ci {
			if s, ok := item.(string); ok && s != "" {
				target.CIList = append(target.CIList, s)
			}
		}
		if len(target.CIList) == 1 {
			target.SVMCI = target.CIList[0]
		}
	}

	if len(target.CIList) == 1 && target.SVMCI == "" {
		target.SVMCI = target.CIList[0]
	}

	// IP исторически встречается и как "ip", и как "IP".
	for _, key := range []string{"ip", "IP"} {
		if s, ok := m[key].(string); ok && s != "" {
			target.IP = s
			break
		}
	}

	return target
}

func asList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	}
	return nil
}

// ResolveTargets разворачивает цели по инвентарю и сужает их селекторами.
//
// Цель с полем host или group — выбор из инвентаря: вместо неё встают
// подходящие хосты, каждый со своими полями (svm_ci и ip — из переменных
// хоста), а прочие поля цели ложатся поверх. Не нашлось ни одного — отказ:
// сценарий на пустой группе молча ничего бы не сделал. limit оставляет
// цели, подходящие хотя бы под один селектор (--limit).
func (s *Scenario) ResolveTargets(hosts []inventory.Host, limit []inventory.Selector) error {
	var out []Target
	for i, t := range s.Targets {
		if !t.selects() {
			out = append(out, t)
			continue
		}
		sel, extra, err := t.selector()
		if err != nil {
			return fmt.Errorf("цель #%d: %w", i+1, err)
		}
		matched := inventory.Filter(hosts, []inventory.Selector{sel})
		if len(matched) == 0 {
			return fmt.Errorf("цель #%d (%s): в инвентаре нет подходящих хостов", i+1, sel)
		}
		for _, h := range matched {
			out = append(out, hostTarget(h, extra))
		}
	}

	if len(limit) > 0 {
		if len(out) == 0 {
			return fmt.Errorf("--limit %s отбирает цели, а у сценария их нет (только items)", inventory.Describe(limit))
		}
		var kept []Target
		for _, t := range out {
			if inventory.MatchAny(limit, t.Labels()) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			return fmt.Errorf("--limit %s не оставил ни одной цели", inventory.Describe(limit))
		}
		out = kept
	}
	s.Targets = out
	return nil
}

// selects — цель выбирает хосты из инвентаря.
func (t *Target) selects() bool {
	_, host := t.Fields["host"]
	_, group := t.Fields["group"]
	return host || group
}

// selector — условие выбора (host, group) и прочие поля цели.
func (t *Target) selector() (inventory.Selector, map[string]string, error) {
	var conds []string
	extra := map[string]string{}
	for k, v := range t.Fields {
		switch k {
		case "host", "group":
			if strings.ContainsAny(v, ",=") {
				return inventory.Selector{}, nil, fmt.Errorf("%s: %q — одно имя или шаблон", k, v)
			}
			conds = append(conds, k+"="+v)
		default:
			extra[k] = v
		}
	}
	sort.Strings(conds)
	sel, err := inventory.ParseSelector(strings.Join(conds, ","))
	return sel, extra, err
}

// hostTarget — цель из хоста инвентаря.
func hostTarget(h inventory.Host, extra map[string]string) Target {
	t := Target{Fields: map[string]string{"host": h.Name}}
	if len(h.Groups) > 0 {
		t.Fields["groups"] = strings.Join(h.Groups, ",")
	}
	for k, v := range h.Vars {
		t.Fields[k] = inventory.Text(v)
	}
	for k, v := range extra {
		t.Fields[k] = v
	}
	if ci := t.Fields["svm_ci"]; ci != "" {
		t.SVMCI, t.CIList = ci, []string{ci}
		delete(t.Fields, "svm_ci")
	}
	if ip := t.Fields["ip"]; ip != "" {
		t.IP = ip
		delete(t.Fields, "ip")
	}
	return t
}

// UsesInventory — сценарию нужен инвентарь: есть цель с host или group.
func (s *Scenario) UsesInventory() bool {
	for i := range s.Targets {
		if s.Targets[i].selects() {
			return true
		}
	}
	return false
}
