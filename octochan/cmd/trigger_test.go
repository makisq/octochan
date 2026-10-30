package cmd

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/viper"
)

// Роль под триггером работает только в окне события и только над целями
// его области; окно гаснет само по max_window, и журнал это помнит.
func TestTriggerWindow(t *testing.T) {
	e := newScriptEnv(t)
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-trigger", ext)
	t.Cleanup(func() { scenario.Unregister("test-trigger") })
	viper.Set("triggers", map[string]any{"release": map[string]any{
		"event": "ci/release",
		"grant": map[string]any{"role": "deployer", "scope": []string{"group=prod"}, "max_window": "2h"},
	}})
	t.Cleanup(func() { viper.Set("triggers", nil) })

	e.write(e.path("tune.yaml"), "service: test-trigger\nparameters: {limit: 200}\ntargets: [{group: db}]\n")
	e.inventory("hosts:\n  db1: {groups: [db, prod], svm_ci: CI001}\n  db2: {groups: [db, stage], svm_ci: CI002}\n")
	e.must("role", "create", "deployer", "--allow", "read,plan,request")
	e.must("role", "create", "ci", "--allow", "trigger")

	as := func(role string) { t.Setenv(EnvRole, role) }

	as("deployer")
	e.fails("роль deployer действует только в окне триггера (release): не активна — ждёт события", "plan", "tune.yaml")
	if out := e.must("role", "show", "deployer"); !strings.Contains(out, "Триггеры:  release (событие ci/release, окно до 2h)") ||
		!strings.Contains(out, "Сейчас:    не активна — ждёт события") {
		t.Fatalf("role show вне окна:\n%s", out)
	}

	as("ci")
	e.fails("роль ci не может выполнять plan", "plan", "tune.yaml")
	out := e.must("trigger", "fire", "release", "--id", "1234")
	if !strings.Contains(out, "Окно release#1234: роль deployer активна до") || !strings.Contains(out, "цели — group=prod") {
		t.Fatalf("trigger fire:\n%s", out)
	}
	e.fails("то же событие второй раз окна не открывает", "trigger", "fire", "release", "--id", "1234")

	// В окне: цели только под областью.
	as("deployer")
	e.fails("цель db2 вне области окна release#1234 (group=prod)", "plan", "tune.yaml")
	if out := e.must("plan", "tune.yaml", "--limit", "group=prod"); !strings.Contains(out, "CI001") {
		t.Fatalf("plan в области:\n%s", out)
	}
	out = e.must("request", "tune.yaml", "--limit", "group=prod")
	id := requestIDPattern.FindStringSubmatch(out)[1]
	if out := e.must("requests", "--after", "release#1234"); !strings.Contains(out, id) ||
		!strings.Contains(out, "Окно release#1234: роль deployer") {
		t.Fatalf("requests --after окна:\n%s", out)
	}
	if out := e.must("role", "show", "deployer"); !strings.Contains(out, "Сейчас:    активна (release#1234, с ") {
		t.Fatalf("role show в окне:\n%s", out)
	}

	// Окно истекло: роль гаснет сама.
	fm, _ := mustFileManager()
	path := filepath.Join(fm.BaseDir(), triggersFile)
	var windows []workspace.TriggerWindow
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &windows); err != nil || len(windows) != 1 {
		t.Fatalf("окна: %v %s", err, data)
	}
	windows[0].Until = time.Now().Add(-time.Minute)
	data, _ = json.Marshal(windows)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	e.fails("не активна — окно release#1234 истекло (max_window 2h)", "plan", "tune.yaml", "--limit", "group=prod")

	as(rbacMasterForTest)
	audit := e.must("audit", "-n", "0")
	for _, want := range []string{"trigger: release#1234 → роль deployer выдана: group=prod, до",
		"trigger: release#1234 истекло в", "роль deployer снята (max_window 2h)"} {
		if !strings.Contains(audit, want) {
			t.Fatalf("в журнале аудита нет %q:\n%s", want, audit)
		}
	}

	// Событие конца закрывает окно раньше.
	as("ci")
	e.must("trigger", "fire", "release", "--id", "1235")
	if out := e.must("trigger", "end", "release", "--id", "1235", "--reason", "релиз выкачен"); !strings.Contains(out, "Окно release#1235 закрыто: роль deployer снята") {
		t.Fatalf("trigger end:\n%s", out)
	}
	as("deployer")
	e.fails("окно release#1235 закрыто: релиз выкачен", "plan", "tune.yaml", "--limit", "group=prod")
	if out := e.must("role", "show"); !strings.Contains(out, "deployer") {
		t.Fatalf("role show доступен и вне окна:\n%s", out)
	}

	as(rbacMasterForTest)
	if out := e.must("trigger"); !strings.Contains(out, "release          событие ci/release → роль deployer, group=prod, окно до 2h") ||
		!strings.Contains(out, "release#1235") {
		t.Fatalf("trigger:\n%s", out)
	}
}

const rbacMasterForTest = "master"

func TestTriggerConfig(t *testing.T) {
	newScriptEnv(t)
	t.Cleanup(func() { viper.Set("triggers", nil) })
	for conf, want := range map[string]map[string]any{
		"grant.role":       {"grant": map[string]any{"max_window": "1h"}},
		"обязателен":       {"grant": map[string]any{"role": "deployer"}},
		"у неё свои":       {"grant": map[string]any{"role": "master", "max_window": "1h"}},
		"grant.scope":      {"grant": map[string]any{"role": "deployer", "max_window": "1h", "scope": []string{"group="}}},
		"grant.max_window": {"grant": map[string]any{"role": "deployer", "max_window": "вечно"}},
	} {
		viper.Set("triggers", map[string]any{"release": want})
		if _, err := triggerConfs(); err == nil || !strings.Contains(err.Error(), conf) {
			t.Errorf("%v: %v", want, err)
		}
	}
}

// Вебхук: событие CI без клиента ochan — с секретом триггера и только на
// primary.
func TestTriggerWebhook(t *testing.T) {
	e := newScriptEnv(t)
	viper.Set("triggers", map[string]any{"release": map[string]any{
		"grant": map[string]any{"role": "deployer", "max_window": "30m"},
	}})
	t.Cleanup(func() { viper.Set("triggers", nil) })
	e.must("role", "create", "deployer", "--allow", "read")
	out := e.must("trigger", "secret", "release")
	if !strings.Contains(out, "release.secret (права 0600)") {
		t.Fatalf("trigger secret:\n%s", out)
	}
	path, _ := triggerSecretPath("release")
	secret, _ := os.ReadFile(path)

	writable := true
	h := eventsHandler(func() (bool, string) { return writable, "узел b — standby" })
	post := func(token, body string) (int, string) {
		req := httptest.NewRequest("POST", "/v1/events/release", strings.NewReader(body))
		req.SetPathValue("trigger", "release")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := post("чужой", `{"id":"7","state":"start"}`); code != 401 {
		t.Fatalf("чужой секрет: %d", code)
	}
	token := strings.TrimSpace(string(secret))
	writable = false
	if code, body := post(token, `{"id":"7","state":"start"}`); code != 503 || !strings.Contains(body, "standby") {
		t.Fatalf("standby: %d %s", code, body)
	}
	writable = true
	if code, body := post(token, `{"id":"7","state":"start"}`); code != 200 || !strings.Contains(body, "release#7: роль deployer активна") {
		t.Fatalf("start: %d %s", code, body)
	}
	if code, body := post(token, `{"id":"7","state":"end","reason":"готово"}`); code != 200 || !strings.Contains(body, "не активна — окно release#7 закрыто: готово") {
		t.Fatalf("end: %d %s", code, body)
	}
	if code, _ := post(token, `{"id":"7","state":"start"}`); code != 409 {
		t.Fatalf("повтор события: %d", code)
	}
}
