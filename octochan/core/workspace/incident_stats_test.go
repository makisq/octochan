package workspace

import (
	"errors"
	"testing"
	"time"
)

// Итог инцидента — по тому, кто его закрыл: модель до срока, человек,
// эскалация по сроку, модель сама; эскалированный и потом решённый
// людьми считается и в «не справилась», и в «решены».
func TestSummarizeIncidents(t *testing.T) {
	base := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	all := []*Incident{
		{ID: "a", Level: "low", Opened: at(0), State: IncidentResolved, Resolved: at(4), ResolvedBy: "llm"},
		{ID: "b", Level: "low", Opened: at(1), State: IncidentResolved, Resolved: at(3), ResolvedBy: "llm"},
		{ID: "c", Level: "critical", Opened: at(2), State: IncidentResolved, Escalated: at(17), EscalatedBy: "ochan",
			Resolved: at(77), ResolvedBy: "master"},
		{ID: "d", Level: "critical", Opened: at(3), State: IncidentEscalated, Escalated: at(5), EscalatedBy: "llm",
			Mails: []IncidentMail{{Kind: "escalation", Error: "smtp: отказ"}}},
		{ID: "e", Level: "critical", Opened: at(4), State: IncidentOpen},
		{ID: "f", Level: "low", Opened: at(5), State: IncidentResolved, Resolved: at(6), ResolvedBy: "dba"},
		{ID: "old", Level: "low", Opened: at(-60), State: IncidentOpen},
	}
	s := SummarizeIncidents(all, at(0), at(60), "llm")
	if s.Total != 6 || s.ByModel != 2 || s.ByHuman != 1 || s.Timeout != 1 || s.GaveUp != 1 || s.Handed != 0 ||
		s.Fixed != 1 || s.Pending != 1 || s.Open != 1 || s.MailErrors != 1 {
		t.Fatalf("счётчики: %+v", s.IncidentCounts)
	}
	if s.Resolved() != 4 || s.Unresolved() != 2 || s.Escalated() != 2 {
		t.Fatalf("решены %d, не решены %d, эскалированы %d", s.Resolved(), s.Unresolved(), s.Escalated())
	}
	if mean, median := MeanMedian(s.ModelTimes); mean != 3*time.Minute || median != 3*time.Minute {
		t.Fatalf("время модели: %s, %s", mean, median)
	}
	if mean, _ := MeanMedian(s.EngineerTimes); mean != time.Hour {
		t.Fatalf("время инженеров: %s", mean)
	}
	if low, critical := s.Levels["low"], s.Levels["critical"]; low.Total != 3 || low.ByModel != 2 ||
		critical.Total != 3 || critical.Escalated() != 2 || critical.Unresolved() != 2 {
		t.Fatalf("по уровням: low %+v, critical %+v", low, critical)
	}
	if s.Incidents[0].ID != "a" || s.Incidents[len(s.Incidents)-1].ID != "f" {
		t.Fatal("инциденты периода — от старых к новым")
	}
}

// Окна отчётов идут подряд; первый вызов только начинает расписание;
// неудача не сдвигает окно, а повтор — не чаще ReportRetry.
func TestDueReport(t *testing.T) {
	store, err := NewIncidents(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	start := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	var windows [][2]time.Time
	fail := false
	send := func(from, to time.Time) ([]string, error) {
		if fail {
			return nil, errors.New("smtp недоступен")
		}
		windows = append(windows, [2]time.Time{from, to})
		return []string{"ops@corp.example"}, nil
	}
	due := func(now time.Time) ReportOutcome {
		t.Helper()
		_, outcome, err := store.DueReport(day, now, send)
		if err != nil && outcome != ReportFailed {
			t.Fatal(err)
		}
		return outcome
	}
	if due(start) != ReportStarted || due(start.Add(time.Hour)) != ReportNotDue {
		t.Fatal("первый вызов начинает расписание, отчёт — через сутки")
	}
	if due(start.Add(day+time.Minute)) != ReportSent || !windows[0][0].Equal(start) || !windows[0][1].Equal(start.Add(day)) {
		t.Fatalf("первое окно: %v", windows)
	}
	// Сервер стоял двое суток с лишним — один отчёт за оба окна.
	if due(start.Add(3*day+time.Hour)) != ReportSent || !windows[1][0].Equal(start.Add(day)) ||
		!windows[1][1].Equal(start.Add(3*day)) {
		t.Fatalf("пропущенные окна: %v", windows)
	}
	fail = true
	if due(start.Add(4*day)) != ReportFailed || due(start.Add(4*day+time.Minute)) != ReportNotDue {
		t.Fatal("неудача: повтор не раньше ReportRetry")
	}
	fail = false
	if due(start.Add(4*day+ReportRetry)) != ReportSent || !windows[2][0].Equal(start.Add(3*day)) {
		t.Fatalf("после неудачи окно то же: %v", windows)
	}
	r, _ := store.Report()
	if r.Error != "" || !r.Since.Equal(start.Add(4*day)) || r.To[0] != "ops@corp.example" {
		t.Fatalf("расписание: %+v", r)
	}
	if all, err := store.List(); err != nil || len(all) != 0 {
		t.Fatalf("расписание не инцидент: %v %v", all, err)
	}
}
