package cmd

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
	"strings"
	"time"

	"octochan/i18n"
	"octochan/rbac"
	"octochan/report"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Статистика инцидентов и отчёт на почту.
//
//	incidents:
//	  report:
//	    every: 7d                         # как часто и за какой срок; пусто — не слать
//	    to: [ops-leads@corp.example]      # пусто — incidents.engineers
//
// Отчёт уходит сам: сервер на primary и incident check в cron шлют его,
// когда подошёл срок. Окна отчётов идут подряд: каждый — за время с
// предыдущего.

const defaultStatsPeriod = 7 * 24 * time.Hour

// incidentReportEvery — период отчёта; 0 — отчёт по расписанию не шлётся.
func incidentReportEvery() (time.Duration, error) {
	d, err := parseDays(viper.GetString("incidents.report.every"))
	if err != nil {
		return 0, fmt.Errorf("incidents.report.every: %v", err)
	}
	return d, nil
}

// incidentReportTo — кому отчёт: своя группа, иначе инженерная.
func incidentReportTo() []string {
	if to := viper.GetStringSlice("incidents.report.to"); len(to) > 0 {
		return to
	}
	if to := viper.GetStringSlice("incidents.engineers"); len(to) > 0 {
		return to
	}
	return viper.GetStringSlice("defaults.smtp_to")
}

// statsPeriod — период из --for: 24h, 7d, all; пусто — период отчёта или 7d.
func statsPeriod(text string, now time.Time) (from time.Time, label string, err error) {
	if text == "all" {
		return time.Time{}, "всё время", nil
	}
	if text == "" {
		d, err := incidentReportEvery()
		if err != nil {
			return time.Time{}, "", err
		}
		if d == 0 {
			d = defaultStatsPeriod
		}
		return now.Add(-d), spanText(d), nil
	}
	d, err := parseDays(text)
	if err != nil || d <= 0 {
		return time.Time{}, "", fmt.Errorf("--for %q: срок вида 24h, 7d или all", text)
	}
	return now.Add(-d), spanText(d), nil
}

// spanText — срок как его пишут в настройках: 7d, 36h, 90m.
func spanText(d time.Duration) string {
	day := 24 * time.Hour
	if d >= day && d%day == 0 {
		return fmt.Sprintf("%dd", d/day)
	}
	text := d.Round(time.Second).String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

func loadIncidentStats(from, to time.Time) (*workspace.IncidentStats, error) {
	store, err := incidentStore()
	if err != nil {
		return nil, err
	}
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	return workspace.SummarizeIncidents(all, from, to, rbac.LLM), nil
}

func percent(n, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", (n*100+total/2)/total)
}

func timesText(ds []time.Duration, after string) string {
	if len(ds) == 0 {
		return ""
	}
	mean, median := workspace.MeanMedian(ds)
	return fmt.Sprintf("   в среднем %s, медиана %s%s", spanText(mean), spanText(median), after)
}

// statsText — статистика текстом: и для экрана, и для письма.
func statsText(s *workspace.IncidentStats, label string) string {
	var b strings.Builder
	period := fmt.Sprintf("по %s", s.To.Local().Format("2006-01-02 15:04"))
	if !s.From.IsZero() {
		period = fmt.Sprintf("с %s %s", s.From.Local().Format("2006-01-02 15:04"), period)
	}
	if s.Total == 0 {
		fmt.Fprintf(&b, "Инцидентов %s (%s): нет\n", period, label)
		return b.String()
	}
	fmt.Fprintf(&b, "Инциденты %s (%s): %d\n\n", period, label, s.Total)
	fmt.Fprintf(&b, "Решены:                       %d\n", s.Resolved())
	fmt.Fprintf(&b, "  моделью до срока            %d%s\n", s.ByModel, timesText(s.ModelTimes, ""))
	fmt.Fprintf(&b, "  человеком до эскалации      %d\n", s.ByHuman)
	fmt.Fprintf(&b, "  людьми после эскалации      %d%s\n", s.Fixed, timesText(s.EngineerTimes, " после эскалации"))
	fmt.Fprintf(&b, "Не решены:                    %d\n", s.Unresolved())
	fmt.Fprintf(&b, "  у инженеров                 %d\n", s.Pending)
	fmt.Fprintf(&b, "  у модели, срок не вышел     %d\n", s.Open)
	fmt.Fprintf(&b, "\nМодель решила сама %d из %d (%s), не справилась — %d (%s):\n",
		s.ByModel, s.Total, percent(s.ByModel, s.Total), s.Escalated(), percent(s.Escalated(), s.Total))
	fmt.Fprintf(&b, "  срок вышел                  %d\n", s.Timeout)
	fmt.Fprintf(&b, "  отдала инженерам сама       %d\n", s.GaveUp)
	fmt.Fprintf(&b, "  отдал инженерам человек     %d\n", s.Handed)
	if s.MailErrors > 0 {
		fmt.Fprintf(&b, "Писем не ушло: по %d инцидентам — ochan incident <номер>\n", s.MailErrors)
	}

	b.WriteString("\nПо уровням:\n")
	fmt.Fprintf(&b, "  %-10s %5s %6s %7s %10s %10s\n", "уровень", "срок", "всего", "модель", "эскалация", "не решены")
	for _, name := range statsLevels(s) {
		c := s.Levels[name]
		fmt.Fprintf(&b, "  %-10s %5s %6d %7d %10d %10d\n", name, levelTimer(name), c.Total, c.ByModel, c.Escalated(), c.Unresolved())
	}

	var open []*workspace.Incident
	for _, i := range s.Incidents {
		if i.State != workspace.IncidentResolved {
			open = append(open, i)
		}
	}
	if len(open) > 0 {
		b.WriteString("\nНерешённые:\n")
		const shown = 20
		for n, i := range open {
			if n == shown {
				fmt.Fprintf(&b, "  … и ещё %d — ochan incident\n", len(open)-shown)
				break
			}
			state := "у модели"
			if i.State == workspace.IncidentEscalated {
				state = "у инженеров"
			}
			fmt.Fprintf(&b, "  %s  %-9s %-12s %s\n", i.ID, i.Level, state, i.Summary)
		}
	}
	return b.String()
}

// statsLevels — уровни периода в порядке настроек: от наименее
// критичного; уровни, которых в настройках уже нет, — в конце.
func statsLevels(s *workspace.IncidentStats) []string {
	var out []string
	seen := map[string]bool{}
	if levels, err := incidentLevels(); err == nil {
		for _, l := range levels {
			if s.Levels[l.Name] != nil {
				out = append(out, l.Name)
				seen[l.Name] = true
			}
		}
	}
	var rest []string
	for name := range s.Levels {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func levelTimer(name string) string {
	if l, err := findIncidentLevel(name); err == nil {
		return l.Text
	}
	return "—"
}

// statsCSV — инциденты периода таблицей: во вложение отчёта.
func statsCSV(s *workspace.IncidentStats) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04:05")
	}
	span := func(from, to time.Time) string {
		if from.IsZero() || to.IsZero() {
			return ""
		}
		return to.Sub(from).Round(time.Second).String()
	}
	_ = w.Write([]string{"id", "level", "timer", "opened", "source", "hosts", "summary", "result", "state",
		"escalated", "escalated_by", "reason", "resolved", "resolved_by", "outcome", "took", "engineers_took"})
	for _, i := range s.Incidents {
		took, engineers := "", ""
		if i.State == workspace.IncidentResolved {
			took = span(i.Opened, i.Resolved)
			engineers = span(i.Escalated, i.Resolved)
		}
		_ = w.Write([]string{i.ID, i.Level, i.Timer, stamp(i.Opened), i.Source, strings.Join(i.Hosts, " "), i.Summary,
			string(i.Result(rbac.LLM)), string(i.State), stamp(i.Escalated), i.EscalatedBy, i.Reason,
			stamp(i.Resolved), i.ResolvedBy, i.Outcome, took, engineers})
	}
	w.Flush()
	return buf.Bytes()
}

// sendIncidentReport — отчёт за [from, to) группе отчёта.
func sendIncidentReport(from, to time.Time, label string) ([]string, error) {
	recipients := incidentReportTo()
	if len(recipients) == 0 {
		return nil, fmt.Errorf("кому слать отчёт, не задано: incidents.report.to или incidents.engineers в config.yaml")
	}
	s, err := loadIncidentStats(from, to)
	if err != nil {
		return nil, err
	}
	day := func(t time.Time) string { return t.Local().Format("2006-01-02") }
	period := day(to)
	if !from.IsZero() {
		period = day(from) + " — " + period
	}
	subject := fmt.Sprintf("[ochan] Инциденты за %s (%s): %d, решила модель %d, у инженеров %d, открыто %d",
		label, period, s.Total, s.ByModel, s.Pending, s.Open)
	body := statsText(s, label)
	lang, err := mailLanguage()
	if err != nil {
		return nil, err
	}
	if lang == langFormal {
		subject = fmt.Sprintf("[ochan] goal: count — incidents %s %s: total %d, model %d, engineers %d, open %d",
			label, strings.ReplaceAll(period, " — ", ".."), s.Total, s.ByModel, s.Pending, s.Open)
		if s.From.IsZero() {
			subject = strings.Replace(subject, "incidents всё время", "incidents all", 1)
		}
		body = formalStats(s)
	}
	name := "incidents-" + strings.ReplaceAll(period, " — ", "_") + ".csv"
	mail := report.Mail{Subject: subject, Body: body,
		Attachments: []report.Attachment{{Name: name, Data: statsCSV(s)}}}
	return recipients, mailSender(mailConfig(recipients), mail)
}

// dueIncidentReport шлёт отчёт по расписанию, если подошёл срок.
func dueIncidentReport(out func(string, ...any)) error {
	every, err := incidentReportEvery()
	if err != nil || every == 0 || onStandby() {
		return err
	}
	store, err := incidentStore()
	if err != nil {
		return err
	}
	now := time.Now()
	r, outcome, err := store.DueReport(every, now, func(from, to time.Time) ([]string, error) {
		return sendIncidentReport(from, to, spanText(to.Sub(from)))
	})
	switch {
	case outcome == workspace.ReportFailed:
		return fmt.Errorf("отчёт по инцидентам не ушёл: %v; повтор после %s", err, r.Next(every).Local().Format("15:04"))
	case err != nil:
		return err
	case outcome == workspace.ReportSent:
		out("Отчёт по инцидентам ушёл: %s; следующий — %s\n", strings.Join(r.To, ", "), r.Next(every).Local().Format("2006-01-02 15:04"))
	case outcome == workspace.ReportStarted:
		out("Отчёт по инцидентам: каждые %s, первый — %s\n", spanText(every), r.Next(every).Local().Format("2006-01-02 15:04"))
	}
	return nil
}

// reportSchedule — строка о расписании отчёта под статистикой.
func reportSchedule() string {
	every, err := incidentReportEvery()
	if err != nil {
		return err.Error()
	}
	if every == 0 {
		return i18n.T("Отчёт на почту по расписанию выключен: incidents.report.every в config.yaml")
	}
	to := strings.Join(incidentReportTo(), ", ")
	if to == "" {
		to = i18n.T("никому: incidents.report.to не задан")
	}
	line := i18n.Sprintf("Отчёт: каждые %s → %s", spanText(every), to)
	store, err := incidentStore()
	if err != nil {
		return line
	}
	r, err := store.Report()
	switch {
	case err != nil:
		return line + "; " + err.Error()
	case r == nil:
		return line + i18n.T("; начнётся при первой проверке: ochan serve или incident check")
	case r.Error != "":
		return line + i18n.Sprintf("; последний не ушёл: %s; повтор после %s", r.Error, r.Next(every).Local().Format("2006-01-02 15:04"))
	}
	if !r.Sent.IsZero() {
		line += i18n.Sprintf("; последний ушёл %s", r.Sent.Local().Format("2006-01-02 15:04"))
	}
	return line + i18n.Sprintf("; следующий — %s", r.Next(every).Local().Format("2006-01-02 15:04"))
}

var incidentStatsFor string

var incidentStatsCmd = &cobra.Command{
	Use:   "stats [--for 7d]",
	Short: "Статистика инцидентов: что решила модель, что ушло инженерам, что не решено",
	Long: "Считает инциденты, открытые за период: решены моделью до срока, человеком до\n" +
		"эскалации, людьми после эскалации; не решены — у инженеров или у модели;\n" +
		"почему модель не справилась — срок вышел, отдала сама, отдал человек.\n" +
		"Время решения — среднее и медиана. Период — --for (24h, 7d, all), без него —\n" +
		"incidents.report.every или 7d.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		settleIncidents(cmd.Printf)
		now := time.Now()
		from, label, err := statsPeriod(incidentStatsFor, now)
		if err != nil {
			return err
		}
		s, err := loadIncidentStats(from, now)
		if err != nil {
			return err
		}
		cmd.Print(statsText(s, label))
		cmd.Printf("\n%s\n", reportSchedule())
		return nil
	},
}

var incidentReportCmd = &cobra.Command{
	Use:   "report [--for 7d]",
	Short: "Отправить статистику инцидентов на почту сейчас",
	Long: "Шлёт статистику за период группе отчёта (incidents.report.to, иначе\n" +
		"incidents.engineers) с таблицей инцидентов во вложении .csv. Расписание\n" +
		"отчётов (incidents.report.every) не сдвигает.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		settleIncidents(cmd.Printf)
		now := time.Now()
		from, label, err := statsPeriod(incidentStatsFor, now)
		if err != nil {
			return err
		}
		to, err := sendIncidentReport(from, now, label)
		if err != nil {
			return fmt.Errorf("отчёт по инцидентам не ушёл: %w", err)
		}
		cmd.Printf("Отчёт по инцидентам за %s ушёл: %s\n", label, strings.Join(to, ", "))
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{incidentStatsCmd, incidentReportCmd} {
		c.Flags().StringVar(&incidentStatsFor, "for", "", "период: 24h, 7d, all; без него — incidents.report.every или 7d")
		incidentCmd.AddCommand(c)
	}
}
