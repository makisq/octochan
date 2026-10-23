// Package logs — журналы: уровни важности строк и перенос журнала в
// архив по размеру.
//
// Уровень строки задают правила из конфига — регулярные выражения по
// порядку, первое совпадение решает, как записи pg_hba.conf. Порядок
// уровней — от наименее к наиболее важному, как log_min_messages у
// PostgreSQL: «не ниже warning» отсекает всё, что левее warning.
//
// Выражения — RE2 из стандартной библиотеки: время разбора линейно, и ни
// правило из конфига, ни строка чужого журнала не подвесят разбор.
package logs

import (
	"bufio"
	"io"
	"regexp"
	"sort"
	"strings"

	"octochan/i18n"
)

// DefaultLevels — уровни по умолчанию, от наименее важного.
var DefaultLevels = []string{"debug", "info", "notice", "warning", "error", "fatal", "panic"}

// DefaultRules — правила по умолчанию: метки, которые пишут PostgreSQL,
// pgbouncer, systemd и большинство программ. Суть сообщения — то, что
// после метки: время и номер процесса в сводку не попадают.
var DefaultRules = []Rule{
	{Level: "panic", Match: marker(`PANIC`)},
	{Level: "fatal", Match: marker(`FATAL|CRITICAL|CRIT|EMERG|ALERT`)},
	{Level: "error", Match: marker(`ERROR|ERR`)},
	{Level: "warning", Match: marker(`WARNING|WARN`)},
	{Level: "notice", Match: marker(`NOTICE`)},
	{Level: "info", Match: marker(`INFO|LOG`)},
	{Level: "debug", Match: marker(`DEBUG[1-5]?|TRACE`)},
}

func marker(names string) string { return `\b(?:` + names + `)\b[\]):]*\s*(?P<msg>.*)` }

// Rule — правило: строка, совпавшая с Match, получает уровень Level.
// Группа (?P<msg>…) в выражении выделяет суть сообщения для сводки.
type Rule struct {
	Level string `mapstructure:"level" json:"level"`
	Match string `mapstructure:"match" json:"match"`
}

// Settings — настройки ранжирования из конфига.
type Settings struct {
	Levels []string
	Rules  []Rule
	// Default — уровень строки, которой не подошло ни одно правило.
	Default string
	// Min — уровень, ниже которого строки не показываются.
	Min string
}

// Ranker — скомпилированные правила.
type Ranker struct {
	levels []string
	index  map[string]int
	rules  []compiled
	def    int
	min    int
}

type compiled struct {
	level int
	re    *regexp.Regexp
	msg   int // номер группы msg; -1 — её нет
}

// NewRanker проверяет настройки и компилирует правила. Ошибка называет
// правило по номеру: конфиг правят руками.
func NewRanker(s Settings) (*Ranker, error) {
	levels := s.Levels
	if len(levels) == 0 {
		levels = DefaultLevels
	}
	r := &Ranker{index: make(map[string]int, len(levels))}
	for i, level := range levels {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" {
			return nil, i18n.Errorf("logs.levels: пустое имя уровня")
		}
		if _, dup := r.index[level]; dup {
			return nil, i18n.Errorf("logs.levels: уровень %s повторяется", level)
		}
		r.index[level] = i
		r.levels = append(r.levels, level)
	}

	var err error
	if r.def, err = r.level("logs.default_level", s.Default, "info"); err != nil {
		return nil, err
	}
	if r.min, err = r.level("logs.min_level", s.Min, r.levels[0]); err != nil {
		return nil, err
	}

	rules := s.Rules
	if len(rules) == 0 {
		rules = DefaultRules
	}
	for i, rule := range rules {
		level, ok := r.index[strings.ToLower(strings.TrimSpace(rule.Level))]
		if !ok {
			return nil, i18n.Errorf("logs.rules[%d]: уровня %q нет в logs.levels (%s)",
				i+1, rule.Level, strings.Join(r.levels, ", "))
		}
		if strings.TrimSpace(rule.Match) == "" {
			return nil, i18n.Errorf("logs.rules[%d]: пустое выражение match", i+1)
		}
		re, err := regexp.Compile(rule.Match)
		if err != nil {
			return nil, i18n.Errorf("logs.rules[%d]: выражение %q: %v", i+1, rule.Match, err)
		}
		r.rules = append(r.rules, compiled{level: level, re: re, msg: re.SubexpIndex("msg")})
	}
	return r, nil
}

func (r *Ranker) level(key, name, fallback string) (int, error) {
	if strings.TrimSpace(name) == "" {
		name = fallback
	}
	i, ok := r.index[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		// Уровни по умолчанию заменили своими, а default_level не
		// поменяли: берём самый нижний, а не отказываем.
		if name == fallback {
			return 0, nil
		}
		return 0, i18n.Errorf("%s: уровня %q нет в logs.levels (%s)", key, name, strings.Join(r.levels, ", "))
	}
	return i, nil
}

// Levels возвращает уровни от наименее важного.
func (r *Ranker) Levels() []string { return append([]string(nil), r.levels...) }

// Min возвращает уровень по умолчанию для отбора.
func (r *Ranker) Min() string { return r.levels[r.min] }

// Level возвращает номер уровня по имени.
func (r *Ranker) Level(name string) (int, error) {
	i, ok := r.index[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return 0, i18n.Errorf("уровня %q нет; уровни: %s", name, strings.Join(r.levels, ", "))
	}
	return i, nil
}

// Entry — строка журнала с уровнем.
type Entry struct {
	Line  int
	Level string
	Rank  int
	Text  string
	// Message — суть сообщения: группа msg правила или вся строка, где
	// числа заменены на N, — так одинаковые сообщения с разными
	// значениями попадают в одну группу сводки.
	Message string
}

var digits = regexp.MustCompile(`\d+`)

// Rank определяет уровень строки.
func (r *Ranker) Rank(line string) (int, string) {
	for _, rule := range r.rules {
		m := rule.re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if rule.msg >= 0 && m[rule.msg] != "" {
			return rule.level, digits.ReplaceAllString(strings.TrimSpace(m[rule.msg]), "N")
		}
		return rule.level, digits.ReplaceAllString(strings.TrimSpace(line), "N")
	}
	return r.def, digits.ReplaceAllString(strings.TrimSpace(line), "N")
}

// maxLine — предел длины строки журнала. Длиннее — ошибка с номером
// строки, а не молчаливая обрезка.
const maxLine = 4 << 20

// Scan читает журнал и передаёт каждую строку с уровнем.
func (r *Ranker) Scan(in io.Reader, each func(Entry) error) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maxLine)
	n := 0
	for scanner.Scan() {
		n++
		text := scanner.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		rank, msg := r.Rank(text)
		if err := each(Entry{Line: n, Level: r.levels[rank], Rank: rank, Text: text, Message: msg}); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return i18n.Errorf("строка %d: %v", n+1, err)
	}
	return nil
}

// Group — одинаковые сообщения одного уровня.
type Group struct {
	Level   string
	Rank    int
	Message string
	Count   int
	// First — номер первой строки группы.
	First int
}

// Summary — сводка журнала.
type Summary struct {
	// Counts — число строк по уровням, в порядке уровней.
	Counts []int
	Groups []Group
}

// Summarize считает строки по уровням и группирует сообщения уровня min
// и выше.
func (r *Ranker) Summarize(in io.Reader, min int) (*Summary, error) {
	s := &Summary{Counts: make([]int, len(r.levels))}
	index := map[string]int{}
	err := r.Scan(in, func(e Entry) error {
		s.Counts[e.Rank]++
		if e.Rank < min {
			return nil
		}
		key := e.Level + "\x00" + e.Message
		if i, ok := index[key]; ok {
			s.Groups[i].Count++
			return nil
		}
		index[key] = len(s.Groups)
		s.Groups = append(s.Groups, Group{Level: e.Level, Rank: e.Rank, Message: e.Message, Count: 1, First: e.Line})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Важнее — выше; при равенстве — чаще; при равенстве — раньше.
	sort.SliceStable(s.Groups, func(i, j int) bool {
		a, b := s.Groups[i], s.Groups[j]
		if a.Rank != b.Rank {
			return a.Rank > b.Rank
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.First < b.First
	})
	return s, nil
}
