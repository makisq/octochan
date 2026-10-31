package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"octochan/rbac"
	"octochan/workspace"
)

// Письма ochan формальным промптом.
//
// Когда людям пишут формальным промптом (mail.lang и режим модели), письма
// самого ochan — эскалация, решение инцидента, статистика — тоже на языке:
// факты — ячейками set, цель — то, о чём письмо. Тексты, которые принесли
// мониторинг и модель (что случилось, заметки), — записи, а не речь
// ochan: они уходят вложением, как журналы.

var notName = regexp.MustCompile(`[^a-z0-9_]+`)

// formalName — имя языка из чего угодно: web-1.example → web_1_example;
// с цифры имя не начинается — prefix.
func formalName(text, prefix string) string {
	name := strings.Trim(notName.ReplaceAllString(strings.ToLower(text), "_"), "_")
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		name = strings.TrimSuffix(prefix+"_"+name, "_")
	}
	return name
}

// formalSpan — срок числом с единицей языка: 7 d, 2 h, 15 min, 84 s.
func formalSpan(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d d", d/(24*time.Hour))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d h", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return fmt.Sprintf("%d s", d/time.Second)
}

type formalDoc struct {
	goal    string
	classes [][2]string // класс, слово
	cells   []formalCell
}

type formalCell struct {
	name string
	sets [][2]string // цель.атрибут, значение
}

func (d *formalDoc) cell(name string) *formalCell {
	d.cells = append(d.cells, formalCell{name: name})
	return &d.cells[len(d.cells)-1]
}

func (c *formalCell) set(target, value string) { c.sets = append(c.sets, [2]string{target, value}) }

func (d *formalDoc) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "goal: %s\n\nclasses:\n", d.goal)
	width := 0
	for _, c := range d.classes {
		width = max(width, len(c[0]))
	}
	for _, c := range d.classes {
		fmt.Fprintf(&b, "  %-*s : %q\n", width, c[0], c[1])
	}
	for _, c := range d.cells {
		if len(c.sets) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\ncell %s:\n", c.name)
		for _, s := range c.sets {
			fmt.Fprintf(&b, "  set %s = %s\n", s[0], s[1])
		}
	}
	return b.String()
}

// formalIncident — инцидент формальным промптом.
func formalIncident(i *workspace.Incident) string {
	doc := &formalDoc{goal: "failure", classes: [][2]string{{"Failure", "failure"}}}
	requests := incidentRequests(i.ID)
	if len(requests) > 0 {
		doc.classes = append(doc.classes, [2]string{"Request", "request"})
	}
	if len(i.Logs) > 0 {
		doc.classes = append(doc.classes, [2]string{"Log", "log"})
	}

	c := doc.cell("incident")
	c.set("Failure.incident", "incident_"+i.ID)
	c.set("Failure.level", formalName(i.Level, "level"))
	if timer, err := findIncidentLevel(i.Level); err == nil {
		c.set("Failure.timer", formalSpan(timer.Timer))
	}
	c.set("Failure.state", string(i.State))
	c.set("Failure.result", string(i.Result(rbac.LLM)))
	c.set("Failure.source", formalName(i.Source, "source"))
	for n, h := range i.Hosts {
		attr := "host"
		if n > 0 {
			attr = fmt.Sprintf("host_%d", n+1)
		}
		c.set("Failure."+attr, formalName(h, "host"))
	}
	if !i.Escalated.IsZero() {
		c.set("Failure.escalated_by", formalName(i.EscalatedBy, "role"))
		c.set("Failure.escalated_after", formalSpan(i.Escalated.Sub(i.Opened)))
	}
	if !i.Resolved.IsZero() {
		c.set("Failure.resolved_by", formalName(i.ResolvedBy, "role"))
		c.set("Failure.took", formalSpan(i.Resolved.Sub(i.Opened)))
	}
	c.set("Failure.steps", fmt.Sprint(len(i.Steps)))

	if len(requests) > 0 {
		r := doc.cell("requests")
		for _, req := range requests {
			r.set("Request.request_"+req.ID, string(req.State))
		}
	}
	if len(i.Logs) > 0 {
		l := doc.cell("logs")
		for _, log := range i.Logs {
			l.set("Log."+formalName(log.Name, "log"), fmt.Sprintf("%d B", len(log.Content)))
		}
	}
	return doc.String()
}

// incidentRecords — записи инцидента как есть: что случилось и шаги. Это
// данные — их написали мониторинг и модель, — и в формальном письме они
// вложение, а не текст письма.
func incidentRecords(i *workspace.Incident) []byte {
	var b strings.Builder
	// Многострочная запись (формальный промпт своей модели) — с отступом
	// под своим заголовком.
	field := func(text string) string {
		if !strings.Contains(strings.TrimSpace(text), "\n") {
			return " " + text
		}
		return "\n" + indent(text, "    ")
	}
	fmt.Fprintf(&b, "incident %s\nsummary:%s\n", i.ID, field(i.Summary))
	if i.Outcome != "" {
		fmt.Fprintf(&b, "outcome:%s\n", field(i.Outcome))
	}
	b.WriteString("steps:\n")
	for _, s := range i.Steps {
		ref := ""
		if s.Ref != "" {
			ref = " " + s.Ref
		}
		fmt.Fprintf(&b, "  %s  %s  %s%s:%s\n", s.Time.UTC().Format(time.RFC3339), s.Who, s.Kind, ref, field(s.Text))
	}
	return []byte(b.String())
}

// formalStats — статистика инцидентов формальным промптом.
func formalStats(s *workspace.IncidentStats) string {
	doc := &formalDoc{goal: "count", classes: [][2]string{{"Count", "count"}, {"Period", "period"}}}
	p := doc.cell("period")
	if s.From.IsZero() {
		p.set("Period.length", "all")
	} else {
		p.set("Period.length", formalSpan(s.To.Sub(s.From)))
	}

	c := doc.cell("incidents")
	for _, kv := range [][2]any{
		{"total", s.Total}, {"resolved", s.Resolved()}, {"resolved_by_model", s.ByModel},
		{"resolved_by_human", s.ByHuman}, {"resolved_after_escalation", s.Fixed},
		{"unresolved", s.Unresolved()}, {"with_engineers", s.Pending}, {"with_model", s.Open},
		{"escalated", s.Escalated()}, {"timeout", s.Timeout}, {"gave_up", s.GaveUp}, {"handed", s.Handed},
	} {
		c.set("Count."+kv[0].(string), fmt.Sprint(kv[1]))
	}
	if len(s.ModelTimes) > 0 {
		mean, median := workspace.MeanMedian(s.ModelTimes)
		c.set("Count.model_mean", formalSpan(mean))
		c.set("Count.model_median", formalSpan(median))
	}
	if len(s.EngineerTimes) > 0 {
		mean, median := workspace.MeanMedian(s.EngineerTimes)
		c.set("Count.engineers_mean", formalSpan(mean))
		c.set("Count.engineers_median", formalSpan(median))
	}
	if s.MailErrors > 0 {
		c.set("Count.mail_errors", fmt.Sprint(s.MailErrors))
	}

	if len(s.Levels) > 0 {
		doc.classes = append(doc.classes, [2]string{"Level", "level"})
		l := doc.cell("levels")
		for _, name := range statsLevels(s) {
			lc, level := s.Levels[name], formalName(name, "level")
			l.set("Level."+level+"_total", fmt.Sprint(lc.Total))
			l.set("Level."+level+"_model", fmt.Sprint(lc.ByModel))
			l.set("Level."+level+"_escalated", fmt.Sprint(lc.Escalated()))
			l.set("Level."+level+"_unresolved", fmt.Sprint(lc.Unresolved()))
		}
	}

	var open []*workspace.Incident
	for _, i := range s.Incidents {
		if i.State != workspace.IncidentResolved {
			open = append(open, i)
		}
	}
	if len(open) > 0 {
		doc.classes = append(doc.classes, [2]string{"Failure", "failure"})
		u := doc.cell("unresolved")
		sort.SliceStable(open, func(a, b int) bool { return open[a].Opened.Before(open[b].Opened) })
		for _, i := range open {
			where := "with_model"
			if i.State == workspace.IncidentEscalated {
				where = "with_engineers"
			}
			u.set("Failure.incident_"+i.ID, where)
		}
	}
	return doc.String()
}
