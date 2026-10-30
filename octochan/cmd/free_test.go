package cmd

import (
	"strings"
	"testing"
	"time"

	"octochan/remote"
	"octochan/scenario"
	"octochan/workspace"
)

// free: только master, только primary контура HA-AI. Заявка модели в
// политике выполняется без человека от имени роли выдачи; вне — ждёт master.
func TestFreeInAIContour(t *testing.T) {
	e := newScriptEnv(t)
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-free", ext)
	t.Cleanup(func() { scenario.Unregister("test-free") })
	e.write(e.path("tune.yaml"), "service: test-free\nparameters: {limit: 200, previous: 100}\ntargets: [{group: db_new}]\n")
	e.write(e.path("wide.yaml"), "service: test-free\nparameters: {limit: 900, previous: 100}\ntargets: [{group: db_new}]\n")
	e.write(e.path("new-servers.yaml"), "name: new-servers\nscenario: tune.yaml\ntargets: [group=db_new]\n"+
		"params:\n  limit: 100..300\n  previous: \"*\"\nexpires: 2099-01-01\n")
	e.inventory("hosts:\n  db7: {groups: [db_new], svm_ci: CI007}\n")
	e.must("policy", "add", e.path("new-servers.yaml"))
	e.must("role", "create", "deployer", "--allow", "read,request,apply")
	e.must("agents", "beat", "--name", "db7", "--group", "db_new")

	grant := []string{"agents", "free", "db7", "--role", "deployer", "--for", "4h", "--policy", "new-servers"}
	e.fails("free доступен только на узлах с HA-AI (контур без человека): узел не в паре", grant...)

	path, _ := haStatePath()
	setNode := func(role, contour string) {
		t.Helper()
		if err := remote.SaveHAState(path, &remote.HAState{Node: "a", Role: role, Epoch: 1, Since: time.Now(), Contour: contour}); err != nil {
			t.Fatal(err)
		}
	}
	setNode(remote.RolePrimary, "")
	e.fails("узел a не в контуре HA-AI", grant...)
	setNode(remote.RolePrimary, remote.ContourAI)

	t.Setenv(EnvRole, "deployer")
	e.fails("роль deployer не может выполнять agents free", grant...)
	t.Setenv(EnvRole, "master")
	e.fails("free не передаётся: master — человек", "agents", "free", "db7", "--role", "llm", "--for", "1h", "--policy", "new-servers")
	e.fails("--policy обязателен", "agents", "free", "db7", "--role", "deployer", "--for", "1h")
	out := e.must(grant...)
	if !strings.Contains(out, "Агент db7: isolation free (до ") || !strings.Contains(out, "роль deployer, политика new-servers)") {
		t.Fatalf("agents free:\n%s", out)
	}
	if out := e.must("agents"); !strings.Contains(out, "isolation free (до ") {
		t.Fatalf("agents:\n%s", out)
	}

	// Модель: в политике — выполнено без человека, от имени deployer.
	t.Setenv(EnvRole, "llm")
	out = e.must("request", "tune.yaml")
	id := regexpID(t, out)
	if !strings.Contains(out, "в free агента db7 (политика new-servers) — выполняется без человека, от имени роли deployer") ||
		len(ext.applied) != 1 {
		t.Fatalf("заявка модели в политике:\n%s", out)
	}
	store, _ := requestStore()
	r, _ := store.Get(id)
	if r.State != workspace.RequestApplied || r.Free != "db7" || !strings.Contains(r.DecidedBy, "free: агент db7") {
		t.Fatalf("заявка: %s, free %q, решил %q", r.State, r.Free, r.DecidedBy)
	}

	// Вне политики — обычная заявка: ждёт master.
	out = e.must("request", "wide.yaml")
	wide := regexpID(t, out)
	if !strings.Contains(out, "вне free: db7 (new-servers): параметр limit = 900 вне допустимого (100..300)") ||
		!strings.Contains(out, "ждёт master") || len(ext.applied) != 1 {
		t.Fatalf("вне политики:\n%s", out)
	}
	if r, _ := store.Get(wide); r.State != workspace.RequestPending {
		t.Fatalf("заявка вне политики: %s", r.State)
	}
	e.fails("не может выполнять approve", "approve", wide)

	// Обратное действие своей заявки — тоже без человека.
	out = e.must("compensate", id)
	if len(ext.undone) != 1 {
		t.Fatalf("обратное действие под free:\n%s", out)
	}

	t.Setenv(EnvRole, "master")
	audit := e.must("audit", "-n", "0")
	for _, want := range []string{"free: agent db7 free включён на a: роль deployer, политика new-servers",
		"approve " + id + " (free: политика new-servers, без человека)", "apply " + id + " (free: политика new-servers, без человека)",
		"request " + wide + " — ждёт master"} {
		if !strings.Contains(audit, want) {
			t.Fatalf("в журнале аудита нет %q:\n%s", want, audit)
		}
	}

	// На standby free не действует; отзыв гасит его.
	setNode(remote.RoleStandby, remote.ContourAI)
	t.Setenv(EnvRole, "llm")
	if out := e.must("request", "tune.yaml"); strings.Contains(out, "free") || len(ext.applied) != 1 {
		t.Fatalf("free на standby:\n%s", out)
	}
	setNode(remote.RolePrimary, remote.ContourAI)
	t.Setenv(EnvRole, "master")
	e.must("agents", "free", "db7", "--revoke")
	t.Setenv(EnvRole, "llm")
	if out := e.must("request", "tune.yaml"); strings.Contains(out, "free") || len(ext.applied) != 1 {
		t.Fatalf("free после отзыва:\n%s", out)
	}
}
