package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Статистика инцидентов: что модель решила сама, что ушло инженерам и
// что ещё не решено. Считается по инцидентам, открытым за период, —
// заново из их файлов, а не накопленными счётчиками: поправленный или
// дорешённый инцидент сразу виден в статистике.

// IncidentCounts — счётчики одного среза: всего периода или уровня.
type IncidentCounts struct {
	Total int
	// Решены без эскалации: моделью до срока и человеком.
	ByModel, ByHuman int
	// Эскалированы: срок вышел, модель отдала сама, отдал человек.
	Timeout, GaveUp, Handed int
	// Из эскалированных: решены потом людьми и всё ещё у инженеров.
	Fixed, Pending int
	// Открыты: модель ещё чинит.
	Open int
	// MailErrors — инцидентов, письмо по которым не ушло.
	MailErrors int
	// ModelTimes — за сколько решила модель; EngineerTimes — за сколько
	// решили люди после эскалации.
	ModelTimes, EngineerTimes []time.Duration
}

// Escalated — модель не решила: инцидент ушёл инженерам.
func (c *IncidentCounts) Escalated() int { return c.Timeout + c.GaveUp + c.Handed }

// Resolved — решены, кем бы то ни было.
func (c *IncidentCounts) Resolved() int { return c.ByModel + c.ByHuman + c.Fixed }

// Unresolved — не решены сейчас: у инженеров или ещё у модели.
func (c *IncidentCounts) Unresolved() int { return c.Pending + c.Open }

func (c *IncidentCounts) add(i *Incident, model string) {
	c.Total++
	switch i.Result(model) {
	case OutcomeByModel:
		c.ByModel++
		c.ModelTimes = append(c.ModelTimes, i.Resolved.Sub(i.Opened))
	case OutcomeByHuman:
		c.ByHuman++
	case OutcomeTimeout:
		c.Timeout++
	case OutcomeModelGave:
		c.GaveUp++
	case OutcomeEscalation:
		c.Handed++
	default:
		c.Open++
	}
	if !i.Escalated.IsZero() {
		if i.State == IncidentResolved {
			c.Fixed++
			c.EngineerTimes = append(c.EngineerTimes, i.Resolved.Sub(i.Escalated))
		} else {
			c.Pending++
		}
	}
	for _, m := range i.Mails {
		if m.Error != "" {
			c.MailErrors++
			break
		}
	}
}

// IncidentStats — статистика инцидентов, открытых в [From, To).
type IncidentStats struct {
	IncidentCounts
	From, To time.Time
	// Levels — по уровням критичности.
	Levels map[string]*IncidentCounts
	// Incidents — инциденты периода, от старых к новым.
	Incidents []*Incident
}

// SummarizeIncidents считает статистику инцидентов, открытых в [from, to).
// model — роль модели: решённое ею отличается от решённого человеком.
func SummarizeIncidents(all []*Incident, from, to time.Time, model string) *IncidentStats {
	s := &IncidentStats{From: from, To: to, Levels: map[string]*IncidentCounts{}}
	for _, i := range all {
		if i.Opened.Before(from) || !i.Opened.Before(to) {
			continue
		}
		s.Incidents = append(s.Incidents, i)
		s.add(i, model)
		level := s.Levels[i.Level]
		if level == nil {
			level = &IncidentCounts{}
			s.Levels[i.Level] = level
		}
		level.add(i, model)
	}
	sort.Slice(s.Incidents, func(a, b int) bool { return s.Incidents[a].Opened.Before(s.Incidents[b].Opened) })
	return s
}

// MeanMedian — среднее и медиана; для пустого — нули.
func MeanMedian(ds []time.Duration) (mean, median time.Duration) {
	if len(ds) == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	mean = sum / time.Duration(len(sorted))
	if n := len(sorted); n%2 == 1 {
		median = sorted[n/2]
	} else {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return mean, median
}

// ---------------------------------------------------------------------------
// Расписание отчёта
// ---------------------------------------------------------------------------

// IncidentReport — расписание отчёта по инцидентам: окно следующего
// отчёта начинается с Since. Лежит рядом с инцидентами — и уходит на
// standby вместе с ними: после promote отчёт не повторится и не пропадёт.
type IncidentReport struct {
	Since time.Time `json:"since"`
	// Sent и To — когда и кому ушёл последний отчёт.
	Sent time.Time `json:"sent,omitempty"`
	To   []string  `json:"to,omitempty"`
	// Error и Tried — последняя неудача: отчёт пробуют снова не чаще
	// ReportRetry, а окно не сдвигается, пока он не уйдёт.
	Error string    `json:"error,omitempty"`
	Tried time.Time `json:"tried,omitempty"`
}

// ReportRetry — как часто повторять отчёт, который не ушёл.
const ReportRetry = 10 * time.Minute

// Расширение не .json: List не примет расписание за инцидент.
const incidentReportFile = "report.state"

func (s *Incidents) reportPath() string { return filepath.Join(s.dir, incidentReportFile) }

// Report — расписание отчёта; nil — отчёт ещё не начинался.
func (s *Incidents) Report() (*IncidentReport, error) {
	data, err := os.ReadFile(s.reportPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r IncidentReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Next — когда уйдёт следующий отчёт.
func (r *IncidentReport) Next(every time.Duration) time.Time {
	next := r.Since.Add(every)
	if r.Error != "" && next.Before(r.Tried.Add(ReportRetry)) {
		next = r.Tried.Add(ReportRetry)
	}
	return next
}

// ReportOutcome — что сделал DueReport.
type ReportOutcome int

// Исходы DueReport.
const (
	ReportNotDue  ReportOutcome = iota // срок не подошёл
	ReportStarted                      // расписание началось: первый отчёт — через every
	ReportSent                         // отчёт ушёл
	ReportFailed                       // отчёт не ушёл: повтор через ReportRetry
)

// DueReport отправляет отчёт, если подошёл срок. Окна отчётов идут
// подряд, без пропусков и наложений: [Since, Since+every), затем
// следующее. Сервер стоял несколько окон — один отчёт за все пропущенные.
// Первый вызов только начинает расписание: первый отчёт — через every.
// Под замком: сервер и incident check в cron не отправят отчёт оба.
func (s *Incidents) DueReport(every time.Duration, now time.Time,
	send func(from, to time.Time) ([]string, error)) (*IncidentReport, ReportOutcome, error) {
	if every <= 0 {
		return nil, ReportNotDue, nil
	}
	unlock, err := lockPath(s.reportPath() + ".lock")
	if err != nil {
		return nil, ReportNotDue, err
	}
	defer unlock()
	r, err := s.Report()
	if err != nil {
		return nil, ReportNotDue, err
	}
	if r == nil {
		r = &IncidentReport{Since: now.UTC()}
		return r, ReportStarted, writeJSONAtomic(s.reportPath(), r)
	}
	if now.Before(r.Next(every)) {
		return r, ReportNotDue, nil
	}
	periods := now.Sub(r.Since) / every
	to := r.Since.Add(periods * every)
	recipients, sendErr := send(r.Since, to)
	outcome := ReportSent
	if sendErr != nil {
		outcome = ReportFailed
		r.Error, r.Tried = sendErr.Error(), now.UTC()
	} else {
		r.Since, r.Sent, r.To, r.Error, r.Tried = to, now.UTC(), recipients, "", time.Time{}
	}
	if err := writeJSONAtomic(s.reportPath(), r); err != nil {
		return r, outcome, err
	}
	return r, outcome, sendErr
}
