// Package runnerhost — мост между исполнителями сценариев и модулями.
//
// Модуль приносит исполнителя (pluginhost.RunnerSpec), ядро выбирает его по
// полю service сценария, как встроенного. По проводу идёт JSON — формат
// описан типами этого файла, и модулю на другом языке достаточно его:
//
//	plan            {"scenario": {...}, "source": "..."}                  → план {"command", "changes": [...], ...}
//	apply           {"scenario": {...}, "source": "...", "plan": {...}}   → {"refs": [...], "summary": "...", "error": "..."}
//	compensate-plan {"scenario", "source", "plan": выполненный, "refs"}   → план обратного действия
//	compensate      {"scenario", "source", "plan": обратного действия, "refs"} → {"refs", "summary", "error"}
//	wait            {"ref": "..."}                                        → {"state": "..."}
//	status          {"ref": "..."}                                        → {"ref", "state", "fields": [...], "events": [...]}
//
// Сценарий — scenario.Scenario: service, parameters, targets, items. В
// apply приходит план, на который сказали «да»: исполнитель применяет ровно
// его и объявляет это операцией apply-plan в списке своих операций.
// Исполнитель без apply-plan собран до ochan 0.2.0, и ядро им не выполняет.
// Ошибка apply идёт в ответе, а не ошибкой вызова: созданное до сбоя уже
// есть во внешней системе, и его следы терять нельзя.
//
// Свой API исполнитель выбирает сам: ядро передаёт ему токен из ochan auth
// --executor и настройки из config.yaml (executors.<имя>), а как с ними
// ходить — HTTP, gRPC, SSH, — дело исполнителя.
package runnerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"octochan/config"
	"octochan/pluginhost"
	"octochan/scenario"
)

type scenarioRequest struct {
	Scenario *scenario.Scenario `json:"scenario"`
	Source   string             `json:"source,omitempty"`
	// Plan — в apply согласованный план, в compensate-plan выполненный,
	// в compensate план обратного действия.
	Plan *config.Plan `json:"plan,omitempty"`
	// Refs — следы выполнения, к которым относится обратное действие.
	Refs []string `json:"refs,omitempty"`
}

type applyReply struct {
	Refs    []string `json:"refs,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type refRequest struct {
	Ref string `json:"ref"`
}

type waitReply struct {
	State string `json:"state"`
}

type statusReply struct {
	Ref    string        `json:"ref"`
	State  string        `json:"state"`
	Fields []statusField `json:"fields,omitempty"`
	Events []statusEvent `json:"events,omitempty"`
}

type statusField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type statusEvent struct {
	Time    string `json:"time,omitempty"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message"`
}

// Expose объявляет исполнителя модуля: ochan выберет его для сценариев с
// этими сервисами. Ожидание, статус и обратное действие объявляются, если
// исполнитель реализует scenario.Waiter, scenario.StatusReporter и
// scenario.Compensator. Токен и настройки исполнителя приходят в
// контексте: Token(ctx), Settings(ctx).
func Expose(r scenario.Runner, services ...string) pluginhost.RunnerSpec {
	// apply-plan объявляется всегда: Apply исполнителя на Go принимает
	// план по сигнатуре.
	ops := []string{pluginhost.OpPlan, pluginhost.OpApply, pluginhost.OpApplyPlan}
	waiter, canWait := r.(scenario.Waiter)
	reporter, canStatus := r.(scenario.StatusReporter)
	compensator, canCompensate := r.(scenario.Compensator)
	if canWait {
		ops = append(ops, pluginhost.OpWait)
	}
	if canStatus {
		ops = append(ops, pluginhost.OpStatus)
	}
	if canCompensate {
		ops = append(ops, pluginhost.OpCompensatePlan, pluginhost.OpCompensate)
	}
	return pluginhost.RunnerSpec{
		Kind:     r.Kind(),
		Services: services,
		Ops:      ops,
		Handle: func(ctx context.Context, req pluginhost.RunnerRequest) ([]byte, error) {
			ctx = context.WithValue(ctx, supplyKey{}, pluginhost.Supply{Credentials: req.Credentials, Settings: req.Settings})
			switch req.Op {
			case pluginhost.OpPlan, pluginhost.OpApply, pluginhost.OpCompensatePlan, pluginhost.OpCompensate:
				var in scenarioRequest
				if err := json.Unmarshal(req.Payload, &in); err != nil || in.Scenario == nil {
					return nil, fmt.Errorf("сценарий не разобрать: %v", err)
				}
				in.Scenario.Source = in.Source
				if in.Scenario.Parameters == nil {
					in.Scenario.Parameters = map[string]any{}
				}
				if req.Op != pluginhost.OpPlan && in.Plan == nil {
					// Ядро до 0.2.0 плана не передаёт: применять «как
					// получится» исполнитель не должен.
					return nil, fmt.Errorf("%s без плана: ядро не передало план, на который сказали «да», — "+
						"обновите ochan до 0.2.0", req.Op)
				}
				switch req.Op {
				case pluginhost.OpPlan:
					plan, err := r.Plan(ctx, in.Scenario)
					if err != nil {
						return nil, err
					}
					return json.Marshal(plan)
				case pluginhost.OpApply:
					return applied(r.Apply(ctx, in.Scenario, in.Plan))
				case pluginhost.OpCompensatePlan:
					if !canCompensate {
						break
					}
					plan, err := compensator.PlanCompensation(ctx, in.Scenario, in.Plan, in.Refs)
					if err != nil {
						return nil, err
					}
					return json.Marshal(plan)
				case pluginhost.OpCompensate:
					if !canCompensate {
						break
					}
					return applied(compensator.Compensate(ctx, in.Scenario, in.Plan, in.Refs))
				}
			case pluginhost.OpWait:
				if !canWait {
					break
				}
				var in refRequest
				if err := json.Unmarshal(req.Payload, &in); err != nil {
					return nil, err
				}
				state, err := waiter.Wait(ctx, in.Ref)
				if err != nil {
					return nil, err
				}
				return json.Marshal(waitReply{State: state})
			case pluginhost.OpStatus:
				if !canStatus {
					break
				}
				var in refRequest
				if err := json.Unmarshal(req.Payload, &in); err != nil {
					return nil, err
				}
				st, err := reporter.Status(ctx, in.Ref)
				if err != nil {
					return nil, err
				}
				return json.Marshal(toStatusReply(st))
			}
			return nil, fmt.Errorf("исполнитель %s не умеет %s", r.Kind(), req.Op)
		},
	}
}

// applied — ответ на apply и compensate: следы выполнения и ошибка в
// ответе, а не ошибкой вызова.
func applied(result *scenario.Result, err error) ([]byte, error) {
	reply := applyReply{}
	if result != nil {
		reply.Refs, reply.Summary = result.Refs, result.Summary
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return json.Marshal(reply)
}

type supplyKey struct{}

// Token — токен, который ядро выдало исполнителю (ochan auth --executor);
// пусто — токена нет.
func Token(ctx context.Context) string {
	supply, _ := ctx.Value(supplyKey{}).(pluginhost.Supply)
	return supply.Credentials["token"]
}

// Settings — настройки исполнителя из config.yaml ochan: executors.<имя>
// поверх общих defaults.api_url, defaults.http_timeout и
// defaults.max_parallel_tasks.
func Settings(ctx context.Context) map[string]string {
	supply, _ := ctx.Value(supplyKey{}).(pluginhost.Supply)
	return supply.Settings
}

func toStatusReply(st *scenario.Status) statusReply {
	out := statusReply{Ref: st.Ref, State: st.State}
	for _, f := range st.Fields {
		out.Fields = append(out.Fields, statusField{Name: f.Name, Value: f.Value})
	}
	for _, e := range st.Events {
		out.Events = append(out.Events, statusEvent{Time: e.Time, Level: e.Level, Message: e.Message})
	}
	return out
}

// Supplier выдаёт исполнителю токены и настройки по имени исполнителя
// (rlm, ssh).
type Supplier func(executor string) pluginhost.Supply

// remote — исполнитель модуля как scenario.Runner.
type remote struct {
	r      pluginhost.RemoteRunner
	supply Supplier
}

func (x remote) Kind() string { return x.r.Kind }

func (x remote) call(ctx context.Context, op string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var supply pluginhost.Supply
	if x.supply != nil {
		supply = x.supply(scenario.ExecutorName(x))
	}
	reply, err := x.r.Call(ctx, op, payload, supply)
	if err != nil {
		return fmt.Errorf("исполнитель %s (модуль %s): %w", x.r.Kind, x.r.Module, err)
	}
	if err := json.Unmarshal(reply, out); err != nil {
		return fmt.Errorf("исполнитель %s (модуль %s): ответ на %s не разобрать: %w", x.r.Kind, x.r.Module, op, err)
	}
	return nil
}

func (x remote) Plan(ctx context.Context, s *scenario.Scenario) (*config.Plan, error) {
	var plan config.Plan
	if err := x.call(ctx, pluginhost.OpPlan, scenarioRequest{Scenario: s, Source: s.Source}, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// ErrNotPlanBound — исполнитель модуля не обещает применять согласованный
// план: модуль собран до ochan 0.2.0.
var ErrNotPlanBound = errors.New("не получает согласованный план: модуль собран до ochan 0.2.0 — пересоберите его")

func (x remote) Apply(ctx context.Context, s *scenario.Scenario, plan *config.Plan) (*scenario.Result, error) {
	if !x.r.Can(pluginhost.OpApplyPlan) {
		return nil, fmt.Errorf("исполнитель %s (модуль %s) %w", x.r.Kind, x.r.Module, ErrNotPlanBound)
	}
	return x.applyLike(ctx, pluginhost.OpApply, scenarioRequest{Scenario: s, Source: s.Source, Plan: plan})
}

func (x remote) applyLike(ctx context.Context, op string, in scenarioRequest) (*scenario.Result, error) {
	var reply applyReply
	if err := x.call(ctx, op, in, &reply); err != nil {
		return nil, err
	}
	result := &scenario.Result{Refs: reply.Refs, Summary: reply.Summary}
	if reply.Error != "" {
		return result, errors.New(reply.Error)
	}
	return result, nil
}

func (x remote) wait(ctx context.Context, ref string) (string, error) {
	var reply waitReply
	if err := x.call(ctx, pluginhost.OpWait, refRequest{Ref: ref}, &reply); err != nil {
		return "", err
	}
	return reply.State, nil
}

func (x remote) status(ctx context.Context, ref string) (*scenario.Status, error) {
	var reply statusReply
	if err := x.call(ctx, pluginhost.OpStatus, refRequest{Ref: ref}, &reply); err != nil {
		return nil, err
	}
	st := &scenario.Status{Ref: reply.Ref, State: reply.State}
	for _, f := range reply.Fields {
		st.Fields = append(st.Fields, scenario.StatusField{Name: f.Name, Value: f.Value})
	}
	for _, e := range reply.Events {
		st.Events = append(st.Events, scenario.StatusEvent{Time: e.Time, Level: e.Level, Message: e.Message})
	}
	return st, nil
}

// Необязательные возможности объявляются типом: команды apply, status и
// compensate проверяют их приведением, и исполнитель, который ждать или
// откатывать не умеет, не должен делать вид, что умеет. Каждая
// возможность — своё поле, а набор — безымянная структура из тех, что
// исполнитель объявил.
type (
	waits        struct{ x remote }
	reports      struct{ x remote }
	compensates  struct{ x remote }
	runnerModule interface{ module() string }
)

func (c waits) Wait(ctx context.Context, ref string) (string, error) { return c.x.wait(ctx, ref) }

func (c reports) Status(ctx context.Context, ref string) (*scenario.Status, error) {
	return c.x.status(ctx, ref)
}

func (c compensates) PlanCompensation(ctx context.Context, s *scenario.Scenario, applied *config.Plan,
	refs []string) (*config.Plan, error) {
	var plan config.Plan
	in := scenarioRequest{Scenario: s, Source: s.Source, Plan: applied, Refs: refs}
	if err := c.x.call(ctx, pluginhost.OpCompensatePlan, in, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (c compensates) Compensate(ctx context.Context, s *scenario.Scenario, plan *config.Plan,
	refs []string) (*scenario.Result, error) {
	return c.x.applyLike(ctx, pluginhost.OpCompensate, scenarioRequest{Scenario: s, Source: s.Source, Plan: plan, Refs: refs})
}

// Runner — исполнитель модуля как scenario.Runner, с ожиданием, статусом и
// обратным действием, если он их объявил.
func Runner(r pluginhost.RemoteRunner, supply Supplier) scenario.Runner {
	x := remote{r: r, supply: supply}
	w, s := waits{x}, reports{x}
	c := compensates{x}
	wait, status := r.Can(pluginhost.OpWait), r.Can(pluginhost.OpStatus)
	comp := r.Can(pluginhost.OpCompensatePlan) && r.Can(pluginhost.OpCompensate)
	switch {
	case wait && status && comp:
		return struct {
			remote
			waits
			reports
			compensates
		}{x, w, s, c}
	case wait && status:
		return struct {
			remote
			waits
			reports
		}{x, w, s}
	case wait && comp:
		return struct {
			remote
			waits
			compensates
		}{x, w, c}
	case status && comp:
		return struct {
			remote
			reports
			compensates
		}{x, s, c}
	case wait:
		return struct {
			remote
			waits
		}{x, w}
	case status:
		return struct {
			remote
			reports
		}{x, s}
	case comp:
		return struct {
			remote
			compensates
		}{x, c}
	}
	return x
}

// Register приводит реестр сценариев к исполнителям загруженных модулей:
// сервисы выгруженных модулей из него уходят, новые приходят. Сервис,
// который уже выполняет встроенный исполнитель или модуль раньше по
// порядку, не перехватывается — модуль не подменит исполнителя молча.
// Возвращает предупреждения о таких сервисах.
func Register(runners []pluginhost.RemoteRunner, supply Supplier) []string {
	for _, service := range scenario.Services() {
		if have, err := scenario.Resolve(service); err == nil {
			if _, fromModule := have.(runnerModule); fromModule {
				scenario.Unregister(service)
			}
		}
	}
	var warnings []string
	for _, r := range runners {
		runner := Runner(r, supply)
		for _, service := range r.Services {
			if have, err := scenario.Resolve(service); err == nil {
				warnings = append(warnings, fmt.Sprintf("модуль %s: сервис %s уже выполняет исполнитель %s — пропущен",
					r.Module, service, have.Kind()))
				continue
			}
			scenario.Register(service, runner)
		}
	}
	return warnings
}

func (x remote) module() string { return x.r.Module }
