package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ochan/embedded"
	"ochan/fp"
	"ochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Цикл агента своей модели — то, чем для внешней модели служит MCP.
//
// Внешняя модель сама решает, какой инструмент вызвать. Своя модель ℒ
// отвечает только формальным промптом, а в действия её ответ переводит
// ochan, по правилам, которые знает оператор:
//
//   - задача — письмо в ящике агента, открытый инцидент на его хосте или
//     --prompt; задача не на языке своей модели не достаётся;
//   - нет предпосылки (⊥undef), отказ модели, вопрос оператору (кольцо 2,
//     посылки не выбирают), ответ, который ochan не принял, — инцидент
//     эскалируется, на письмо уходит ответ с причиной;
//   - ответ принят — заметка к инциденту, ответ на письмо; если к задаче
//     приложен сценарий исполнителя с местами ${Класс.атрибут}, решённые
//     значения подставляются в него, и он становится заявкой с --incident:
//     под free агента заявка в пределах политики выполняется без человека,
//     иначе ждёт согласования; выполнена — инцидент решён.
//
// Решение в заявку переводит не модель и не догадка ochan: это делает
// сценарий, который написал оператор. Перенос структуры задачи в структуру
// реализации манифест сознательно не автоматизирует (вопрос 14) — здесь он
// явный.
//
// Действия идут командами ochan от роли llm и агента — с теми же правами,
// пределами, журналом аудита и проверкой языка, что у любой другой модели.

const agentsDir = "agents"

// agentState — что агент уже обработал: по письму и инциденту — один раз.
type agentState struct {
	Mail      map[string]time.Time `json:"mail"`
	Incidents map[string]time.Time `json:"incidents"`
}

type agentRun struct {
	cmd    *cobra.Command
	agent  string
	engine llmBackend
	info   embedded.Info
	opts   fp.Options
	state  agentState
	path   string
	ctx    context.Context
}

func (a *agentRun) load() error {
	fm, err := mustFileManager()
	if err != nil {
		return err
	}
	name := a.agent
	if name == "" {
		name = "_"
	}
	a.path = filepath.Join(fm.BaseDir(), llmDir, agentsDir, name+".json")
	a.state = agentState{Mail: map[string]time.Time{}, Incidents: map[string]time.Time{}}
	data, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &a.state); err != nil {
		return fmt.Errorf("состояние агента %s испорчено: %w", a.path, err)
	}
	if a.state.Mail == nil {
		a.state.Mail = map[string]time.Time{}
	}
	if a.state.Incidents == nil {
		a.state.Incidents = map[string]time.Time{}
	}
	return nil
}

func (a *agentRun) save() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// ochan выполняет команду ochan от роли llm и агента: у действия модели —
// её права, пределы и журнал аудита.
func (a *agentRun) ochan(stdin string, args ...string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	c := exec.CommandContext(a.ctx, exe, args...)
	c.Env = append(os.Environ(), EnvRole+"=llm")
	// Тот же конфиг, что у цикла: путём из окружения, а не флагом — путь в
	// рабочий каталог ochan роли llm закрыт.
	if cfg := viper.ConfigFileUsed(); cfg != "" {
		c.Env = append(c.Env, "OCTOCHAN_CONFIG="+cfg)
	}
	if a.agent != "" {
		c.Env = append(c.Env, EnvAgent+"="+a.agent)
	}
	c.Stdin = strings.NewReader(stdin)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (a *agentRun) logf(format string, args ...any) { a.cmd.Printf(format, args...) }

// ---------------------------------------------------------------------------
// Задача и ответ
// ---------------------------------------------------------------------------

// outcome — что вышло с задачей.
type outcome struct {
	// answer — принятый ответ модели; пусто — ответа нет.
	answer string
	// errors — недостающие предпосылки; checks — вопросы оператору; faults
	// — что не так с ответом модели или задачей. Всё — именами языка.
	errors, checks, faults []string
	why                    string
}

func (o outcome) accepted() bool { return o.answer != "" && len(o.faults) == 0 }

// solve — ответ своей модели на задачу: те же проверки, что у llm
// generate, и та же запись в журналы.
func (a *agentRun) solve(name, text, object string) outcome {
	r := fp.CheckText(name, text, a.opts)
	if !r.InLanguage() {
		return outcome{faults: []string{object}, why: "задача не на языке: " + countCodes(r.Syntax())}
	}
	var undef []string
	for _, d := range r.Diagnostics {
		if d.Code == fp.ErrUndef {
			undef = append(undef, d.Msg)
		}
	}
	if len(undef) > 0 {
		var names []string
		for _, req := range r.Required {
			if !slices.Contains(r.Knowledge, req) {
				names = append(names, req)
			}
		}
		if len(names) == 0 {
			names = []string{object}
		}
		return outcome{errors: names, why: "не хватает предпосылок (⊥undef): " + strings.Join(undef, "; ")}
	}
	task := archTask{name: name, text: text, plan: fp.PlanAnswer(text, r.Program), premises: premisesOf(r.Program),
		knowledge: knowledgeImports(r.Program, a.opts)}
	for _, k := range task.knowledge {
		if !slices.Contains(a.info.Knowledge, k) {
			return outcome{faults: []string{"model"}, why: "задача подключает знания @" + k + ", а модель их не видит"}
		}
	}
	prompt := fp.ProtocolFor(fp.Formal) + "\n\n# Task\n\n" + text
	res, err := answerArchitectural(a.cmd, a.engine, a.info, prompt, task, a.opts)
	if err != nil {
		return outcome{faults: []string{"model"}, why: "модель не ответила: " + err.Error()}
	}
	if res.ans.Refused {
		var names []string
		for _, m := range res.ans.Missing {
			head, _, _ := strings.Cut(m, ":")
			names = append(names, strings.TrimSpace(head))
		}
		return outcome{errors: names, why: "модель отказала: не хватает " + strings.Join(res.ans.Missing, "; ")}
	}
	if len(res.problems) > 0 {
		return outcome{faults: []string{"model"}, why: "ochan не принял ответ: " + strings.Join(res.problems, "; ")}
	}
	o := outcome{answer: res.ans.Text}
	for _, q := range res.ans.Questions {
		switch {
		case q.Attr != "":
			o.checks = append(o.checks, q.Attr)
		case q.Statement != "":
			target, _, _ := strings.Cut(strings.TrimPrefix(q.Statement, "set "), " ")
			_, attr, _ := strings.Cut(target, ".")
			o.checks = append(o.checks, attr)
		default:
			o.checks = append(o.checks, "model")
		}
	}
	if len(o.checks) > 0 {
		o.why = fmt.Sprintf("вопросов оператору %d", len(o.checks))
	}
	return o
}

// reason — почему задача уходит людям, формальным промптом: недостающие
// предпосылки — Failure.error, вопросы оператору — Failure.check, что не
// так с ответом или задачей — Failure.fault. Значения — имена задачи и
// объекты ochan: текст проходит ту же проверку, что любой текст модели.
func (o outcome) reason() string {
	doc := &formalDoc{goal: "failure", classes: [][2]string{{"Failure", "failure"}}}
	c := doc.cell("c1")
	add := func(attr string, values []string) {
		seen := map[string]bool{}
		n := 0
		for _, v := range values {
			v = formalName(v, attr)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			n++
			name := attr
			if n > 1 {
				name = fmt.Sprintf("%s_%d", attr, n)
			}
			c.set("Failure."+name, v)
		}
	}
	add("error", o.errors)
	add("check", o.checks)
	add("fault", o.faults)
	return doc.String()
}

// ---------------------------------------------------------------------------
// Сценарий реализации
// ---------------------------------------------------------------------------

var placeholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)+)\}`)

// checkRealization — сценарий исполнителя с местами ${Класс.атрибут}:
// YAML-словарь с service. Возвращает текст как есть.
func checkRealization(text string) (string, error) {
	var top map[string]any
	if err := yaml.Unmarshal([]byte(text), &top); err != nil {
		return "", fmt.Errorf("сценарий не YAML: %w", err)
	}
	if _, ok := top["service"]; !ok {
		return "", errors.New("в сценарии нет service: решение модели становится заявкой на исполнителя")
	}
	return text, nil
}

func setIncidentScenario(id, text string) (*workspace.Incident, error) {
	store, err := incidentStore()
	if err != nil {
		return nil, err
	}
	return store.Update(id, func(i *workspace.Incident) error {
		i.Scenario = text
		return nil
	})
}

// answerValues — значения ответа: посылки задачи и решения модели,
// «Класс.атрибут» → значение. Неточные (5.2) не подставляются.
func answerValues(answer string) map[string]string {
	prog, _ := fp.Parse("answer", answer)
	out := map[string]string{}
	if prog == nil {
		return out
	}
	for _, cell := range prog.Cells {
		for _, set := range cell.Sets {
			if !set.Value.Inexact() && !set.Value.Ref {
				out[set.Target] = set.Value.Text
			}
		}
	}
	return out
}

// realize подставляет значения ответа в сценарий. Место, которому в
// ответе нет значения, — вопрос оператору, а не заявка с пустым местом.
func realize(text string, values map[string]string) (string, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", nil, err
	}
	var missing []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode && n.Tag != "!!null" {
			if m := placeholder.FindStringSubmatch(n.Value); m != nil && m[0] == n.Value {
				v, ok := values[m[1]]
				if !ok {
					missing = append(missing, m[1])
					return
				}
				n.Value, n.Style = v, 0
				if _, err := strconv.ParseFloat(v, 64); err == nil {
					n.Tag = "!!float"
					if _, err := strconv.Atoi(v); err == nil {
						n.Tag = "!!int"
					}
				} else {
					n.Tag = "!!str"
				}
				return
			}
			n.Value = placeholder.ReplaceAllStringFunc(n.Value, func(s string) string {
				key := placeholder.FindStringSubmatch(s)[1]
				v, ok := values[key]
				if !ok {
					missing = append(missing, key)
				}
				return v
			})
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	if len(missing) > 0 {
		return "", missing, nil
	}
	out, err := yaml.Marshal(&doc)
	return string(out), nil, err
}

var createdRequest = regexp.MustCompile(`Заявка ([0-9a-f]{6,}) создана`)

// request — заявка по сценарию с решёнными значениями: от роли llm и
// агента, под его free. Возвращает заявку и выполнена ли она.
func (a *agentRun) request(item, scenarioText, answer, incident string) (string, bool, []string, error) {
	filled, missing, err := realize(scenarioText, answerValues(answer))
	if err != nil || len(missing) > 0 {
		return "", false, missing, err
	}
	dir := filepath.Join(filepath.Dir(a.path), strings.TrimSuffix(filepath.Base(a.path), ".json"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, nil, err
	}
	// Копия — для разбора: сценарий, каким его получила заявка. Самой
	// команде он идёт через stdin: рабочий каталог ochan роли llm закрыт.
	if err := os.WriteFile(filepath.Join(dir, item+".yaml"), []byte(filled), 0o600); err != nil {
		return "", false, nil, err
	}
	args := []string{"request", "-"}
	if incident != "" {
		args = append(args, "--incident", incident)
	}
	out, err := a.ochan(filled, args...)
	m := createdRequest.FindStringSubmatch(out)
	if m == nil {
		if err == nil {
			err = errors.New("заявка не создана")
		}
		return "", false, nil, fmt.Errorf("%v: %s", err, out)
	}
	store, serr := requestStore()
	if serr != nil {
		return m[1], false, nil, serr
	}
	r, serr := store.Get(m[1])
	if serr != nil {
		return m[1], false, nil, serr
	}
	return r.ID, r.State == workspace.RequestApplied, nil, nil
}

// ---------------------------------------------------------------------------
// Источники задач
// ---------------------------------------------------------------------------

// mailbox — письма агенту, которые он ещё не обработал.
func (a *agentRun) mailbox() ([]*workspace.Message, error) {
	if a.agent == "" {
		return nil, nil
	}
	box, err := mailbox()
	if err != nil {
		return nil, err
	}
	all, err := box.List()
	if err != nil {
		return nil, err
	}
	var out []*workspace.Message
	for _, msg := range workspace.MessagesFor(all, a.agent) {
		if _, done := a.state.Mail[msg.ID]; !done && msg.FromAgent != a.agent {
			out = append(out, msg)
		}
	}
	slices.Reverse(out) // от старых к новым
	return out, nil
}

// incidents — открытые инциденты на хосте агента, которые он ещё не брал.
func (a *agentRun) incidents() ([]*workspace.Incident, error) {
	if a.agent == "" {
		return nil, nil
	}
	settleIncidents(func(string, ...any) {})
	store, err := incidentStore()
	if err != nil {
		return nil, err
	}
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	var out []*workspace.Incident
	for _, i := range all {
		if _, done := a.state.Incidents[i.ID]; done || i.State != workspace.IncidentOpen {
			continue
		}
		if slices.Contains(i.Hosts, a.agent) {
			out = append(out, i)
		}
	}
	slices.Reverse(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// Действия
// ---------------------------------------------------------------------------

func (a *agentRun) handleMail(msg *workspace.Message) {
	o := a.solve("письмо "+msg.ID, msg.Body, "request")
	to := msg.FromAgent
	if to == "" {
		to = "operator"
	}
	send := func(body, what string) {
		if out, err := a.ochan(body, "mail", "send", "--to", to, "--reply", msg.ID, "-"); err != nil {
			a.logf("  письмо %s → %s не ушло: %s\n", what, to, out)
		} else {
			a.logf("  %s → %s\n", what, to)
		}
	}
	a.logf("Письмо %s от %s\n", msg.ID, msg.From)
	if o.accepted() {
		send(o.answer, "ответ")
	}
	if !o.accepted() || len(o.checks) > 0 {
		a.logf("  %s\n", o.why)
		send(o.reason(), "причина")
	}
	a.state.Mail[msg.ID] = time.Now().UTC()
}

func (a *agentRun) handleIncident(i *workspace.Incident) {
	a.logf("Инцидент %s (%s)\n", i.ID, i.Level)
	o := a.solve("инцидент "+i.ID, i.Summary, "incident_"+i.ID)
	escalate := func(o outcome) {
		if out, err := a.ochan(o.reason(), "incident", "escalate", i.ID, "-"); err != nil {
			a.logf("  эскалация не удалась: %s\n", out)
		} else {
			a.logf("  эскалирован инженерам: %s\n", o.why)
		}
	}
	defer func() { a.state.Incidents[i.ID] = time.Now().UTC() }()
	if !o.accepted() {
		escalate(o)
		return
	}
	if out, err := a.ochan(o.answer, "incident", "note", i.ID, "-"); err != nil {
		a.logf("  заметка не записана: %s\n", out)
		escalate(outcome{faults: []string{"model"}, why: "ответ модели не принят записью инцидента: " + out})
		return
	}
	a.logf("  ответ модели — заметкой\n")
	if len(o.checks) > 0 {
		escalate(o)
		return
	}
	if i.Scenario == "" {
		escalate(outcome{checks: []string{"request"}, why: "к инциденту не приложен сценарий: решение есть, заявку оформит человек"})
		return
	}
	id, applied, missing, err := a.request(i.ID, i.Scenario, o.answer, i.ID)
	switch {
	case err != nil:
		escalate(outcome{faults: []string{"request"}, why: "заявка не оформлена: " + err.Error()})
	case len(missing) > 0:
		escalate(outcome{checks: missing, why: "в ответе нет значений для сценария: " + strings.Join(missing, ", ")})
	case applied:
		a.logf("  заявка %s выполнена без человека (free)\n", id)
		if out, err := a.ochan(o.answer, "incident", "resolve", i.ID, "-"); err != nil {
			a.logf("  инцидент не закрыт: %s\n", out)
		} else {
			a.logf("  инцидент решён\n")
		}
	default:
		a.logf("  заявка %s ждёт согласования — инцидент открыт\n", id)
	}
}

// once — один проход: письма, затем инциденты.
func (a *agentRun) once() error {
	msgs, err := a.mailbox()
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		a.handleMail(msg)
		if err := a.save(); err != nil {
			return err
		}
	}
	incidents, err := a.incidents()
	if err != nil {
		return err
	}
	for _, i := range incidents {
		a.handleIncident(i)
		if err := a.save(); err != nil {
			return err
		}
	}
	return nil
}

var (
	llmRunAgent    string
	llmRunOnce     bool
	llmRunEvery    time.Duration
	llmRunPrompt   string
	llmRunScenario string
)

var llmRunCmd = &cobra.Command{
	Use:   "run [--agent имя] [--once] | --prompt задача.fp [--scenario сценарий.yaml]",
	Short: "Цикл агента своей модели: ответ становится заявкой, шагом инцидента или письмом",
	Long: "Своя модель ℒ отвечает только формальным промптом; в действия её ответ\n" +
		"переводит ochan — командами от роли llm и агента, с их правами и журналом.\n\n" +
		"Задачи: письма в ящике агента (--agent или OCTOCHAN_AGENT) и открытые\n" +
		"инциденты на его хосте; с --prompt — одна задача из файла. Задача не на\n" +
		"языке, без предпосылки (⊥undef), отказ модели, вопрос оператору, ответ,\n" +
		"который ochan не принял, — инцидент эскалируется инженерам, на письмо\n" +
		"уходит причина формальным промптом. Ответ принят — заметка к инциденту\n" +
		"или ответ на письмо.\n\n" +
		"Заявку из решения делает сценарий исполнителя, который приложил оператор\n" +
		"(incident open --scenario, поле scenario вебхука, здесь --scenario): места\n" +
		"${Класс.атрибут} получают решённые значения ответа. Под free агента заявка\n" +
		"в пределах политики выполняется без человека — тогда инцидент решён; иначе\n" +
		"ждёт согласования. Без сценария решение уходит людям: заявку оформит\n" +
		"человек.\n\n" +
		"Без --once цикл ходит каждые --every, пока его не остановят; на standby он\n" +
		"ждёт. Своя модель (llm.model) загружается в процесс один раз на весь цикл.",
	Args: cobra.NoArgs,
	Example: "ochan llm run --agent db7\n" +
		"ochan llm run --agent db7 --once\n" +
		"ochan llm run --prompt задача.fp --scenario raise.yaml",
	RunE: func(cmd *cobra.Command, _ []string) error {
		mode, err := llmMode()
		if err != nil {
			return err
		}
		if mode != llmEmbedded {
			return fmt.Errorf("llm run — цикл своей модели: llm.mode: embedded (сейчас %s; внешняя модель работает через MCP)", mode)
		}
		if llmRunScenario != "" && llmRunPrompt == "" {
			return errors.New("--scenario — к задаче --prompt; у инцидента сценарий свой (incident open --scenario)")
		}
		engine, err := llmEngine()
		if err != nil {
			return err
		}
		ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stopSignals()
		info, err := engine.Info(ctx)
		if err != nil {
			return fmt.Errorf("%w — цикла нет: ochan говорит с моделью только по подтверждённому протоколу", err)
		}
		if !info.Generate {
			return errors.New("цикл агента — для своей модели ℒ (/v1/info: \"generate\": true); чужой движок ведёт декодер ochan — llm generate")
		}
		opts, err := fpOptions(cmd, false)
		if err != nil {
			return err
		}
		agent := llmRunAgent
		if agent == "" {
			agent = os.Getenv(EnvAgent)
		}
		a := &agentRun{cmd: cmd, agent: agent, engine: engine, info: info, opts: opts, ctx: ctx}
		if err := a.load(); err != nil {
			return err
		}

		if llmRunPrompt != "" {
			src, err := readSource(llmRunPrompt)
			if err != nil {
				return err
			}
			o := a.solve(src.Name, src.Content, "request")
			if !o.accepted() {
				cmd.PrintErrf("%s\n", o.why)
				cmd.Print(o.reason())
				return errors.New("ответа нет — причина выше, формальным промптом")
			}
			cmd.Print(o.answer)
			if len(o.checks) > 0 {
				cmd.PrintErrf("%s:\n%s", o.why, o.reason())
			}
			if llmRunScenario == "" {
				return nil
			}
			data, err := os.ReadFile(llmRunScenario)
			if err != nil {
				return err
			}
			text, err := checkRealization(string(data))
			if err != nil {
				return err
			}
			if len(o.checks) > 0 {
				return errors.New("есть вопросы оператору — заявки нет, пока их не решат")
			}
			item := "prompt-" + time.Now().UTC().Format("20060102-150405")
			id, applied, missing, err := a.request(item, text, o.answer, "")
			switch {
			case err != nil:
				return err
			case len(missing) > 0:
				return fmt.Errorf("в ответе нет значений для сценария: %s", strings.Join(missing, ", "))
			case applied:
				cmd.PrintErrf("Заявка %s выполнена без человека (free)\n", id)
			default:
				cmd.PrintErrf("Заявка %s ждёт согласования: ochan approve %s\n", id, id)
			}
			return nil
		}

		if agent == "" {
			return errors.New("чей ящик и чьи инциденты: --agent или OCTOCHAN_AGENT")
		}
		every := llmRunEvery
		if every <= 0 {
			every = 30 * time.Second
		}
		cmd.Printf("Агент %s: модель %s; задачи — письма и инциденты на хосте %s\n", agent, info.Model, agent)
		for {
			if onStandby() {
				if llmRunOnce {
					return errors.New("узел — standby: задачи берёт primary")
				}
			} else if err := a.once(); err != nil {
				return err
			}
			if llmRunOnce {
				return nil
			}
			select {
			case <-ctx.Done():
				cmd.Println("Цикл агента остановлен")
				return nil
			case <-time.After(every):
			}
		}
	},
}

func init() {
	llmRunCmd.Flags().StringVar(&llmRunAgent, "agent", "", "агент: его ящик и инциденты на его хосте; без флага — OCTOCHAN_AGENT")
	llmRunCmd.Flags().BoolVar(&llmRunOnce, "once", false, "один проход и выход")
	llmRunCmd.Flags().DurationVar(&llmRunEvery, "every", 30*time.Second, "как часто смотреть письма и инциденты")
	llmRunCmd.Flags().StringVar(&llmRunPrompt, "prompt", "", "одна задача из файла (- — stdin)")
	llmRunCmd.Flags().StringVar(&llmRunScenario, "scenario", "", "сценарий исполнителя с местами ${Класс.атрибут} для задачи --prompt")
	llmCmd.AddCommand(llmRunCmd)
}
