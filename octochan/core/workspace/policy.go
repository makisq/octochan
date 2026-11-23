package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Политика автоматического выполнения.
//
// ochan watch проверяет и планирует сам, по расписанию, а выполняет сам
// только то, что укладывается в политику: белые списки меток целей и
// параметров сценария, срок. Политику регистрируют (ochan policy add —
// право approve, это заранее данное согласование), её видно (ochan
// policy) и её отзывают (ochan policy revoke) — отзыв действует со
// следующего прохода watch. Всё, что вне политики, становится заявкой, а
// не изменением.

// Policy — зарегистрированная политика.
type Policy struct {
	Name string `json:"name"`
	// Scenario — сценарий исполнителя (полный путь), который watch строит
	// для нового хоста; Service — его сервис при регистрации: сценарий,
	// переписанный на другой сервис, политика не покрывает.
	Scenario string `json:"scenario"`
	Service  string `json:"service"`
	// Targets — селекторы инвентаря: хост вне них — заявка.
	Targets []string `json:"targets"`
	// Params — параметры сценария, которые можно выставлять без человека,
	// и их допустимые значения.
	Params  map[string]ParamRule `json:"params"`
	Expires time.Time            `json:"expires"`

	// File и Sum — файл политики и его SHA-256 при регистрации.
	File string `json:"file"`
	Sum  string `json:"sum"`

	AddedBy   string    `json:"added_by"`
	AddedRole string    `json:"added_role,omitempty"`
	AddedAt   time.Time `json:"added_at"`

	RevokedBy    string    `json:"revoked_by,omitempty"`
	RevokedAt    time.Time `json:"revoked_at,omitempty"`
	RevokeReason string    `json:"revoke_reason,omitempty"`
}

// ParamRule — допустимые значения параметра: любое, из списка или
// числа в диапазоне.
type ParamRule struct {
	Any    bool     `json:"any,omitempty"`
	Values []string `json:"values,omitempty"`
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
}

// Allows сообщает, что значение допустимо.
func (r ParamRule) Allows(value string) bool {
	switch {
	case r.Any:
		return true
	case r.Min != nil || r.Max != nil:
		n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && (r.Min == nil || n >= *r.Min) && (r.Max == nil || n <= *r.Max)
	}
	for _, v := range r.Values {
		if v == value {
			return true
		}
	}
	return false
}

func (r ParamRule) String() string {
	switch {
	case r.Any:
		return "любое"
	case r.Min != nil || r.Max != nil:
		lo, hi := "", ""
		if r.Min != nil {
			lo = strconv.FormatFloat(*r.Min, 'f', -1, 64)
		}
		if r.Max != nil {
			hi = strconv.FormatFloat(*r.Max, 'f', -1, 64)
		}
		return lo + ".." + hi
	}
	return strings.Join(r.Values, " | ")
}

// ExpiryText — срок для людей: дата из файла — «по 2026-12-31
// включительно», время — как есть.
func (p *Policy) ExpiryText() string {
	local := p.Expires.Local()
	if local.Hour() == 0 && local.Minute() == 0 && local.Second() == 0 {
		return "по " + local.AddDate(0, 0, -1).Format("2006-01-02") + " включительно"
	}
	return "до " + local.Format("2006-01-02 15:04")
}

// State — действует ли политика сейчас; ошибка объясняет, почему нет.
func (p *Policy) State(now time.Time) error {
	switch {
	case !p.RevokedAt.IsZero():
		reason := ""
		if p.RevokeReason != "" {
			reason = ": " + p.RevokeReason
		}
		return fmt.Errorf("политика %s отозвана %s (%s)%s", p.Name,
			p.RevokedAt.Local().Format("2006-01-02 15:04"), p.RevokedBy, reason)
	case !now.Before(p.Expires):
		return fmt.Errorf("срок политики %s истёк: она действовала %s", p.Name, p.ExpiryText())
	}
	return nil
}

// ParamNames — параметры политики по алфавиту.
func (p *Policy) ParamNames() []string {
	names := make([]string, 0, len(p.Params))
	for name := range p.Params {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var policyName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

// policyFile — файл политики в YAML.
type policyFile struct {
	Name     string         `yaml:"name"`
	Scenario string         `yaml:"scenario"`
	Targets  []string       `yaml:"targets"`
	Params   map[string]any `yaml:"params"`
	Expires  string         `yaml:"expires"`
}

// ParsePolicy разбирает файл политики. Пути сценария отсчитываются от
// каталога файла. Сервис, селекторы и сам сценарий проверяет вызывающий:
// пакету заявок о них знать незачем.
func ParsePolicy(path string, data []byte, now time.Time) (*Policy, error) {
	var f policyFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("политика %s: %w", path, err)
	}
	fail := func(format string, args ...any) (*Policy, error) {
		return nil, fmt.Errorf("политика %s: %s", path, fmt.Sprintf(format, args...))
	}
	if !policyName.MatchString(f.Name) {
		return fail("name — латинская буква, затем буквы, цифры, _ . -, до 64 символов")
	}
	if f.Scenario == "" {
		return fail("нет scenario — сценария, который watch строит для нового хоста")
	}
	if len(f.Targets) == 0 {
		return fail("нет targets: политика без меток целей покрыла бы любой хост")
	}
	if len(f.Params) == 0 {
		return fail("нет params: перечислите параметры сценария и их допустимые значения")
	}
	if f.Expires == "" {
		return fail("нет expires: политика без срока не отзывается сама, когда о ней забыли")
	}
	expires, err := parseExpiry(f.Expires)
	if err != nil {
		return fail("expires: %v", err)
	}
	if !expires.After(now) {
		return fail("срок %s уже прошёл", f.Expires)
	}

	scenario := f.Scenario
	if !filepath.IsAbs(scenario) {
		scenario = filepath.Join(filepath.Dir(path), scenario)
	}
	if abs, err := filepath.Abs(scenario); err == nil {
		scenario = abs
	}
	p := &Policy{Name: f.Name, Scenario: scenario, Targets: f.Targets, Expires: expires,
		Params: map[string]ParamRule{}}
	for name, spec := range f.Params {
		rule, err := parseRule(spec)
		if err != nil {
			return fail("params.%s: %v", name, err)
		}
		p.Params[name] = rule
	}
	sum := sha256.Sum256(data)
	p.Sum = hex.EncodeToString(sum[:])
	if abs, err := filepath.Abs(path); err == nil {
		p.File = abs
	}
	return p, nil
}

func parseExpiry(text string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", text, time.Local)
	if err != nil {
		return time.Time{}, errors.New("дата 2006-01-02 или время RFC 3339")
	}
	// Дата — включительно: политика действует весь этот день.
	return t.AddDate(0, 0, 1), nil
}

var rangeRule = regexp.MustCompile(`^\s*(-?[0-9.]*)\s*\.\.\s*(-?[0-9.]*)\s*$`)

func parseRule(spec any) (ParamRule, error) {
	switch v := spec.(type) {
	case nil:
		return ParamRule{}, errors.New("пусто: \"*\" — любое значение, список — допустимые, \"100..500\" — диапазон")
	case []any:
		var rule ParamRule
		for _, item := range v {
			rule.Values = append(rule.Values, fmt.Sprint(item))
		}
		if len(rule.Values) == 0 {
			return rule, errors.New("пустой список значений")
		}
		return rule, nil
	case string:
		if strings.TrimSpace(v) == "*" {
			return ParamRule{Any: true}, nil
		}
		if m := rangeRule.FindStringSubmatch(v); m != nil && (m[1] != "" || m[2] != "") {
			var rule ParamRule
			for i, bound := range []**float64{&rule.Min, &rule.Max} {
				if m[i+1] == "" {
					continue
				}
				n, err := strconv.ParseFloat(m[i+1], 64)
				if err != nil {
					return rule, fmt.Errorf("диапазон %q: %v", v, err)
				}
				*bound = &n
			}
			if rule.Min != nil && rule.Max != nil && *rule.Min > *rule.Max {
				return rule, fmt.Errorf("диапазон %q пуст", v)
			}
			return rule, nil
		}
		return ParamRule{Values: []string{v}}, nil
	}
	return ParamRule{Values: []string{fmt.Sprint(spec)}}, nil
}

// Policies — хранилище политик.
type Policies struct{ dir string }

// NewPolicies открывает хранилище политик.
func NewPolicies(dir string) (*Policies, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("не удалось создать каталог политик: %w", err)
	}
	return &Policies{dir: dir}, nil
}

func (s *Policies) path(name string) string { return filepath.Join(s.dir, name+".json") }

// Add регистрирует политику. Действующую с тем же именем не заменяет:
// сначала её отзывают — так смена политики видна в журнале. Прежняя,
// отозванная или истёкшая, уходит в архив (old/), а не стирается.
func (s *Policies) Add(p *Policy, now time.Time) error {
	if old, err := s.Get(p.Name); err == nil {
		if old.State(now) == nil {
			return fmt.Errorf("политика %s уже действует (%s): чтобы заменить, отзовите её — ochan policy revoke %s",
				p.Name, old.ExpiryText(), p.Name)
		}
		archive := filepath.Join(s.dir, "old")
		if err := os.MkdirAll(archive, dirPerm); err != nil {
			return err
		}
		name := fmt.Sprintf("%s-%s.json", old.Name, old.AddedAt.UTC().Format("20060102T150405"))
		if err := os.Rename(s.path(old.Name), filepath.Join(archive, name)); err != nil {
			return fmt.Errorf("прежняя политика %s не ушла в архив: %w", old.Name, err)
		}
	}
	p.AddedAt = now.UTC()
	if p.AddedBy == "" {
		p.AddedBy = currentUser()
	}
	return s.save(p)
}

func (s *Policies) save(p *Policy) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(p.Name), data, filePerm)
}

// Get читает политику по имени.
func (s *Policies) Get(name string) (*Policy, error) {
	if !policyName.MatchString(name) {
		return nil, fmt.Errorf("имя политики %q: латинская буква, затем буквы, цифры, _ . -", name)
	}
	data, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("политики %s нет: ochan policy add <файл>", name)
	}
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("политика %s испорчена: %w", name, err)
	}
	return &p, nil
}

// List — все политики, и отозванные тоже, по имени.
func (s *Policies) List() ([]*Policy, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Policy
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		p, err := s.Get(name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Revoke отзывает политику. Запись остаётся: видно, что политика была и
// кто её отозвал.
func (s *Policies) Revoke(name, who, reason string, now time.Time) (*Policy, error) {
	p, err := s.Get(name)
	if err != nil {
		return nil, err
	}
	if !p.RevokedAt.IsZero() {
		return nil, fmt.Errorf("политика %s уже отозвана %s", name, p.RevokedAt.Local().Format("2006-01-02 15:04"))
	}
	p.RevokedAt, p.RevokedBy, p.RevokeReason = now.UTC(), who, reason
	if p.RevokedBy == "" {
		p.RevokedBy = currentUser()
	}
	return p, s.save(p)
}
