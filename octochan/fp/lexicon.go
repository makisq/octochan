package fp

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// LexiconName — словарь пакета алиасов: слова, которые скилл приносит в
// промпт через import. Лежит рядом с package.yaml.
const LexiconName = "lexicon.yaml"

//go:embed lexicon.yaml
var baseLexicon []byte

// Keywords — ключевые слова языка. Они заняты грамматикой: ни именем, ни
// словом словаря быть не могут. Типы отношений R — тоже ключевые слова.
var Keywords = append([]string{
	"goal", "classes", "relations", "priority", "generalization", "instance", "requires", "import",
	"provides", "requires_class", "hard", "forbid", "require", "cell", "set", "binds_to", "report",
	"q", "choice", "alternatives", "basis", "except", "scope", "not_in_prompt", "skill", "relation", "rule",
}, RelationTypes...)

var keywords = func() map[string]bool {
	set := make(map[string]bool, len(Keywords))
	for _, k := range Keywords {
		set[k] = true
	}
	return set
}()

// IsKeyword сообщает, что слово занято грамматикой.
func IsKeyword(word string) bool { return keywords[word] }

var (
	lexWordPattern  = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	lexSensePattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	lexUnitPattern  = regexp.MustCompile(`^[A-Za-z]+$`)
)

// Entry — статья словаря: ровно n смыслов слова и nl — сколько их у
// слова в естественном языке.
type Entry struct {
	// Senses — смыслы слова в языке, классы. Их ровно столько, сколько
	// записано: промпт смыслов не добавляет и не убирает.
	Senses []string
	// NL — смыслов у слова в естественном языке (для базового словаря —
	// по WordNet 3.0). Язык строже естественного: len(Senses) < NL, а
	// при NL = 1 смысл один.
	NL int
	// Sources — откуда статья: base, @скилл, путь словаря оператора.
	Sources []string
}

// N — число смыслов слова в языке.
func (e *Entry) N() int { return len(e.Senses) }

// Lexicon — словарь языка W с отображением μ: слово → его смыслы.
//
// Язык замкнут: слово со смыслом в промпте — только из словаря. Словарь
// складывается из базового, словарей оператора и словарей скиллов,
// которые подключил промпт.
type Lexicon struct {
	words    map[string]*Entry
	function map[string]string
	units    map[string]string
}

// lexiconFile — словарь на диске.
type lexiconFile struct {
	Words map[string]struct {
		Senses []string `yaml:"senses"`
		NL     int      `yaml:"nl"`
	} `yaml:"words"`
	Function []string `yaml:"function"`
	Units    []string `yaml:"units"`
}

// ParseLexicon разбирает словарь. source — откуда он: попадает в статьи
// и в сообщения.
func ParseLexicon(source string, data []byte) (*Lexicon, error) {
	var file lexiconFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// Пустой файл — пустой словарь, а не ошибка.
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", source, err)
	}

	lex := newLexicon()
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	for word, raw := range file.Words {
		switch {
		case !lexWordPattern.MatchString(word):
			fail("слово %q: одно английское слово из строчных латинских букв и цифр; составные имена "+
				"собираются в промпте через _", word)
			continue
		case IsKeyword(word):
			fail("слово %q — ключевое слово языка, словом словаря быть не может", word)
			continue
		}
		entry := &Entry{NL: raw.NL, Sources: []string{source}}
		seen := map[string]bool{}
		for _, sense := range raw.Senses {
			if !lexSensePattern.MatchString(sense) {
				fail("слово %q: смысл %q — класс с заглавной латинской буквы", word, sense)
				continue
			}
			if seen[sense] {
				fail("слово %q: смысл %s повторяется", word, sense)
				continue
			}
			seen[sense] = true
			entry.Senses = append(entry.Senses, sense)
		}
		if err := entry.validate(); err != nil {
			fail("слово %q: %v", word, err)
			continue
		}
		lex.words[word] = entry
	}

	for _, word := range file.Function {
		switch {
		case !lexWordPattern.MatchString(word):
			fail("служебное слово %q: строчные латинские буквы и цифры", word)
		case IsKeyword(word):
			fail("служебное слово %q — ключевое слово языка", word)
		case lex.words[word] != nil:
			fail("слово %q и служебное, и со смыслом: у служебного слова класса нет", word)
		case lex.function[word] != "":
			fail("служебное слово %q повторяется", word)
		default:
			lex.function[word] = source
		}
	}
	for _, unit := range file.Units {
		switch {
		case !lexUnitPattern.MatchString(unit):
			fail("единица %q: латинские буквы (km, MB); «%%» есть в языке и так", unit)
		case lex.units[unit] != "":
			fail("единица %q повторяется", unit)
		default:
			lex.units[unit] = source
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("%s: %s", source, strings.Join(errs, "; "))
	}
	return lex, nil
}

// validate — 1 ≤ n < nl; при nl = 1 смысл один.
func (e *Entry) validate() error {
	n := len(e.Senses)
	switch {
	case n == 0:
		return errors.New("нет смыслов: у слова языка их от одного")
	case e.NL < 1:
		return errors.New("не указано nl — сколько смыслов у слова в естественном языке")
	case e.NL == 1 && n > 1:
		return fmt.Errorf("в естественном языке у слова один смысл, а в словаре %d: язык не может быть "+
			"многозначнее естественного", n)
	case e.NL > 1 && n >= e.NL:
		return fmt.Errorf("смыслов %d, а в естественном языке %d: в языке их должно быть меньше (n < nl)", n, e.NL)
	}
	return nil
}

func newLexicon() *Lexicon {
	return &Lexicon{words: map[string]*Entry{}, function: map[string]string{}, units: map[string]string{}}
}

// LoadLexicon читает словарь из файла.
func LoadLexicon(path string) (*Lexicon, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseLexicon(path, data)
}

var (
	baseOnce sync.Once
	base     *Lexicon
)

// BaseLexicon — базовый словарь, встроенный в ochan. Возвращает копию:
// словари скиллов и оператора сливаются с ней, не трогая общий.
func BaseLexicon() *Lexicon {
	baseOnce.Do(func() {
		lex, err := ParseLexicon("base", baseLexicon)
		if err != nil {
			// Базовый словарь проверяется тестом пакета: сюда попасть
			// можно только со сломанной сборкой.
			panic(err)
		}
		base = lex
	})
	return base.Clone()
}

// Clone — независимая копия словаря.
func (l *Lexicon) Clone() *Lexicon {
	c := newLexicon()
	for w, e := range l.words {
		c.words[w] = &Entry{Senses: append([]string(nil), e.Senses...), NL: e.NL,
			Sources: append([]string(nil), e.Sources...)}
	}
	for w, s := range l.function {
		c.function[w] = s
	}
	for u, s := range l.units {
		c.units[u] = s
	}
	return c
}

// Merge добавляет в словарь слова другого. Смыслы одного слова из
// разных словарей объединяются (П4: одноимённость), но язык и после
// слияния строже естественного: n < nl. nl у слова одно — разные nl
// значат, что словари говорят о разных словах.
func (l *Lexicon) Merge(other *Lexicon) error {
	var errs []string
	for _, word := range sortedKeys(other.words) {
		add := other.words[word]
		if l.function[word] != "" {
			errs = append(errs, fmt.Sprintf("слово %q в %s служебное, а в %s со смыслом", word,
				l.function[word], strings.Join(add.Sources, ", ")))
			continue
		}
		have, ok := l.words[word]
		if !ok {
			l.words[word] = &Entry{Senses: append([]string(nil), add.Senses...), NL: add.NL,
				Sources: append([]string(nil), add.Sources...)}
			continue
		}
		if have.NL != add.NL {
			errs = append(errs, fmt.Sprintf("слово %q: nl %d в %s и %d в %s", word, have.NL,
				strings.Join(have.Sources, ", "), add.NL, strings.Join(add.Sources, ", ")))
			continue
		}
		merged := &Entry{Senses: append([]string(nil), have.Senses...), NL: have.NL,
			Sources: append([]string(nil), have.Sources...)}
		for _, sense := range add.Senses {
			if !contains(merged.Senses, sense) {
				merged.Senses = append(merged.Senses, sense)
			}
		}
		for _, src := range add.Sources {
			if !contains(merged.Sources, src) {
				merged.Sources = append(merged.Sources, src)
			}
		}
		if err := merged.validate(); err != nil {
			errs = append(errs, fmt.Sprintf("слово %q после слияния %s: %v", word, strings.Join(merged.Sources, ", "), err))
			continue
		}
		l.words[word] = merged
	}
	for _, word := range sortedKeys(other.function) {
		if l.words[word] != nil {
			errs = append(errs, fmt.Sprintf("слово %q в %s со смыслом, а в %s служебное", word,
				strings.Join(l.words[word].Sources, ", "), other.function[word]))
			continue
		}
		if l.function[word] == "" {
			l.function[word] = other.function[word]
		}
	}
	for _, unit := range sortedKeys(other.units) {
		if l.units[unit] == "" {
			l.units[unit] = other.units[unit]
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Labeled — копия словаря, у статей которой источник — source: так
// словарь пакета подписан именем скилла, а не путём к файлу.
func (l *Lexicon) Labeled(source string) *Lexicon {
	c := l.Clone()
	for _, e := range c.words {
		e.Sources = []string{source}
	}
	for w := range c.function {
		c.function[w] = source
	}
	for u := range c.units {
		c.units[u] = source
	}
	return c
}

// Lookup — статья слова со смыслом.
func (l *Lexicon) Lookup(word string) (*Entry, bool) {
	e, ok := l.words[word]
	return e, ok
}

// IsFunction — служебное слово.
func (l *Lexicon) IsFunction(word string) bool { return l.function[word] != "" }

// IsUnit — единица измерения.
func (l *Lexicon) IsUnit(unit string) bool { return l.units[unit] != "" }

// Words — слова со смыслом по алфавиту.
func (l *Lexicon) Words() []string { return sortedKeys(l.words) }

// FunctionWords — служебные слова по алфавиту.
func (l *Lexicon) FunctionWords() []string { return sortedKeys(l.function) }

// Units — единицы измерения по алфавиту.
func (l *Lexicon) Units() []string { return sortedKeys(l.units) }

// WordsWithin — слова, которые входят в язык при потолке k: у слова не
// больше k смыслов.
func (l *Lexicon) WordsWithin(k int) []string {
	var words []string
	for _, w := range l.Words() {
		if l.words[w].N() <= k {
			words = append(words, w)
		}
	}
	return words
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
