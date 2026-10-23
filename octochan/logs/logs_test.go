package logs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const pgLog = `2026-10-23 10:00:01 UTC [101] LOG:  database system is ready to accept connections
2026-10-23 10:00:05 UTC [102] WARNING:  could not open statistics file
2026-10-23 10:01:00 UTC [103] ERROR:  relation "users" does not exist at character 15
2026-10-23 10:01:07 UTC [104] ERROR:  relation "orders" does not exist at character 15

2026-10-23 10:02:00 UTC [105] FATAL:  password authentication failed for user "app"
2026-10-23 10:02:01 UTC [106] FATAL:  password authentication failed for user "app"
просто строка без уровня
`

func TestDefaultRulesRankPostgresLog(t *testing.T) {
	r, err := NewRanker(Settings{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := r.Scan(strings.NewReader(pgLog), func(e Entry) error {
		got = append(got, e.Level)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := "info warning error error fatal fatal info"
	if strings.Join(got, " ") != want {
		t.Fatalf("уровни %v, ожидалось %s", got, want)
	}
}

// Первое совпадение решает: правило выше по списку важнее.
func TestRulesFromConfigFirstMatchWins(t *testing.T) {
	r, err := NewRanker(Settings{
		Levels:  []string{"low", "mid", "high"},
		Default: "low",
		Min:     "mid",
		Rules: []Rule{
			{Level: "high", Match: `authentication failed`},
			{Level: "mid", Match: `(?:ERROR|FATAL):\s+(?P<msg>.*)`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if level, _ := r.Rank(`FATAL:  password authentication failed`); r.Levels()[level] != "high" {
		t.Fatalf("первое правило не сработало: %s", r.Levels()[level])
	}
	if level, msg := r.Rank(`ERROR:  relation "x" does not exist`); r.Levels()[level] != "mid" || msg != `relation "x" does not exist` {
		t.Fatalf("уровень %s, сообщение %q", r.Levels()[level], msg)
	}
	if r.Min() != "mid" {
		t.Fatalf("min %s", r.Min())
	}
}

func TestBadSettingsAreExplained(t *testing.T) {
	cases := []struct {
		s    Settings
		want string
	}{
		{Settings{Rules: []Rule{{Level: "oops", Match: "x"}}}, `logs.rules[1]: уровня "oops" нет`},
		{Settings{Rules: []Rule{{Level: "error", Match: "("}}}, `logs.rules[1]: выражение "("`},
		{Settings{Rules: []Rule{{Level: "error", Match: " "}}}, "пустое выражение"},
		{Settings{Min: "критично"}, `logs.min_level: уровня "критично" нет`},
		{Settings{Levels: []string{"a", "a"}}, "уровень a повторяется"},
	}
	for _, tc := range cases {
		if _, err := NewRanker(tc.s); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: ожидалась ошибка с %q, получено %v", tc.s, tc.want, err)
		}
	}
}

// Сводка: одинаковые сообщения с разными числами — одна группа; важнее
// и чаще — выше.
func TestSummary(t *testing.T) {
	r, err := NewRanker(Settings{})
	if err != nil {
		t.Fatal(err)
	}
	min, _ := r.Level("error")
	s, err := r.Summarize(strings.NewReader(pgLog), min)
	if err != nil {
		t.Fatal(err)
	}
	if s.Counts[1] != 2 || s.Counts[3] != 1 || s.Counts[4] != 2 || s.Counts[5] != 2 {
		t.Fatalf("счётчики %v", s.Counts)
	}
	if len(s.Groups) != 3 {
		t.Fatalf("группы %+v", s.Groups)
	}
	first := s.Groups[0]
	if first.Level != "fatal" || first.Count != 2 || first.First != 6 {
		t.Fatalf("первая группа %+v", first)
	}
	if s.Groups[1].Level != "error" || s.Groups[1].Count != 1 {
		t.Fatalf("вторая группа %+v", s.Groups[1])
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"100MB": 100 << 20, "512kb": 512 << 10, "1GB": 1 << 30, "10": 10 << 20, "64 B": 64, "2 МБ": 2 << 20}
	for text, want := range cases {
		if got, err := ParseSize(text); err != nil || got != want {
			t.Errorf("%s: %d, %v", text, got, err)
		}
	}
	for _, bad := range []string{"", "MB", "10TB", "-1MB", "1.5MB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
	}
}

func TestPrefix(t *testing.T) {
	at := time.Date(2026, 10, 23, 9, 5, 7, 0, time.UTC)
	if got := FormatPrefix(DefaultPrefix, at); got != "2026-10-23_09-05-07_" {
		t.Fatalf("префикс %q", got)
	}
	if got := FormatPrefix("журнал-%d.%m.%Y-100%%-", at); got != "журнал-23.10.2026-100%-" {
		t.Fatalf("префикс %q", got)
	}
	for _, bad := range []string{"%Y/%m-", "..%Y", "%q", "a%"} {
		if err := CheckPrefix(bad); err == nil {
			t.Errorf("префикс %q принят", bad)
		}
	}
	if err := CheckPrefix("%Y%m%d-"); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// Журнал, который не помещает очередную запись, уходит в архив с датой,
// а запись идёт в новый файл. Архивов — не больше Keep, чужие файлы
// каталога не трогаются.
func TestRotation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "чужой.octochan.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	r := &Rotator{Dir: dir, Name: "octochan.log", MaxSize: 10, Prefix: "%Y-%m-%d_", Keep: 2,
		Now: func() time.Time { return clock }}
	defer r.Close()

	write := func(text string) {
		t.Helper()
		if _, err := r.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	write("12345")
	write("67890") // ровно 10 — помещается
	write("abc")   // не помещается: перенос
	if got := names(t, dir); strings.Join(got, " ") != "2026-10-23_octochan.log octochan.log чужой.octochan.log" {
		t.Fatalf("файлы: %v", got)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "2026-10-23_octochan.log")); string(data) != "1234567890" {
		t.Fatalf("архив: %q", data)
	}

	// Второй перенос в тот же день не затирает первый.
	write("defghijk")
	if got := names(t, dir); !contains(got, "2026-10-23_1.octochan.log") {
		t.Fatalf("файлы: %v", got)
	}

	// Третий: архивов три, хранится два — старейший удалён.
	clock = clock.Add(24 * time.Hour)
	write("lmnopqrstu")
	got := names(t, dir)
	if contains(got, "2026-10-23_octochan.log") || !contains(got, "2026-10-24_octochan.log") ||
		!contains(got, "чужой.octochan.log") || len(got) != 4 {
		t.Fatalf("файлы: %v", got)
	}

	archives, err := Archives(dir, "octochan.log", "%Y-%m-%d_")
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 3 || filepath.Base(archives[0].Path) != "octochan.log" {
		t.Fatalf("архивы: %+v", archives)
	}
}

// Журнал перенёс другой процесс: запись идёт в новый файл, а не в архив.
func TestRotationByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	a := &Rotator{Dir: dir, Name: "j.log"}
	defer a.Close()
	if _, err := a.Write([]byte("до\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "j.log"), filepath.Join(dir, "old_j.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("после\n")); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "j.log")); string(data) != "после\n" {
		t.Fatalf("j.log: %q", data)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
