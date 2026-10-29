package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/viper"
)

// Агент сообщает о себе — сервер помнит его, и реестр становится
// источником инвентаря: новый агент для watch — новый хост.
func TestAgentsRegistry(t *testing.T) {
	e := newScriptEnv(t)
	t.Setenv(EnvRemoteAddr, "10.0.0.7")
	out := e.must("agents", "beat", "--name", "db7", "--group", "db,db_new", "--var", "svm_ci=CI007", "--every", "1m")
	if !strings.Contains(out, "Агент db7 зарегистрирован (10.0.0.7)") {
		t.Fatalf("beat:\n%s", out)
	}
	if out := e.must("agents", "beat", "--name", "db7", "--group", "db,db_new", "--var", "svm_ci=CI007"); !strings.Contains(out, "на связи") {
		t.Fatalf("второй beat:\n%s", out)
	}
	if list := e.must("agents"); !strings.Contains(list, "db7") || !strings.Contains(list, "на связи") ||
		!strings.Contains(list, "10.0.0.7") {
		t.Fatalf("agents:\n%s", list)
	}

	// Реестр — источник инвентаря.
	viper.Set("inventory.sources", map[string]string{"agents": "agents://online"})
	t.Cleanup(func() { viper.Set("inventory.sources", nil) })
	if inv := e.must("inventory"); !strings.Contains(inv, "db7") || !strings.Contains(inv, "agent_address=10.0.0.7") {
		t.Fatalf("inventory:\n%s", inv)
	}
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-agents", ext)
	t.Cleanup(func() { scenario.Unregister("test-agents") })
	e.write(e.path("tune.yaml"), "service: test-agents\nparameters: {limit: 200}\ntargets: [{group: db_new}]\n")
	if out := e.must("watch", "--source", "agents", "--scenario", "tune.yaml"); !strings.Contains(out, "db7: заявка") {
		t.Fatalf("watch по агентам:\n%s", out)
	}

	// Имя принадлежит роли, в которой агент пришёл первым.
	e.must("role", "create", "agent2", "--allow", "agent")
	t.Setenv(EnvRole, "agent2")
	e.fails("имя агента db7 занято ролью master", "agents", "beat", "--name", "db7")
	e.fails("роль agent2 не может выполнять agents forget", "agents", "forget", "db7")
	os.Unsetenv(EnvRole)

	// Давно молчащий агент — не на связи и не в agents://online.
	path, _ := agentsPath()
	reg, _ := loadAgents(path)
	reg.Agents["db7"].Last = time.Now().Add(-time.Hour)
	data, _ := json.Marshal(reg)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if list := e.must("agents"); !strings.Contains(list, "нет связи") {
		t.Fatalf("молчащий агент:\n%s", list)
	}
	if inv, _ := e.ochan("inventory"); strings.Contains(inv, "db7") {
		t.Fatalf("молчащий агент в agents://online:\n%s", inv)
	}
	e.must("agents", "forget", "db7")
	if list := e.must("agents"); !strings.Contains(list, "Агентов нет") {
		t.Fatalf("после forget:\n%s", list)
	}
}

// Одну заявку два процесса не выполнят; пропавший процесс оставляет исход
// неизвестным — заявка упала.
func TestRequestRunning(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("changes.conf"), "max_connections = 200\n")
	newRequest := func() *workspace.Request {
		id := requestIDPattern.FindStringSubmatch(e.must("request", "prod.conf", "changes.conf"))[1]
		e.must("approve", id)
		store, _ := requestStore()
		r, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	host, _ := os.Hostname()
	store, _ := requestStore()

	busy := newRequest()
	if err := store.MarkRunning(busy, workspace.RunnerMark{Host: host, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning(busy, workspace.RunnerMark{Host: host, PID: 1}); err == nil {
		t.Fatal("заявку взяли в работу дважды")
	}
	e.fails("заявка "+busy.ID+" уже выполняется", "apply", "--request", busy.ID)

	gone := newRequest()
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skip(err)
	}
	if err := store.MarkRunning(gone, workspace.RunnerMark{Host: host, PID: dead.ProcessState.Pid()}); err != nil {
		t.Fatal(err)
	}
	e.fails("исход неизвестен", "apply", "--request", gone.ID)
	if card := e.must("requests", gone.ID); !strings.Contains(card, "Состояние: упала") {
		t.Fatalf("после пропавшего процесса:\n%s", card)
	}
}

// ochan agent run на машине агента: вход в роль agent и beat на сервере.
func TestAgentRunOnce(t *testing.T) {
	s := startServer(t)
	s.must("role", "create", "agent", "--allow", "agent")
	out := s.must("agent", "run", "--server", s.addr, "--name", "db9", "--group", "db", "--once")
	if !strings.Contains(out, "Агент db9 зарегистрирован (127.0.0.1") {
		t.Fatalf("agent run:\n%s", out)
	}
	if list := s.must("agents"); !strings.Contains(list, "db9") || !strings.Contains(list, "роль agent") {
		t.Fatalf("agents:\n%s", list)
	}
	// Роль агента ничего, кроме beat, не может.
	if _, err := s.ochan("remote", "--role", "agent", s.addr, "requests"); err == nil {
		t.Fatal("роль agent читает заявки")
	}
}
