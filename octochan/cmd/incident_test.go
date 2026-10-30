package cmd

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"octochan/remote"
	"octochan/report"
	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/viper"
)

// Модель чинит до срока уровня; не успела — инцидент уходит инженерам
// письмом с шагами и журналами, и модель по нему больше не действует.
func TestIncidentEscalation(t *testing.T) {
	e := newScriptEnv(t)
	ext := &extExecutor{remote: "v1"}
	scenario.Register("test-inc", ext)
	t.Cleanup(func() { scenario.Unregister("test-inc") })
	viper.Set("incidents", map[string]any{
		"levels":    map[string]any{"low": "4h", "critical": "15m"},
		"engineers": []string{"dba@corp.example"},
		"notify":    map[string]any{"critical": []string{"oncall@corp.example"}},
	})
	t.Cleanup(func() { viper.Set("incidents", nil) })
	var sent []report.Mail
	var sentTo [][]string
	mailSender = func(cfg report.MailConfig, mail report.Mail) error {
		sentTo = append(sentTo, cfg.To)
		sent = append(sent, mail)
		return nil
	}
	t.Cleanup(func() { mailSender = report.Send })

	// free для модели — заявка по инциденту выполняется без человека.
	e.write(e.path("raise.yaml"), "service: test-inc\nparameters: {limit: 200}\ntargets: [{group: db_new}]\n")
	e.write(e.path("pol.yaml"), "name: pol\nscenario: raise.yaml\ntargets: [group=db_new]\nparams:\n  limit: 100..300\nexpires: 2099-01-01\n")
	e.inventory("hosts:\n  db7: {groups: [db_new], svm_ci: CI007}\n")
	e.must("policy", "add", e.path("pol.yaml"))
	e.must("role", "create", "deployer", "--allow", "read,request,apply")
	e.must("agents", "beat", "--name", "db7")
	path, _ := haStatePath()
	if err := remote.SaveHAState(path, &remote.HAState{Node: "a", Role: remote.RolePrimary, Epoch: 1, Contour: remote.ContourAI}); err != nil {
		t.Fatal(err)
	}
	e.must("agents", "free", "db7", "--role", "deployer", "--for", "4h", "--policy", "pol")

	t.Setenv(EnvRole, "llm")
	e.fails("уровня \"urgent\" нет; уровни: low (4h), critical (15m)", "incident", "open", "--level", "urgent", "x")
	out := e.must("incident", "open", "--level", "critical", "--host", "db7", "PostgreSQL на db7 не принимает соединения")
	id := regexpAfter(t, out, "Инцидент ")
	if !strings.Contains(out, "(critical) открыт: модель чинит сама до") || !strings.Contains(out, "oncall@corp.example") {
		t.Fatalf("incident open:\n%s", out)
	}
	e.must("incident", "note", id, "max_connections исчерпан: 200 из 200")
	e.write(e.path("pg.log"), "2026-10-30 14:02:01 LOG:  checkpoint\n2026-10-30 14:02:03 FATAL:  sorry, too many clients already\n")
	e.must("incident", "attach", id, e.path("pg.log"))
	out = e.must("request", "raise.yaml", "--incident", id)
	reqID := regexpID(t, out)
	if !strings.Contains(out, "без человека") || len(ext.applied) != 1 {
		t.Fatalf("заявка по инциденту под free:\n%s", out)
	}

	// Срок вышел — эскалация при следующем обращении.
	store, _ := incidentStore()
	if _, err := store.Update(id, func(i *workspace.Incident) error {
		i.Deadline = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.fails("эскалирован инженерам (время вышло: модель не решила за 15m): модель по нему больше не действует",
		"incident", "note", id, "ещё попытка")
	if len(sent) != 1 || strings.Join(sentTo[0], ",") != "oncall@corp.example" {
		t.Fatalf("письмо: %d, кому %v", len(sent), sentTo)
	}
	body := sent[0].Body
	for _, want := range []string{"Инцидент " + id + " — эскалация инженерам", "Уровень:  critical (модель чинит сама 15m)",
		"PostgreSQL на db7 не принимает соединения", "max_connections исчерпан", "request " + reqID,
		"FATAL:  sorry, too many clients already", "Журнал аудита за время инцидента"} {
		if !strings.Contains(body, want) {
			t.Fatalf("в письме нет %q:\n%s", want, body)
		}
	}
	names := []string{}
	for _, a := range sent[0].Attachments {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "incident-"+id+".docx,pg.log" {
		t.Fatalf("вложения: %v", names)
	}

	// Модель по эскалированному больше не действует: ни шагов, ни заявок,
	// ни решения.
	e.fails("модель по нему больше не действует", "request", "raise.yaml", "--incident", id)
	e.fails("модель по нему больше не действует", "incident", "resolve", id, "починила")
	// Человек закрывает его сам.
	t.Setenv(EnvRole, "master")
	if out := e.must("incident", "resolve", id, "инженер поднял пул соединений"); !strings.Contains(out, "решён за") {
		t.Fatalf("resolve человеком:\n%s", out)
	}
	if out := e.must("incident", id); !strings.Contains(out, "Состояние: решён") || !strings.Contains(out, "escalate") ||
		!strings.Contains(out, "Письмо:    oncall@corp.example") {
		t.Fatalf("карточка:\n%s", out)
	}

	// Модель решила вовремя — письма нет.
	t.Setenv(EnvRole, "llm")
	out = e.must("incident", "open", "--level", "low", "диск db7 заполнен на 91%")
	other := regexpAfter(t, out, "Инцидент ")
	if out := e.must("incident", "resolve", other, "удалены старые архивы WAL, занято 60%"); !strings.Contains(out, "решён за") {
		t.Fatalf("resolve моделью:\n%s", out)
	}
	if len(sent) != 1 {
		t.Fatalf("письмо о решённом: %d", len(sent))
	}

	// Модель сама признала, что не справится.
	out = e.must("incident", "open", "--level", "low", "репликация db7 отстаёт")
	third := regexpAfter(t, out, "Инцидент ")
	if out := e.must("incident", "escalate", third, "причина в сети, а не в конфиге"); !strings.Contains(out, "эскалирован инженерам (llm: причина в сети") {
		t.Fatalf("escalate:\n%s", out)
	}
	if len(sent) != 2 || strings.Join(sentTo[1], ",") != "dba@corp.example" {
		t.Fatalf("письмо уровня low: %v", sentTo)
	}
}

// Вебхук мониторинга открывает инцидент с журналом.
func TestIncidentWebhook(t *testing.T) {
	e := newScriptEnv(t)
	e.must("incident", "secret")
	fm, _ := mustFileManager()
	secret, _ := os.ReadFile(filepath.Join(fm.BaseDir(), incidentsSecret))
	h := incidentsHandler(func() (bool, string) { return true, "" })
	post := func(token, body string) (int, string) {
		req := httptest.NewRequest("POST", "/v1/incidents", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := post("чужой", `{}`); code != 401 {
		t.Fatalf("чужой секрет: %d", code)
	}
	code, body := post(strings.TrimSpace(string(secret)),
		`{"level":"high","hosts":["db7"],"summary":"нет соединений","source":"zabbix","log":"FATAL: too many clients"}`)
	if code != 200 || !strings.Contains(body, "(high) открыт: модель чинит до") {
		t.Fatalf("webhook: %d %s", code, body)
	}
	out := e.must("incident")
	if !strings.Contains(out, "high") || !strings.Contains(out, "нет соединений") {
		t.Fatalf("incident:\n%s", out)
	}
	store, _ := incidentStore()
	all, _ := store.List()
	if len(all) != 1 || all[0].Source != "мониторинг zabbix" || len(all[0].Logs) != 1 {
		t.Fatalf("инцидент: %+v", all)
	}
}

func regexpAfter(t *testing.T, out, prefix string) string {
	t.Helper()
	i := strings.Index(out, prefix)
	if i < 0 {
		t.Fatalf("нет %q:\n%s", prefix, out)
	}
	rest := out[i+len(prefix):]
	return strings.Fields(rest)[0]
}
