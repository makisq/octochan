package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"octochan/audit"
	"octochan/i18n"
	"octochan/logs"
	"octochan/rbac"
	"octochan/remote"
	"octochan/report"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Инциденты контура HA-AI.
//
//	incidents:
//	  levels:                 # уровень: сколько модель чинит сама
//	    low: 4h
//	    medium: 1h
//	    high: 30m
//	    critical: 15m
//	  engineers: [dba@corp.example, oncall@corp.example]
//	  notify:                 # свои адреса уровня — вместо engineers
//	    critical: [oncall@corp.example, head@corp.example]
//
// Инцидент открывает мониторинг (вебхук POST /v1/incidents или ochan
// incident open) или сама модель. Модель чинит: её заметки, заявки и их
// выполнение — шаги инцидента. Решила до срока — incident resolve. Не
// успела или признала, что не справится (incident escalate), — инцидент
// эскалируется: инженерной группе уходит письмо с описанием, шагами,
// журналами и документом .docx, а модель по нему больше не действует.

const incidentsDir = "incidents"

// incidentsSecret — секрет вебхука мониторинга.
const incidentsSecret = "incidents.secret"

var defaultIncidentLevels = map[string]string{"low": "4h", "medium": "1h", "high": "30m", "critical": "15m"}

type incidentLevel struct {
	Name  string
	Timer time.Duration
	Text  string
}

// incidentLevels — уровни из настроек, от самого долгого срока к самому
// короткому: от наименее критичного к самому критичному.
func incidentLevels() ([]incidentLevel, error) {
	raw := viper.GetStringMapString("incidents.levels")
	if len(raw) == 0 {
		raw = defaultIncidentLevels
	}
	var out []incidentLevel
	for name, text := range raw {
		d, err := rbac.ParseFor(text)
		if err != nil {
			return nil, fmt.Errorf("incidents.levels.%s: %v", name, err)
		}
		out = append(out, incidentLevel{Name: strings.ToLower(name), Timer: d, Text: text})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Timer != out[j].Timer {
			return out[i].Timer > out[j].Timer
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func findIncidentLevel(name string) (incidentLevel, error) {
	levels, err := incidentLevels()
	if err != nil {
		return incidentLevel{}, err
	}
	var names []string
	for _, l := range levels {
		if strings.EqualFold(l.Name, name) {
			return l, nil
		}
		names = append(names, fmt.Sprintf("%s (%s)", l.Name, l.Text))
	}
	return incidentLevel{}, fmt.Errorf("уровня %q нет; уровни: %s — incidents.levels в config.yaml", name, strings.Join(names, ", "))
}

// incidentEngineers — инженерная группа уровня.
func incidentEngineers(level string) []string {
	if to := viper.GetStringSlice("incidents.notify." + strings.ToLower(level)); len(to) > 0 {
		return to
	}
	if to := viper.GetStringSlice("incidents.engineers"); len(to) > 0 {
		return to
	}
	return viper.GetStringSlice("defaults.smtp_to")
}

func incidentStore() (*workspace.Incidents, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewIncidents(filepath.Join(fm.BaseDir(), incidentsDir))
}

// actor — кто делает шаг: роль и пользователь ОС.
func actor() string {
	if currentRole != nil {
		return currentRole.Name
	}
	return workspace.CurrentUser()
}

func auditIncident(i *workspace.Incident, what string, decision error) {
	journal, err := auditJournal()
	if err != nil {
		return
	}
	role := ""
	if currentRole != nil {
		role = currentRole.Name
	}
	_ = journal.Append(audit.Record{Event: audit.EventIncident, Role: role, Command: i.ID,
		Args: []string{what}, Remote: os.Getenv(EnvRemoteAddr), User: workspace.CurrentUser()},
		func(audit.Recent) error { return decision })
}

// openIncident открывает инцидент.
func openIncident(level string, hosts []string, summary, source string) (*workspace.Incident, error) {
	l, err := findIncidentLevel(level)
	if err != nil {
		return nil, err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil, fmt.Errorf("что случилось — описание инцидента обязательно: его прочтут инженеры")
	}
	if len(summary) > 4096 {
		return nil, fmt.Errorf("описание длиннее 4 КиБ: подробности — заметками и журналами (incident note, incident attach)")
	}
	for _, h := range hosts {
		if err := checkAgentName(h); err != nil {
			return nil, fmt.Errorf("хост %q: %w", h, err)
		}
	}
	store, err := incidentStore()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	i := &workspace.Incident{Level: l.Name, Timer: l.Text, Opened: now, Deadline: now.Add(l.Timer),
		Source: source, Hosts: hosts, Summary: summary}
	i.Step(source, "open", summary, "")
	if err := store.Create(i); err != nil {
		return nil, err
	}
	auditIncident(i, fmt.Sprintf("открыт (%s, модель чинит до %s): %s", l.Name, i.Deadline.Local().Format("15:04"), summary), nil)
	return i, nil
}

// closedForModel — модель по закрытому инциденту больше не действует:
// срок для того и нужен, чтобы она не ушла в цикл.
func closedForModel(i *workspace.Incident) error {
	switch i.State {
	case workspace.IncidentEscalated:
		return i18n.Errorf("инцидент %s эскалирован инженерам (%s): модель по нему больше не действует", i.ID, i.Outcome)
	case workspace.IncidentResolved:
		return i18n.Errorf("инцидент %s уже решён: %s", i.ID, i.Outcome)
	}
	return nil
}

// stepIncident дописывает шаг. Модели — только в открытый инцидент.
func stepIncident(id, kind, text, ref string) (*workspace.Incident, error) {
	store, err := incidentStore()
	if err != nil {
		return nil, err
	}
	settleIncidents(func(string, ...any) {})
	return store.Update(id, func(i *workspace.Incident) error {
		if i.State != workspace.IncidentOpen && (currentRole == nil || currentRole.Name == rbac.LLM) {
			return closedForModel(i)
		}
		i.Step(actor(), kind, text, ref)
		return nil
	})
}

// ---------------------------------------------------------------------------
// Эскалация
// ---------------------------------------------------------------------------

// settleIncidents эскалирует открытые инциденты, срок которых вышел.
// Вызывается командами инцидентов, заявками по инциденту и сервером.
func settleIncidents(out func(string, ...any)) {
	store, err := incidentStore()
	if err != nil {
		return
	}
	all, err := store.List()
	if err != nil {
		return
	}
	now := time.Now()
	for _, i := range all {
		if i.Overdue(now) {
			reason := fmt.Sprintf("время вышло: модель не решила за %s", i.Timer)
			if err := escalateIncident(i.ID, reason, "ochan", out); err != nil {
				out("инцидент %s: эскалация не удалась: %v\n", i.ID, err)
			}
		}
	}
}

// escalateIncident закрывает инцидент эскалацией и шлёт инженерам письмо.
func escalateIncident(id, reason, by string, out func(string, ...any)) error {
	store, err := incidentStore()
	if err != nil {
		return err
	}
	var escalated *workspace.Incident
	_, err = store.Update(id, func(i *workspace.Incident) error {
		if i.State != workspace.IncidentOpen {
			return closedForModel(i)
		}
		i.State, i.Closed, i.Outcome = workspace.IncidentEscalated, time.Now().UTC(), reason
		i.Step(by, "escalate", reason, "")
		escalated = i
		return nil
	})
	if err != nil {
		return err
	}
	to := incidentEngineers(escalated.Level)
	mailErr := sendEscalation(escalated, to)
	_, err = store.Update(id, func(i *workspace.Incident) error {
		if mailErr != nil {
			i.NotifyError = mailErr.Error()
		} else {
			i.Notified = to
		}
		return nil
	})
	auditIncident(escalated, "эскалирован инженерам: "+reason, nil)
	if mailErr != nil {
		out("Инцидент %s эскалирован (%s), но письмо инженерам не ушло: %v\n", id, reason, mailErr)
		return err
	}
	out("Инцидент %s эскалирован инженерам (%s): письмо — %s\n", id, reason, strings.Join(to, ", "))
	return err
}

// sendEscalation — заявка инженерной группе: письмо с описанием, шагами и
// журналами, документ .docx и сами журналы вложениями.
func sendEscalation(i *workspace.Incident, to []string) error {
	if len(to) == 0 {
		return fmt.Errorf("инженерная группа не задана: incidents.engineers в config.yaml")
	}
	body := escalationText(i)
	doc, err := report.DOCX(escalationDoc(i, body))
	if err != nil {
		return err
	}
	attachments := []report.Attachment{{Name: "incident-" + i.ID + ".docx", Data: doc}}
	for _, l := range i.Logs {
		attachments = append(attachments, report.Attachment{Name: l.Name, Data: l.Content})
	}
	cfg := report.MailConfig{
		Host:     viper.GetString("defaults.smtp_host"),
		Port:     viper.GetInt("defaults.smtp_port"),
		From:     viper.GetString("defaults.smtp_from"),
		To:       to,
		Username: viper.GetString("defaults.smtp_user"),
		Password: os.Getenv("OCTOCHAN_SMTP_PASSWORD"),
		StartTLS: viper.GetBool("defaults.smtp_starttls"),
	}
	subject := fmt.Sprintf("[ochan] Инцидент %s (%s) эскалирован: %s", i.ID, i.Level, i.Outcome)
	return mailSender(cfg, report.Mail{Subject: subject, Body: body, Attachments: attachments})
}

// mailSender — отправка; тесты подменяют её.
var mailSender = report.Send

// escalationText — тело письма инженерам.
func escalationText(i *workspace.Incident) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Инцидент %s — эскалация инженерам\n\n", i.ID)
	fmt.Fprintf(&b, "Уровень:  %s (модель чинит сама %s)\n", i.Level, i.Timer)
	fmt.Fprintf(&b, "Открыт:   %s (%s)\n", i.Opened.Local().Format("2006-01-02 15:04"), i.Source)
	if len(i.Hosts) > 0 {
		fmt.Fprintf(&b, "Хосты:    %s\n", strings.Join(i.Hosts, ", "))
	}
	fmt.Fprintf(&b, "Что:      %s\n", i.Summary)
	fmt.Fprintf(&b, "Причина:  %s\n\n", i.Outcome)

	b.WriteString("Что сделано:\n")
	for _, s := range i.Steps {
		ref := ""
		if s.Ref != "" {
			ref = " " + s.Ref
		}
		fmt.Fprintf(&b, "  %s  %-10s %s%s: %s\n", s.Time.Local().Format("15:04:05"), s.Who, s.Kind, ref, s.Text)
	}

	if requests := incidentRequests(i.ID); len(requests) > 0 {
		b.WriteString("\nЗаявки по инциденту:\n")
		for _, r := range requests {
			fmt.Fprintf(&b, "  %s  %s  %s\n", r.ID, r.State.Russian(), r.Summary)
		}
	}

	if len(i.Logs) > 0 {
		b.WriteString("\nЖурналы (важное; целиком — во вложениях):\n")
		for _, l := range i.Logs {
			fmt.Fprintf(&b, "  %s:\n", l.Name)
			for _, line := range importantLines(l.Content, 30) {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
	}

	if records := auditSince(i.Opened); len(records) > 0 {
		b.WriteString("\nЖурнал аудита за время инцидента:\n")
		for _, r := range records {
			fmt.Fprintf(&b, "  %s\n", r)
		}
	}
	return b.String()
}

func escalationDoc(i *workspace.Incident, body string) report.Document {
	doc := report.Document{
		Title:   fmt.Sprintf("Инцидент %s — эскалация", i.ID),
		Author:  "ochan",
		Created: time.Now(),
		Fields: []report.Field{
			{Name: "Уровень", Value: fmt.Sprintf("%s (срок модели %s)", i.Level, i.Timer)},
			{Name: "Открыт", Value: i.Opened.Local().Format("2006-01-02 15:04:05") + " — " + i.Source},
			{Name: "Хосты", Value: strings.Join(i.Hosts, ", ")},
			{Name: "Что", Value: i.Summary},
			{Name: "Причина эскалации", Value: i.Outcome},
		},
		Listing: body,
	}
	for _, s := range i.Steps {
		doc.Comments = append(doc.Comments, fmt.Sprintf("%s %s (%s): %s", s.Time.Local().Format("15:04:05"), s.Who, s.Kind, s.Text))
	}
	return doc
}

// importantLines — строки журнала важнее logs.min_level, не больше limit.
func importantLines(content []byte, limit int) []string {
	r, err := ranker()
	if err != nil {
		return nil
	}
	min, _ := r.Level(r.Min())
	var out []string
	_ = r.Scan(bytes.NewReader(content), func(e logs.Entry) error {
		if e.Rank >= min && len(out) < limit {
			out = append(out, fmt.Sprintf("%s  %s", e.Level, e.Text))
		}
		return nil
	})
	if len(out) == 0 {
		lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
		if len(lines) > limit {
			lines = lines[len(lines)-limit:]
		}
		out = append(out, lines...)
	}
	return out
}

// auditSince — записи журнала аудита с начала инцидента.
func auditSince(from time.Time) []string {
	journal, err := auditJournal()
	if err != nil {
		return nil
	}
	records, err := journal.Last(200, func(r audit.Record) bool { return !r.Time.Before(from) })
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(records))
	for _, r := range records {
		mark := "+"
		if !r.Allowed {
			mark = "-"
		}
		what := strings.TrimSpace(strings.ReplaceAll(r.Command, ".", " ") + " " + strings.Join(r.Args, " "))
		if r.Event != audit.EventCommand {
			what = r.Event + ": " + what
		}
		out = append(out, fmt.Sprintf("%s %s %-10s %s", mark, r.Time.Local().Format("15:04:05"), r.Role, what))
	}
	return out
}

// incidentRequests — заявки, оформленные по инциденту.
func incidentRequests(id string) []*workspace.Request {
	store, err := requestStore()
	if err != nil {
		return nil
	}
	all, err := store.List()
	if err != nil {
		return nil
	}
	var out []*workspace.Request
	for _, r := range all {
		if r.Incident == id {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Заявки по инциденту
// ---------------------------------------------------------------------------

// linkIncident проверяет инцидент заявки: модель по закрытому не
// оформляет. Возвращает полный номер.
func linkIncident(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	settleIncidents(func(string, ...any) {})
	store, err := incidentStore()
	if err != nil {
		return "", err
	}
	i, err := store.Get(id)
	if err != nil {
		return "", err
	}
	if currentRole != nil && currentRole.Name == rbac.LLM {
		if err := closedForModel(i); err != nil {
			return "", err
		}
	}
	return i.ID, nil
}

// incidentOpenForFree — free выполняет заявку по инциденту, только пока
// инцидент открыт и срок не вышел.
func incidentOpenForFree(request *workspace.Request) error {
	if request.Incident == "" {
		return nil
	}
	settleIncidents(func(string, ...any) {})
	store, err := incidentStore()
	if err != nil {
		return err
	}
	i, err := store.Get(request.Incident)
	if err != nil {
		return err
	}
	return closedForModel(i)
}

// noteRequestStep — шаг инцидента о заявке.
func noteRequestStep(request *workspace.Request, kind, text string) {
	if request.Incident == "" {
		return
	}
	store, err := incidentStore()
	if err != nil {
		return
	}
	_, _ = store.Update(request.Incident, func(i *workspace.Incident) error {
		i.Step(actor(), kind, text, request.ID)
		return nil
	})
}

// ---------------------------------------------------------------------------
// Команды
// ---------------------------------------------------------------------------

var (
	incidentLevelFlag  string
	incidentHosts      []string
	incidentSource     string
	incidentAttachName string
)

// textArg — текст из аргумента или stdin («-»).
func textArg(arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	data, err := io.ReadAll(io.LimitReader(roleInput, 64<<10))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

var incidentCmd = &cobra.Command{
	Use:   "incident [номер]",
	Short: "Инциденты HA-AI: модель чинит до срока, не успела — эскалация инженерам",
	Long: "Инцидент открывает мониторинг (вебхук POST /v1/incidents или incident open)\n" +
		"или сама модель. У уровня критичности — срок (incidents.levels): столько\n" +
		"модель чинит сама. Её заметки, заявки (request --incident) и их выполнение —\n" +
		"шаги инцидента. Решила — incident resolve. Не успела или признала, что не\n" +
		"справится (incident escalate), — инцидент эскалируется: инженерной группе\n" +
		"(incidents.engineers) уходит письмо с описанием, шагами, журналами и .docx,\n" +
		"а модель по инциденту больше не действует. Срок проверяет сервер, любая\n" +
		"команда инцидентов и incident check — для cron.\n\n" +
		"Без аргументов — инциденты; с номером — карточка.",
	Args: cobra.MaximumNArgs(1),
	Example: "ochan incident open --level critical --host db7 \"PostgreSQL не принимает соединения\"\n" +
		"ochan incident note 3f2a \"max_connections исчерпан: 200 из 200\"\n" +
		"ochan request raise.yaml --incident 3f2a\n" +
		"ochan incident resolve 3f2a \"поднят max_connections до 300, соединения идут\"\n" +
		"ochan incident escalate 3f2a \"причина не в конфиге: нужен человек\"",
	RunE: func(cmd *cobra.Command, args []string) error {
		settleIncidents(cmd.Printf)
		store, err := incidentStore()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			i, err := store.Get(args[0])
			if err != nil {
				return err
			}
			showIncident(cmd, i)
			return nil
		}
		all, err := store.List()
		if err != nil {
			return err
		}
		if len(all) == 0 {
			cmd.Println(i18n.T("Инцидентов нет"))
			return nil
		}
		now := time.Now()
		for _, i := range all {
			state := i.State.Russian()
			if i.State == workspace.IncidentOpen {
				state = i18n.Sprintf("открыт, модель чинит ещё %s", i.Deadline.Sub(now).Round(time.Second))
			}
			cmd.Printf("%s  %s  %-9s %-34s %s\n", i.Opened.Local().Format("2006-01-02 15:04"), i.ID, i.Level, state, i.Summary)
		}
		return nil
	},
}

func showIncident(cmd *cobra.Command, i *workspace.Incident) {
	now := time.Now()
	cmd.Printf("Инцидент:  %s\n", i.ID)
	cmd.Printf("Состояние: %s\n", i.State.Russian())
	cmd.Printf("Уровень:   %s (модель чинит сама %s)\n", i.Level, i.Timer)
	cmd.Printf("Открыт:    %s (%s)\n", i.Opened.Local().Format("2006-01-02 15:04:05"), i.Source)
	if i.State == workspace.IncidentOpen {
		cmd.Printf("Срок:      %s (осталось %s), затем — эскалация: %s\n", i.Deadline.Local().Format("15:04:05"),
			i.Deadline.Sub(now).Round(time.Second), strings.Join(incidentEngineers(i.Level), ", "))
	} else {
		cmd.Printf("Итог:      %s (%s)\n", i.Outcome, i.Closed.Local().Format("2006-01-02 15:04:05"))
	}
	if len(i.Notified) > 0 {
		cmd.Printf("Письмо:    %s\n", strings.Join(i.Notified, ", "))
	}
	if i.NotifyError != "" {
		cmd.Printf("Письмо:    не ушло — %s\n", i.NotifyError)
	}
	if len(i.Hosts) > 0 {
		cmd.Printf("Хосты:     %s\n", strings.Join(i.Hosts, ", "))
	}
	cmd.Printf("Что:       %s\n", i.Summary)
	cmd.Println("\nШаги:")
	for _, s := range i.Steps {
		ref := ""
		if s.Ref != "" {
			ref = " " + s.Ref
		}
		cmd.Printf("  %s  %-10s %s%s: %s\n", s.Time.Local().Format("15:04:05"), s.Who, s.Kind, ref, s.Text)
	}
	if len(i.Logs) > 0 {
		cmd.Println("\nЖурналы:")
		for _, l := range i.Logs {
			cmd.Printf("  %s, байт %d\n", l.Name, len(l.Content))
		}
	}
}

var incidentOpenCmd = &cobra.Command{
	Use:   "open --level <уровень> [--host h] <что случилось | ->",
	Short: "Открыть инцидент: модель идёт чинить, срок — по уровню",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		summary, err := textArg(args[0])
		if err != nil {
			return err
		}
		source := incidentSource
		if source == "" {
			source = actor()
		}
		i, err := openIncident(incidentLevelFlag, incidentHosts, summary, source)
		if err != nil {
			return err
		}
		cmd.Printf("Инцидент %s (%s) открыт: модель чинит сама до %s (%s)\n", i.ID, i.Level,
			i.Deadline.Local().Format("15:04"), i.Timer)
		cmd.Printf("Не успеет — эскалация инженерам: %s\n", orDash(strings.Join(incidentEngineers(i.Level), ", ")))
		return nil
	},
}

var incidentNoteCmd = &cobra.Command{
	Use:   "note <номер> <что сделано или найдено | ->",
	Short: "Заметка к инциденту: что модель нашла и сделала",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		text, err := textArg(args[1])
		if err != nil {
			return err
		}
		if text == "" {
			return fmt.Errorf("пустая заметка")
		}
		i, err := stepIncident(args[0], "note", text, "")
		if err != nil {
			return err
		}
		cmd.Printf("Инцидент %s: заметка записана, шагов %d\n", i.ID, len(i.Steps))
		return nil
	},
}

var incidentAttachCmd = &cobra.Command{
	Use:   "attach <номер> <журнал | ->",
	Short: "Приложить журнал к инциденту: он уйдёт инженерам при эскалации",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		in, name, err := openLog(args[1])
		if err != nil {
			return err
		}
		defer in.Close()
		data, err := io.ReadAll(io.LimitReader(in, workspace.MaxIncidentLog+1))
		if err != nil {
			return err
		}
		if incidentAttachName != "" {
			name = incidentAttachName
		}
		name = filepath.Base(name)
		store, err := incidentStore()
		if err != nil {
			return err
		}
		settleIncidents(cmd.Printf)
		i, err := store.Update(args[0], func(i *workspace.Incident) error {
			if i.State != workspace.IncidentOpen && (currentRole == nil || currentRole.Name == rbac.LLM) {
				return closedForModel(i)
			}
			if err := i.Attach(name, data); err != nil {
				return err
			}
			i.Step(actor(), "attach", fmt.Sprintf("журнал %s, байт %d", name, len(data)), name)
			return nil
		})
		if err != nil {
			return err
		}
		cmd.Printf("Инцидент %s: приложен %s, байт %d\n", i.ID, name, len(data))
		return nil
	},
}

var incidentResolveCmd = &cobra.Command{
	Use:   "resolve <номер> <что сделано | ->",
	Short: "Инцидент решён",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		text, err := textArg(args[1])
		if err != nil {
			return err
		}
		if text == "" {
			return fmt.Errorf("что сделано — обязательно: решение без описания не проверить")
		}
		store, err := incidentStore()
		if err != nil {
			return err
		}
		settleIncidents(cmd.Printf)
		i, err := store.Update(args[0], func(i *workspace.Incident) error {
			switch {
			case i.State == workspace.IncidentResolved:
				return closedForModel(i)
			case i.State == workspace.IncidentEscalated && (currentRole == nil || currentRole.Name == rbac.LLM):
				return closedForModel(i)
			}
			i.State, i.Closed, i.Outcome = workspace.IncidentResolved, time.Now().UTC(), text
			i.Step(actor(), "resolve", text, "")
			return nil
		})
		if err != nil {
			return err
		}
		auditIncident(i, "решён: "+text, nil)
		cmd.Printf("Инцидент %s решён за %s: %s\n", i.ID, i.Closed.Sub(i.Opened).Round(time.Second), text)
		return nil
	},
}

var incidentEscalateCmd = &cobra.Command{
	Use:   "escalate <номер> <почему | ->",
	Short: "Эскалировать инцидент инженерам сейчас: модель не справится",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		text, err := textArg(args[1])
		if err != nil {
			return err
		}
		if text == "" {
			return fmt.Errorf("почему эскалация — обязательно: это первое, что прочтут инженеры")
		}
		store, err := incidentStore()
		if err != nil {
			return err
		}
		i, err := store.Get(args[0])
		if err != nil {
			return err
		}
		return escalateIncident(i.ID, actor()+": "+text, actor(), cmd.Printf)
	},
}

var incidentCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Эскалировать инциденты, срок которых вышел — для cron",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		settleIncidents(cmd.Printf)
		return nil
	},
}

var incidentSecretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Создать секрет вебхука мониторинга",
	Long: "Секрет — 256 случайных бит в ~/.octochan/incidents.secret (права 0600).\n" +
		"Мониторинг шлёт его заголовком Authorization: Bearer <секрет> на\n" +
		"POST https://<сервер>/v1/incidents с телом {\"level\": \"critical\",\n" +
		"\"hosts\": [\"db7\"], \"summary\": \"…\", \"source\": \"zabbix\", \"log\": \"…\"}.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		path := filepath.Join(fm.BaseDir(), incidentsSecret)
		if err := os.WriteFile(path, []byte(hex.EncodeToString(buf)+"\n"), 0o600); err != nil {
			return err
		}
		cmd.Printf("Секрет вебхука мониторинга: %s (права 0600)\n", path)
		return nil
	},
}

// incidentRequest — тело вебхука мониторинга.
type incidentRequest struct {
	Level   string   `json:"level"`
	Hosts   []string `json:"hosts"`
	Summary string   `json:"summary"`
	Source  string   `json:"source"`
	// Log — кусок журнала, который мониторинг видел.
	Log string `json:"log,omitempty"`
}

// incidentsHandler — POST /v1/incidents: мониторинг открывает инцидент.
func incidentsHandler(writable func() (bool, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reply := func(code int, payload map[string]string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(payload)
		}
		fm, err := mustFileManager()
		if err != nil {
			reply(http.StatusInternalServerError, map[string]string{"result": err.Error()})
			return
		}
		secret, err := os.ReadFile(filepath.Join(fm.BaseDir(), incidentsSecret))
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(secret))), []byte(token)) != 1 {
			reply(http.StatusUnauthorized, map[string]string{"result": "нет секрета вебхука или он не тот"})
			return
		}
		if ok, why := writable(); !ok {
			reply(http.StatusServiceUnavailable, map[string]string{"result": why})
			return
		}
		var req incidentRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 512<<10)).Decode(&req); err != nil {
			reply(http.StatusBadRequest, map[string]string{"result": "тело — JSON {\"level\", \"hosts\", \"summary\", \"source\", \"log\"}"})
			return
		}
		source := "мониторинг " + remote.ClientIP(r)
		if req.Source != "" {
			source = "мониторинг " + req.Source
		}
		i, err := openIncident(req.Level, req.Hosts, req.Summary, source)
		if err == nil && req.Log != "" {
			store, serr := incidentStore()
			if serr == nil {
				_, err = store.Update(i.ID, func(i *workspace.Incident) error {
					if err := i.Attach("monitoring.log", []byte(req.Log)); err != nil {
						return err
					}
					i.Step(source, "attach", "журнал мониторинга", "monitoring.log")
					return nil
				})
			}
		}
		if err != nil {
			reply(http.StatusBadRequest, map[string]string{"result": err.Error()})
			return
		}
		reply(http.StatusOK, map[string]string{"id": i.ID, "deadline": i.Deadline.Format(time.RFC3339),
			"result": fmt.Sprintf("инцидент %s (%s) открыт: модель чинит до %s", i.ID, i.Level, i.Deadline.Local().Format("15:04"))})
	}
}

// watchIncidents — сервер проверяет сроки сам, пока он primary.
func watchIncidents(ctx context.Context, writable func() bool, logf func(string, ...any)) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if writable() {
				settleIncidents(logf)
			}
		}
	}
}

func init() {
	incidentOpenCmd.Flags().StringVar(&incidentLevelFlag, "level", "", "уровень критичности: incidents.levels")
	_ = incidentOpenCmd.MarkFlagRequired("level")
	incidentOpenCmd.Flags().StringSliceVar(&incidentHosts, "host", nil, "хосты инцидента")
	incidentOpenCmd.Flags().StringVar(&incidentSource, "source", "", "кто открыл: мониторинг, модель")
	incidentAttachCmd.Flags().StringVar(&incidentAttachName, "name", "", "имя журнала во вложении (для stdin)")
	incidentCmd.AddCommand(incidentOpenCmd, incidentNoteCmd, incidentAttachCmd, incidentResolveCmd,
		incidentEscalateCmd, incidentCheckCmd, incidentSecretCmd)
	rootCmd.AddCommand(incidentCmd)
}
