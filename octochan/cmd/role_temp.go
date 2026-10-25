package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"octochan/audit"
	"octochan/i18n"
	"octochan/rbac"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

// Сроки прав и ролей: --for 2h или --until "2026-10-24 18:00".

// tempFlags — флаги срока одной команды.
type tempFlags struct {
	forValue   string
	untilValue string
}

func (f *tempFlags) add(cmd *cobra.Command, what string) {
	cmd.Flags().StringVar(&f.forValue, "for", "", what+" на время: 30m, 2h, 1d")
	cmd.Flags().StringVar(&f.untilValue, "until", "", what+" до момента: \"2026-10-24 18:00\" или 18:00 сегодня")
}

// until — срок из флагов; ok — срок задан.
func (f *tempFlags) until(now time.Time) (time.Time, bool, error) {
	switch {
	case f.forValue != "" && f.untilValue != "":
		return time.Time{}, false, errors.New("укажите --for или --until, не оба")
	case f.forValue != "":
		d, err := rbac.ParseFor(f.forValue)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("--for: %w", err)
		}
		return now.Add(d), true, nil
	case f.untilValue != "":
		t, err := parseUntil(f.untilValue, now)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("--until: %w", err)
		}
		if err := rbac.CheckUntil(t, now); err != nil {
			return time.Time{}, false, fmt.Errorf("--until: %w", err)
		}
		return t, true, nil
	}
	return time.Time{}, false, nil
}

// parseUntil — момент: RFC 3339, «2006-01-02 15:04» или «15:04» сегодня,
// по местному времени.
func parseUntil(text string, now time.Time) (time.Time, error) {
	text = strings.TrimSpace(text)
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("15:04", text, time.Local); err == nil {
		local := now.In(time.Local)
		return time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, time.Local), nil
	}
	return time.Time{}, errors.New("момент вида \"2026-10-24 18:00\", 18:00 или RFC 3339")
}

// formatUntil — срок и сколько осталось.
func formatUntil(until, now time.Time) string {
	left := until.Sub(now).Round(time.Minute)
	if left < time.Minute {
		left = time.Minute
	}
	return fmt.Sprintf("%s (осталось %s)", until.Local().Format("2006-01-02 15:04"), shortDuration(left))
}

// shortDuration — срок в днях, часах и минутах: 1d, 2h, 1h30m, 45m.
func shortDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := d / (24 * time.Hour)
	hours := d % (24 * time.Hour) / time.Hour
	minutes := d % time.Hour / time.Minute
	var b strings.Builder
	if days > 0 {
		fmt.Fprintf(&b, "%dd", days)
	}
	if hours > 0 {
		fmt.Fprintf(&b, "%dh", hours)
	}
	if minutes > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%dm", minutes)
	}
	return b.String()
}

// tempTexts — временные права или запреты со сроками.
func tempTexts(list []rbac.TempPerm, now time.Time) []string {
	texts := make([]string, len(list))
	for i, t := range list {
		texts[i] = t.Perm + " до " + formatUntil(t.Until, now)
	}
	return texts
}

// expireRole возвращает роль, срок которой истёк, и пишет это в журнал
// аудита: смена роли без команды — тоже событие.
func expireRole(cmd *cobra.Command, store *rbac.Store, path string) error {
	from, to, ok := store.Expire(rbac.Now())
	if !ok {
		return nil
	}
	if err := store.Save(path); err != nil {
		return err
	}
	cmd.PrintErrln(i18n.Sprintf("Срок роли %s истёк: текущая роль %s", from, to))
	if journal, err := auditJournal(); err == nil {
		_ = journal.Append(audit.Record{Event: audit.EventRole, Role: from, Command: "role.expire",
			Args: []string{to}, User: workspace.CurrentUser()}, func(audit.Recent) error { return nil })
	}
	return nil
}
