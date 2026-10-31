package cmd

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"octochan/fp"
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
	// Письма — естественным языком; формальные — TestIncidentMailFormal.
	viper.Set("mail.lang", "natural")
	t.Cleanup(func() { viper.Set("mail.lang", "auto") })
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
	if out := e.must("incident", "resolve", id[:4], "инженер поднял пул соединений"); !strings.Contains(out, "решён за") ||
		!strings.Contains(out, "Письмо о решении: oncall@corp.example") {
		t.Fatalf("resolve человеком:\n%s", out)
	}
	if out := e.must("incident", id); !strings.Contains(out, "Состояние: решён") || !strings.Contains(out, "escalate") ||
		!strings.Contains(out, "Письмо:    эскалация — oncall@corp.example") ||
		!strings.Contains(out, "Письмо:    решение — oncall@corp.example") {
		t.Fatalf("карточка:\n%s", out)
	}
	// О решении пишут тем же, кому писали об эскалации.
	if len(sent) != 2 || !strings.Contains(sent[1].Subject, "решён человеком (master) за") ||
		!strings.Contains(sent[1].Body, "Эскалация:") || !strings.Contains(sent[1].Body, "resolve: инженер поднял пул соединений") {
		t.Fatalf("письмо о решении человеком: %d\n%+v", len(sent), sent[len(sent)-1])
	}

	// Модель решила вовремя — письмо о решении, без эскалации.
	t.Setenv(EnvRole, "llm")
	out = e.must("incident", "open", "--level", "low", "диск db7 заполнен на 91%")
	other := regexpAfter(t, out, "Инцидент ")
	if out := e.must("incident", "resolve", other, "удалены старые архивы WAL, занято 60%"); !strings.Contains(out, "решён за") {
		t.Fatalf("resolve моделью:\n%s", out)
	}
	if len(sent) != 3 || strings.Join(sentTo[2], ",") != "dba@corp.example" ||
		!strings.Contains(sent[2].Subject, "(low) решён моделью за") || strings.Contains(sent[2].Body, "Эскалация:") {
		t.Fatalf("письмо о решении моделью: %d %v\n%+v", len(sent), sentTo, sent[len(sent)-1])
	}

	// Письма о решении можно выключить.
	viper.Set("incidents.notify_resolved", false)
	out = e.must("incident", "open", "--level", "low", "лишний алерт")
	quiet := regexpAfter(t, out, "Инцидент ")
	e.must("incident", "resolve", quiet, "ложное срабатывание")
	viper.Set("incidents.notify_resolved", true)
	if len(sent) != 3 {
		t.Fatalf("notify_resolved: false, а писем %d", len(sent))
	}

	// Модель сама признала, что не справится.
	out = e.must("incident", "open", "--level", "low", "репликация db7 отстаёт")
	third := regexpAfter(t, out, "Инцидент ")
	if out := e.must("incident", "escalate", third, "причина в сети, а не в конфиге"); !strings.Contains(out, "эскалирован инженерам (llm: причина в сети") {
		t.Fatalf("escalate:\n%s", out)
	}
	if len(sent) != 4 || strings.Join(sentTo[3], ",") != "dba@corp.example" {
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

// Статистика: решённые и нерешённые за период; отчёт — на почту группе
// отчёта, сам — по расписанию incidents.report.every.
func TestIncidentStats(t *testing.T) {
	e := newScriptEnv(t)
	viper.Set("incidents", map[string]any{
		"levels":    map[string]any{"low": "4h", "critical": "15m"},
		"engineers": []string{"dba@corp.example"},
		"report":    map[string]any{"every": "7d", "to": []string{"leads@corp.example", "ops@corp.example"}},
	})
	t.Cleanup(func() { viper.Set("incidents", nil) })
	// Письма — естественным языком; формальные — TestIncidentMailFormal.
	viper.Set("mail.lang", "natural")
	t.Cleanup(func() { viper.Set("mail.lang", "auto") })
	var sent []report.Mail
	var sentTo [][]string
	mailSender = func(cfg report.MailConfig, mail report.Mail) error {
		sentTo = append(sentTo, cfg.To)
		sent = append(sent, mail)
		return nil
	}
	t.Cleanup(func() { mailSender = report.Send })

	store, _ := incidentStore()
	now := time.Now().UTC()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, i := range []*workspace.Incident{
		{Level: "low", Timer: "4h", Opened: ago(3 * time.Hour), Summary: "диск db7 заполнен на 91%",
			Resolved: ago(3*time.Hour - 4*time.Minute), ResolvedBy: "llm"},
		{Level: "low", Timer: "4h", Opened: ago(2 * time.Hour), Summary: "отстаёт реплика db8",
			Resolved: ago(2*time.Hour - 2*time.Minute), ResolvedBy: "llm"},
		{Level: "critical", Timer: "15m", Opened: ago(90 * time.Minute), Summary: "PostgreSQL на db7 не принимает соединения",
			Escalated: ago(75 * time.Minute), EscalatedBy: "ochan", Reason: "время вышло: модель не решила за 15m",
			Resolved: ago(15 * time.Minute), ResolvedBy: "master"},
		{Level: "critical", Timer: "15m", Opened: ago(time.Hour), Summary: "кластер db9 разделился",
			Escalated: ago(55 * time.Minute), EscalatedBy: "llm", Reason: "llm: причина в сети"},
		{Level: "low", Timer: "4h", Opened: ago(10 * time.Minute), Summary: "медленные запросы на db7"},
		{Level: "low", Timer: "4h", Opened: ago(30 * 24 * time.Hour), Summary: "давний инцидент",
			Resolved: ago(30*24*time.Hour - time.Minute), ResolvedBy: "llm"},
	} {
		if err := store.Create(i); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update(i.ID, func(s *workspace.Incident) error {
			s.Deadline = s.Opened.Add(4 * time.Hour)
			switch {
			case !s.Resolved.IsZero():
				s.State = workspace.IncidentResolved
			case !s.Escalated.IsZero():
				s.State = workspace.IncidentEscalated
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	out := e.must("incident", "stats")
	for _, want := range []string{"(7d): 5", "Решены:                       3",
		"  моделью до срока            2   в среднем 3m, медиана 3m", "  людьми после эскалации      1   в среднем 1h, медиана 1h после эскалации",
		"Не решены:                    2", "  у инженеров                 1", "  у модели, срок не вышел     1",
		"Модель решила сама 2 из 5 (40%), не справилась — 2 (40%)", "  срок вышел                  1",
		"  отдала инженерам сама       1", "кластер db9 разделился", "медленные запросы на db7",
		"Отчёт: каждые 7d → leads@corp.example, ops@corp.example; начнётся при первой проверке"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в статистике нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "давний инцидент") {
		t.Fatalf("инцидент вне периода:\n%s", out)
	}
	if out := e.must("incident", "stats", "--for", "all"); !strings.Contains(out, "(всё время): 6") {
		t.Fatalf("за всё время:\n%s", out)
	}
	e.fails("--for \"неделя\": срок вида 24h, 7d или all", "incident", "stats", "--for", "неделя")

	// Сейчас — руками.
	if out := e.must("incident", "report", "--for", "24h"); !strings.Contains(out, "Отчёт по инцидентам за 1d ушёл: leads@corp.example, ops@corp.example") {
		t.Fatalf("report:\n%s", out)
	}
	if len(sent) != 1 || !strings.Contains(sent[0].Subject, "Инциденты за 1d") ||
		!strings.Contains(sent[0].Subject, ": 5, решила модель 2, у инженеров 1, открыто 1") ||
		!strings.Contains(sent[0].Body, "Модель решила сама 2 из 5") || len(sent[0].Attachments) != 1 {
		t.Fatalf("письмо отчёта: %+v", sent)
	}
	csv := string(sent[0].Attachments[0].Data)
	if !strings.HasPrefix(csv, "id,level,timer,opened,") || strings.Count(csv, "\n") != 6 ||
		!strings.Contains(csv, ",timeout,resolved,") || !strings.Contains(csv, ",gave_up,escalated,") {
		t.Fatalf("csv:\n%s", csv)
	}

	// По расписанию: первая проверка начинает его, отчёт — через период.
	if out := e.must("incident", "check"); !strings.Contains(out, "Отчёт по инцидентам: каждые 7d, первый —") {
		t.Fatalf("check:\n%s", out)
	}
	e.must("incident", "check")
	if len(sent) != 1 {
		t.Fatalf("отчёт раньше срока: %d", len(sent))
	}
	r, _ := store.Report()
	r.Since = r.Since.Add(-7*24*time.Hour - time.Minute)
	writeState(t, r)
	if out := e.must("incident", "check"); !strings.Contains(out, "Отчёт по инцидентам ушёл: leads@corp.example, ops@corp.example; следующий —") {
		t.Fatalf("check по сроку:\n%s", out)
	}
	if len(sent) != 2 || !strings.Contains(sent[1].Subject, "Инциденты за 7d") {
		t.Fatalf("отчёт по расписанию: %+v", sent[len(sent)-1].Subject)
	}
	if out := e.must("incident", "stats"); !strings.Contains(out, "; последний ушёл ") {
		t.Fatalf("расписание в статистике:\n%s", out)
	}

	// Не ушёл — ошибка видна, окно не сдвинулось.
	mailSender = func(report.MailConfig, report.Mail) error { return errors.New("smtp: 554 отказ") }
	r, _ = store.Report()
	r.Since = r.Since.Add(-7 * 24 * time.Hour)
	writeState(t, r)
	e.fails("отчёт по инцидентам не ушёл: smtp: 554 отказ; повтор после", "incident", "check")
	if out := e.must("incident", "stats"); !strings.Contains(out, "; последний не ушёл: smtp: 554 отказ; повтор после") {
		t.Fatalf("ошибка в статистике:\n%s", out)
	}

	// На standby ни отчётов, ни эскалаций: это дело primary, cron на обоих
	// узлах не шлёт письма дважды.
	mailSender = func(cfg report.MailConfig, mail report.Mail) error {
		sent = append(sent, mail)
		return nil
	}
	path, _ := haStatePath()
	if err := remote.SaveHAState(path, &remote.HAState{Node: "b", Role: remote.RoleStandby, Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	r, _ = store.Report()
	r.Since, r.Error = r.Since.Add(-7*24*time.Hour), ""
	writeState(t, r)
	before := len(sent)
	open := e.must("incident", "open", "--level", "critical", "реплика отстаёт")
	late := regexpAfter(t, open, "Инцидент ")
	if _, err := store.Update(late, func(i *workspace.Incident) error {
		i.Deadline = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.must("incident", "check")
	e.must("incident", "stats")
	if i, _ := store.Get(late); len(sent) != before || i.State != workspace.IncidentOpen {
		t.Fatalf("standby: писем %d, инцидент %s", len(sent)-before, i.State)
	}
}

func writeState(t *testing.T, r *workspace.IncidentReport) {
	t.Helper()
	fm, _ := mustFileManager()
	data, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(fm.BaseDir(), incidentsDir, "report.state"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Людям пишут формальным промптом — письма ochan тоже на языке: факты —
// ячейками, тексты мониторинга и модели — вложением.
func TestIncidentMailFormal(t *testing.T) {
	newScriptEnv(t)
	viper.Set("incidents", map[string]any{
		"levels":    map[string]any{"low": "4h", "mc+": "15m"},
		"engineers": []string{"dba@corp.example"},
	})
	t.Cleanup(func() { viper.Set("incidents", nil) })
	var sent []report.Mail
	mailSender = func(_ report.MailConfig, mail report.Mail) error {
		sent = append(sent, mail)
		return nil
	}
	t.Cleanup(func() { mailSender = report.Send })

	store, _ := incidentStore()
	now := time.Now().UTC()
	i := &workspace.Incident{Level: "mc+", Timer: "15m", Opened: now.Add(-time.Hour), Source: "мониторинг zabbix",
		Hosts: []string{"web-1.example", "db7"}, Summary: "PostgreSQL на db7 не принимает соединения",
		Escalated: now.Add(-45 * time.Minute), EscalatedBy: "ochan", Reason: "время вышло: модель не решила за 15m",
		Resolved: now, ResolvedBy: "master", Outcome: "инженер поднял пул"}
	if err := store.Create(i); err != nil {
		t.Fatal(err)
	}
	i, _ = store.Update(i.ID, func(i *workspace.Incident) error {
		i.State = workspace.IncidentResolved
		i.Step("llm", "note", "max_connections исчерпан", "")
		return i.Attach("pg.log", []byte("FATAL: too many clients\n"))
	})

	if err := sendResolution(i, []string{"dba@corp.example"}); err != nil {
		t.Fatal(err)
	}
	m := sent[0]
	if r := fp.CheckText("письмо", m.Body, fp.Options{}); !r.InLanguage() {
		t.Fatalf("письмо о решении не на языке: %v\n%s", r.Syntax(), m.Body)
	}
	for _, want := range []string{"goal: failure", "set Failure.incident = incident_" + i.ID, "set Failure.level = mc",
		"set Failure.timer = 15 min", "set Failure.result = timeout", "set Failure.host = web_1_example",
		"set Failure.host_2 = db7", "set Failure.took = 1 h", "set Log.pg_log = 24 B"} {
		if !strings.Contains(m.Body, want) {
			t.Fatalf("в письме нет %q:\n%s", want, m.Body)
		}
	}
	if strings.Contains(m.Body, "PostgreSQL") || m.Subject != "[ochan] goal: failure — incident "+i.ID+" mc+ resolved timeout" ||
		len(m.Attachments) != 2 || m.Attachments[0].Name != "incident-"+i.ID+".txt" ||
		!strings.Contains(string(m.Attachments[0].Data), "summary: PostgreSQL на db7 не принимает соединения") {
		t.Fatalf("письмо: %s\n%s\n%v", m.Subject, m.Body, m.Attachments)
	}

	viper.Set("incidents.report.to", []string{"leads@corp.example"})
	end := now.Add(time.Minute)
	if _, err := sendIncidentReport(end.Add(-24*time.Hour), end, "1d"); err != nil {
		t.Fatal(err)
	}
	m = sent[1]
	if r := fp.CheckText("отчёт", m.Body, fp.Options{}); !r.InLanguage() {
		t.Fatalf("отчёт не на языке: %v\n%s", r.Syntax(), m.Body)
	}
	for _, want := range []string{"goal: count", "set Period.length = 1 d", "set Count.total = 1",
		"set Count.resolved_after_escalation = 1", "set Level.mc_escalated = 1"} {
		if !strings.Contains(m.Body, want) {
			t.Fatalf("в отчёте нет %q:\n%s", want, m.Body)
		}
	}
	if !strings.HasPrefix(m.Subject, "[ochan] goal: count — incidents 1d ") {
		t.Fatalf("тема отчёта: %s", m.Subject)
	}
}
