package cmd

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"ochan/embedded"
	"ochan/fp"
	"ochan/remote"
	"ochan/report"
	"ochan/scenario"
	"ochan/workspace"

	"github.com/spf13/viper"
)

// logicModel — своя модель для теста: отвечает тем, что выводит логика
// (как own.Engine, у которого сеть выбирает только порядок).
type logicModel struct{}

func (logicModel) Next(context.Context, string, string, int) (embedded.Step, error) {
	return embedded.Step{End: 1}, nil
}

func (logicModel) Generate(_ context.Context, prompt string) (embedded.Answer, error) {
	_, task, _ := strings.Cut(prompt, "\n\n# Task\n\n")
	prog, diags := fp.Parse("task", task)
	if len(diags) > 0 {
		return embedded.Answer{Refused: true, Missing: []string{"task"}}, nil
	}
	plan := fp.PlanAnswer(task, prog)
	ans := embedded.Answer{Text: plan.Text()}
	for _, d := range plan.Decisions {
		ans.Packages = append(ans.Packages, embedded.ModelPackage{Statement: strings.TrimSpace(d.Line()),
			Rules: []string{"fp.defines"}, L: 0.91, U: 1, Verdict: "well_true"})
		ans.Forced++
	}
	return ans, nil
}

const loopTask = "goal: probe\n\nclasses:\n  File : \"file\"\n  Approval : \"approve\"\n  Probe : \"probe\"\n\n" +
	"relations:\n  File --defines--> Approval (database_quota)\n  Approval --strives_to--> Probe\n\n" +
	"cell c1:\n  set File.limit = 200\n"

// Цикл агента: ответ своей модели становится заметкой, заявкой под free и
// решением инцидента; на письмо — ответ; чего модель не может — уходит
// людям формальным промптом.
func TestLLMRunAgent(t *testing.T) {
	e := newScriptEnv(t)
	t.Setenv("OCHAN_TEST_AS_OCHAN", "1")
	t.Setenv("OCHAN_TEST_EXECUTOR", "test-loop")
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-loop", ext)
	t.Cleanup(func() { scenario.Unregister("test-loop") })
	srv := httptest.NewServer(embedded.Handler(embedded.Info{Engine: "own", Model: "own-tiny"}, logicModel{}))
	defer srv.Close()
	mailSender = func(report.MailConfig, report.Mail) error { return nil }
	t.Cleanup(func() { mailSender = report.Send })
	// Дочерние ochan читают настройки из файла.
	for k, v := range map[string]any{"llm.mode": "embedded", "llm.endpoint": srv.URL,
		"incidents.engineers": []string{"dba@corp.example"}, "defaults.smtp_host": "127.0.0.1", "defaults.smtp_port": 1} {
		viper.Set(k, v)
	}
	if err := viper.WriteConfigAs(os.Getenv("OCTOCHAN_CONFIG")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { viper.Set("llm.mode", ""); viper.Set("llm.endpoint", "http://127.0.0.1:8080") })

	// Контур HA-AI и free агента db7.
	e.write(e.path("raise.yaml"), "service: test-loop\nparameters: {limit: 200}\ntargets: [{group: db_new}]\n")
	e.write(e.path("pol.yaml"), "name: pol\nscenario: raise.yaml\ntargets: [group=db_new]\nparams:\n  limit: 100..300\nexpires: 2099-01-01\n")
	e.inventory("hosts:\n  db7: {groups: [db_new], svm_ci: CI007}\n")
	e.must("policy", "add", e.path("pol.yaml"))
	e.must("role", "create", "deployer", "--allow", "read,request,apply")
	e.must("agents", "beat", "--name", "db7")
	e.must("agents", "beat", "--name", "db8")
	path, _ := haStatePath()
	if err := remote.SaveHAState(path, &remote.HAState{Node: "a", Role: remote.RolePrimary, Epoch: 1, Contour: remote.ContourAI}); err != nil {
		t.Fatal(err)
	}
	e.must("agents", "free", "db7", "--role", "deployer", "--for", "4h", "--policy", "pol")

	// Инцидент с задачей и сценарием реализации.
	e.write(e.path("loop.yaml"), "service: test-loop\nparameters: {limit: \"${Approval.limit}\"}\ntargets: [{group: db_new}]\n")
	out := e.must("incident", "open", "--level", "critical", "--host", "db7", "--scenario", e.path("loop.yaml"), loopTask)
	fixed := regexpAfter(t, out, "Инцидент ")
	// Без сценария: решение есть, заявку оформит человек.
	out = e.must("incident", "open", "--level", "low", "--host", "db7", loopTask)
	bare := regexpAfter(t, out, "Инцидент ")
	// Не на языке: своей модели не достаётся.
	out = e.must("incident", "open", "--level", "low", "--host", "db7", "PostgreSQL на db7 не отвечает")
	prose := regexpAfter(t, out, "Инцидент ")
	// Без предпосылки: требование, которого нет в знании задачи.
	out = e.must("incident", "open", "--level", "low", "--host", "db7", loopTask+"\nrequires: store\n")
	undef := regexpAfter(t, out, "Инцидент ")
	// Чужой хост — не задача db7.
	e.must("incident", "open", "--level", "low", "--host", "db9", loopTask)

	// Письмо от агента db8.
	t.Setenv(EnvRole, "llm")
	t.Setenv(EnvAgent, "db8")
	e.must("mail", "send", "--to", "db7", loopTask)
	t.Setenv(EnvRole, "master")
	t.Setenv(EnvAgent, "")

	out = e.must("llm", "run", "--agent", "db7", "--once")
	store, _ := incidentStore()
	i, _ := store.Get(fixed)
	if i.State != workspace.IncidentResolved || !strings.Contains(out, "выполнена без человека (free)") {
		t.Fatalf("инцидент со сценарием: %s\n%s", i.State, out)
	}
	kinds := []string{}
	for _, s := range i.Steps {
		kinds = append(kinds, s.Kind)
	}
	if strings.Join(kinds, ",") != "open,note,request,apply,resolve" || !strings.Contains(i.Steps[1].Text, "set Approval.limit = 200") {
		t.Fatalf("шаги: %v\n%s", kinds, i.Steps[1].Text)
	}
	if i, _ := store.Get(bare); i.State != workspace.IncidentEscalated || !strings.Contains(i.Outcome, "set Failure.check = request") {
		t.Fatalf("без сценария: %s %q", i.State, i.Outcome)
	}
	if i, _ := store.Get(prose); i.State != workspace.IncidentEscalated || !strings.Contains(i.Outcome, "set Failure.fault = incident_"+prose) {
		t.Fatalf("не на языке: %s %q", i.State, i.Outcome)
	}
	if i, _ := store.Get(undef); i.State != workspace.IncidentEscalated || !strings.Contains(i.Outcome, "set Failure.error = store") {
		t.Fatalf("без предпосылки: %s %q", i.State, i.Outcome)
	}
	box, _ := mailbox()
	all, _ := box.List()
	replies := workspace.MessagesFor(all, "db8")
	if len(replies) != 1 || replies[0].FromAgent != "db7" || !strings.Contains(replies[0].Body, "set Approval.limit = 200") {
		t.Fatalf("ответ на письмо: %+v", replies)
	}

	// Второй проход: ничего не берётся дважды.
	again := e.must("llm", "run", "--agent", "db7", "--once")
	if strings.Contains(again, "Инцидент ") || strings.Contains(again, "Письмо ") {
		t.Fatalf("повтор:\n%s", again)
	}
}

// Сценарий получает решённые значения; места без значения — вопрос, а не
// заявка с пустым местом.
func TestRealize(t *testing.T) {
	values := map[string]string{"Config.limit": "300", "Config.buffers": "8 GB"}
	out, missing, err := realize("service: x\nparameters: {limit: \"${Config.limit}\", buffers: \"${Config.buffers}\", note: \"b=${Config.buffers}\"}\n", values)
	if err != nil || len(missing) > 0 || !strings.Contains(out, "limit: 300") || !strings.Contains(out, "buffers: 8 GB") ||
		!strings.Contains(out, "b=8 GB") {
		t.Fatalf("realize: %v %v\n%s", err, missing, out)
	}
	if _, missing, _ := realize("service: x\nparameters: {limit: \"${Config.max}\"}\n", values); len(missing) != 1 || missing[0] != "Config.max" {
		t.Fatalf("нет значения: %v", missing)
	}
}
