package pluginhost

import (
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Исполнители сценариев в модулях.
//
// Модуль может принести исполнителя — способ выполнять сценарии: по SSH,
// через Ansible, через API своей системы. Пакет переносит вызовы и не знает
// ни сценариев, ни планов: данные идут JSON, а разбирает их мост runnerhost
// по обе стороны. Модуль на другом языке реализует тот же gRPC-сервис и тот
// же JSON — Go-типы ему не нужны.

// Операции исполнителя. plan и apply обязательны, wait и status — если
// исполнитель умеет ждать и показывать состояние того, что создал.
const (
	OpPlan   = "plan"
	OpApply  = "apply"
	OpWait   = "wait"
	OpStatus = "status"
)

// RunnerSpec — исполнитель, которого объявляет модуль.
type RunnerSpec struct {
	// Kind — как называется способ выполнения: SSH, Ansible. Имя
	// исполнителя для --executor — Kind в нижнем регистре.
	Kind string
	// Services — сервисы сценариев, которые он выполняет.
	Services []string
	Ops      []string
	Handle   func(ctx context.Context, req RunnerRequest) ([]byte, error)
}

// RunnerRequest — один вызов исполнителя.
type RunnerRequest struct {
	Op      string
	Payload []byte
	// Credentials — токены, которые ядро выдало этому исполнителю (ochan
	// auth --executor). Свой API исполнитель может авторизовать и сам.
	Credentials map[string]string
}

// RunnerProvider реализует модуль с исполнителями.
type RunnerProvider interface {
	Runners() []RunnerSpec
}

func (s *CommandGRPCServer) GetRunners(_ context.Context, _ *Empty) (*RunnersResponse, error) {
	resp := &RunnersResponse{}
	provider, ok := s.Impl.(RunnerProvider)
	if !ok {
		return resp, nil
	}
	for _, spec := range provider.Runners() {
		resp.Runners = append(resp.Runners, &Runner{Kind: spec.Kind, Services: spec.Services, Ops: spec.Ops})
	}
	return resp, nil
}

func (s *CommandGRPCServer) CallRunner(ctx context.Context, call *RunnerCall) (*RunnerReply, error) {
	provider, ok := s.Impl.(RunnerProvider)
	if !ok {
		return nil, fmt.Errorf("у модуля %s нет исполнителей", s.Impl.Name())
	}
	for _, spec := range provider.Runners() {
		if spec.Kind != call.GetKind() {
			continue
		}
		if !slices.Contains(spec.Ops, call.GetOp()) || spec.Handle == nil {
			return nil, fmt.Errorf("исполнитель %s не умеет %s", spec.Kind, call.GetOp())
		}
		out, err := spec.Handle(ctx, RunnerRequest{Op: call.GetOp(), Payload: call.GetPayload(),
			Credentials: call.GetCredentials()})
		if err != nil {
			return nil, err
		}
		return &RunnerReply{Payload: out}, nil
	}
	return nil, fmt.Errorf("в модуле %s нет исполнителя %s", s.Impl.Name(), call.GetKind())
}

// RemoteRunner — исполнитель модуля на стороне ядра.
type RemoteRunner struct {
	// Module — имя модуля, который его принёс.
	Module   string
	Kind     string
	Services []string
	Ops      []string
	client   CommandModuleClient
}

// Can сообщает, умеет ли исполнитель операцию.
func (r RemoteRunner) Can(op string) bool { return slices.Contains(r.Ops, op) }

// Call вызывает исполнителя в процессе модуля с пределом времени команды
// модуля.
func (r RemoteRunner) Call(ctx context.Context, op string, payload []byte, credentials map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout())
	defer cancel()
	resp, err := r.client.CallRunner(ctx, &RunnerCall{Kind: r.Kind, Op: op, Payload: payload, Credentials: credentials})
	if err != nil {
		return nil, unwrapGRPCError(err)
	}
	return resp.GetPayload(), nil
}

// Runners спрашивает у модуля его исполнителей. Модуль, собранный до их
// появления, отвечает Unimplemented: исполнителей у него нет, и это не
// ошибка. Исполнитель без вида, сервисов, plan или apply — ошибка модуля.
func (c *CommandGRPCClient) Runners() ([]RemoteRunner, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Client.GetRunners(ctx, &Empty{})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
			return nil, nil
		}
		return nil, unwrapGRPCError(err)
	}
	var out []RemoteRunner
	for _, r := range resp.GetRunners() {
		switch {
		case r.GetKind() == "":
			return nil, fmt.Errorf("исполнитель без вида (kind)")
		case len(r.GetServices()) == 0:
			return nil, fmt.Errorf("исполнитель %s не выполняет ни одного сервиса", r.GetKind())
		case !slices.Contains(r.GetOps(), OpPlan) || !slices.Contains(r.GetOps(), OpApply):
			return nil, fmt.Errorf("исполнитель %s не умеет plan или apply", r.GetKind())
		}
		out = append(out, RemoteRunner{Kind: r.GetKind(), Services: r.GetServices(), Ops: r.GetOps(), client: c.Client})
	}
	return out, nil
}

// Runners возвращает исполнителей всех модулей в порядке путей модулей.
func (m *ModuleManager) Runners() []RemoteRunner {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []RemoteRunner
	for _, path := range m.pathsLocked() {
		out = append(out, m.modules[path].runners...)
	}
	return out
}
