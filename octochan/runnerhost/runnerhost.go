// Package runnerhost — мост между исполнителями сценариев и модулями.
//
// Модуль приносит исполнителя (pluginhost.RunnerSpec), ядро выбирает его по
// полю service сценария, как встроенного. По проводу идёт JSON — формат
// описан типами этого файла, и модулю на другом языке достаточно его:
//
//	plan   {"scenario": {...}, "source": "..."} → план {"command", "changes": [...], ...}
//	apply  {"scenario": {...}, "source": "..."} → {"refs": [...], "summary": "...", "error": "..."}
//	wait   {"ref": "..."}                       → {"state": "..."}
//	status {"ref": "..."}                       → {"ref", "state", "fields": [...], "events": [...]}
//
// Сценарий — scenario.Scenario: service, parameters, targets, items. Ошибка
// apply идёт в ответе, а не ошибкой вызова: созданное до сбоя уже есть во
// внешней системе, и его следы терять нельзя.
//
// Свой API исполнитель выбирает сам: ядро передаёт ему токен из ochan auth
// --executor, а как с ним ходить — HTTP, gRPC, SSH, — дело исполнителя.
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
// этими сервисами. Ожидание и статус объявляются, если исполнитель
// реализует scenario.Waiter и scenario.StatusReporter. Токен исполнителя
// приходит в контексте: Token(ctx).
func Expose(r scenario.Runner, services ...string) pluginhost.RunnerSpec {
	ops := []string{pluginhost.OpPlan, pluginhost.OpApply}
	waiter, canWait := r.(scenario.Waiter)
	reporter, canStatus := r.(scenario.StatusReporter)
	if canWait {
		ops = append(ops, pluginhost.OpWait)
	}
	if canStatus {
		ops = append(ops, pluginhost.OpStatus)
	}
	return pluginhost.RunnerSpec{
		Kind:     r.Kind(),
		Services: services,
		Ops:      ops,
		Handle: func(ctx context.Context, req pluginhost.RunnerRequest) ([]byte, error) {
			ctx = context.WithValue(ctx, credentialsKey{}, req.Credentials)
			switch req.Op {
			case pluginhost.OpPlan, pluginhost.OpApply:
				var in scenarioRequest
				if err := json.Unmarshal(req.Payload, &in); err != nil || in.Scenario == nil {
					return nil, fmt.Errorf("сценарий не разобрать: %v", err)
				}
				in.Scenario.Source = in.Source
				if in.Scenario.Parameters == nil {
					in.Scenario.Parameters = map[string]any{}
				}
				if req.Op == pluginhost.OpPlan {
					plan, err := r.Plan(ctx, in.Scenario)
					if err != nil {
						return nil, err
					}
					return json.Marshal(plan)
				}
				result, err := r.Apply(ctx, in.Scenario)
				reply := applyReply{}
				if result != nil {
					reply.Refs, reply.Summary = result.Refs, result.Summary
				}
				if err != nil {
					reply.Error = err.Error()
				}
				return json.Marshal(reply)
			case pluginhost.OpWait:
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

type credentialsKey struct{}

// Token — токен, который ядро выдало исполнителю (ochan auth --executor);
// пусто — токена нет.
func Token(ctx context.Context) string {
	creds, _ := ctx.Value(credentialsKey{}).(map[string]string)
	return creds["token"]
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

// Credentials выдаёт исполнителю его токены по имени исполнителя (rlm,
// ssh); nil — токенов нет.
type Credentials func(executor string) map[string]string

// remote — исполнитель модуля как scenario.Runner.
type remote struct {
	r     pluginhost.RemoteRunner
	creds Credentials
}

func (x remote) Kind() string { return x.r.Kind }

func (x remote) call(ctx context.Context, op string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var creds map[string]string
	if x.creds != nil {
		creds = x.creds(scenario.ExecutorName(x))
	}
	reply, err := x.r.Call(ctx, op, payload, creds)
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

func (x remote) Apply(ctx context.Context, s *scenario.Scenario) (*scenario.Result, error) {
	var reply applyReply
	if err := x.call(ctx, pluginhost.OpApply, scenarioRequest{Scenario: s, Source: s.Source}, &reply); err != nil {
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

// Необязательные возможности объявляются типом: команды apply и status
// проверяют их приведением, и исполнитель, который ждать не умеет, не
// должен делать вид, что умеет.
type (
	waiting   struct{ remote }
	reporting struct{ remote }
	full      struct{ remote }
)

func (x waiting) Wait(ctx context.Context, ref string) (string, error) { return x.wait(ctx, ref) }
func (x reporting) Status(ctx context.Context, ref string) (*scenario.Status, error) {
	return x.status(ctx, ref)
}
func (x full) Wait(ctx context.Context, ref string) (string, error) { return x.wait(ctx, ref) }
func (x full) Status(ctx context.Context, ref string) (*scenario.Status, error) {
	return x.status(ctx, ref)
}

// Runner — исполнитель модуля как scenario.Runner, с ожиданием и статусом,
// если он их объявил.
func Runner(r pluginhost.RemoteRunner, creds Credentials) scenario.Runner {
	x := remote{r: r, creds: creds}
	switch wait, st := r.Can(pluginhost.OpWait), r.Can(pluginhost.OpStatus); {
	case wait && st:
		return full{x}
	case wait:
		return waiting{x}
	case st:
		return reporting{x}
	}
	return x
}

// Register приводит реестр сценариев к исполнителям загруженных модулей:
// сервисы выгруженных модулей из него уходят, новые приходят. Сервис,
// который уже выполняет встроенный исполнитель или модуль раньше по
// порядку, не перехватывается — модуль не подменит исполнителя молча.
// Возвращает предупреждения о таких сервисах.
func Register(runners []pluginhost.RemoteRunner, creds Credentials) []string {
	for _, service := range scenario.Services() {
		if have, err := scenario.Resolve(service); err == nil {
			if _, fromModule := have.(interface{ module() string }); fromModule {
				scenario.Unregister(service)
			}
		}
	}
	var warnings []string
	for _, r := range runners {
		runner := Runner(r, creds)
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
