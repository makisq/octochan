// Package inventory — цели: хосты, их группы и переменные, откуда бы они
// ни пришли.
//
// Цель сценария больше не обязана быть svm_ci, ci_list или ip: это
// свободный набор полей, а хосты с их полями приносят источники
// инвентаря. Источник выбирается схемой адреса — как источник конфигов
// (postgres://…): встроенный file — YAML-файл, остальные регистрируют
// модули и сам ochan (Register). Селектор (group=db_new, host=db*) отбирает
// хосты и цели — так работает --limit.
//
// Обнаружение новых серверов — сравнение с тем, что уже видели (Seen): хост,
// которого не было, — новый, пока его не отметили.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Host — хост инвентаря.
type Host struct {
	Name   string   `json:"name"`
	Groups []string `json:"groups,omitempty"`
	// Vars — поля и переменные хоста: ip, svm_ci, max_connections… Поверх
	// переменных его групп, в порядке групп.
	Vars map[string]any `json:"vars,omitempty"`
	// Source — имя источника, из которого пришёл хост.
	Source string `json:"source,omitempty"`
}

// Labels — то, по чему селектор отбирает хост или цель.
type Labels struct {
	Fields map[string]string
	Groups []string
}

// Labels — поля хоста текстом и его группы; имя — поле host.
func (h Host) Labels() Labels {
	fields := map[string]string{"host": h.Name}
	for key, value := range h.Vars {
		fields[key] = Text(value)
	}
	return Labels{Fields: fields, Groups: h.Groups}
}

// Text — значение поля текстом: списки через запятую.
func Text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = Text(item)
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(x)
	}
}

// Source — источник инвентаря.
type Source interface {
	Hosts(ctx context.Context) ([]Host, error)
}

// Opener открывает источник по адресу.
type Opener func(ref string) (Source, error)

var (
	schemesMu sync.RWMutex
	schemes   = map[string]Opener{"file": openFile}
)

// Register регистрирует схему источника инвентаря: cmdb, agents… Адрес
// источника — «схема://…»; путь без схемы — файл.
func Register(scheme string, open Opener) {
	schemesMu.Lock()
	defer schemesMu.Unlock()
	schemes[scheme] = open
}

// Schemes — зарегистрированные схемы.
func Schemes() []string {
	schemesMu.RLock()
	defer schemesMu.RUnlock()
	names := make([]string, 0, len(schemes))
	for name := range schemes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Open открывает источник по адресу.
func Open(ref string) (Source, error) {
	scheme, rest, found := strings.Cut(ref, "://")
	if !found {
		scheme, rest = "file", ref
	}
	schemesMu.RLock()
	open, ok := schemes[scheme]
	schemesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("источник инвентаря %q: схемы %s нет; есть: %s", ref, scheme, strings.Join(Schemes(), ", "))
	}
	if scheme == "file" {
		return open(rest)
	}
	return open(ref)
}

// Load читает источники (имя → адрес) и возвращает хосты по имени. Хост с
// одним именем в двух источниках — ошибка: какой из них цель, решать
// некому.
func Load(ctx context.Context, sources map[string]string) ([]Host, error) {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)

	byName := map[string]Host{}
	for _, name := range names {
		src, err := Open(sources[name])
		if err != nil {
			return nil, fmt.Errorf("инвентарь %s: %w", name, err)
		}
		hosts, err := src.Hosts(ctx)
		if err != nil {
			return nil, fmt.Errorf("инвентарь %s: %w", name, err)
		}
		for _, h := range hosts {
			h.Source = name
			if prev, dup := byName[h.Name]; dup {
				return nil, fmt.Errorf("хост %s есть и в инвентаре %s, и в %s", h.Name, prev.Source, name)
			}
			byName[h.Name] = h
		}
	}
	hosts := make([]Host, 0, len(byName))
	for _, h := range byName {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	return hosts, nil
}

// ---------------------------------------------------------------------------
// Файл
// ---------------------------------------------------------------------------

// fileSource — инвентарь в YAML:
//
//	groups:
//	  db: {max_connections: 200}             # переменные группы
//	hosts:
//	  db1: {groups: [db], ip: 10.0.0.1, svm_ci: CI001}
//	  db2: {groups: [db, db_new], max_connections: 300}
type fileSource struct{ path string }

func openFile(p string) (Source, error) {
	if p == "" {
		return nil, errors.New("не указан путь к файлу инвентаря")
	}
	return fileSource{path: p}, nil
}

type fileFormat struct {
	Groups map[string]map[string]any `yaml:"groups"`
	Hosts  map[string]map[string]any `yaml:"hosts"`
}

func (f fileSource) Hosts(context.Context) ([]Host, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	return ParseFile(f.path, data)
}

// ParseFile разбирает инвентарь в YAML.
func ParseFile(name string, data []byte) ([]Host, error) {
	var file fileFormat
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	var hosts []Host
	for hostName, raw := range file.Hosts {
		if hostName == "" || strings.ContainsAny(hostName, " ,=") {
			return nil, fmt.Errorf("%s: имя хоста %q: без пробелов, запятых и «=»", name, hostName)
		}
		h := Host{Name: hostName, Vars: map[string]any{}}
		groups, err := groupList(raw["groups"], raw["group"])
		if err != nil {
			return nil, fmt.Errorf("%s: хост %s: %w", name, hostName, err)
		}
		h.Groups = groups
		// Группа без раздела в groups — просто метка без переменных.
		for _, g := range groups {
			for key, value := range file.Groups[g] {
				h.Vars[key] = value
			}
		}
		for key, value := range raw {
			if key == "groups" || key == "group" {
				continue
			}
			if key == "host" {
				return nil, fmt.Errorf("%s: хост %s: поле host — имя хоста, его задаёт ключ", name, hostName)
			}
			h.Vars[key] = value
		}
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	return hosts, nil
}

func groupList(values ...any) ([]string, error) {
	var groups []string
	for _, v := range values {
		switch x := v.(type) {
		case nil:
		case string:
			groups = append(groups, x)
		case []any:
			for _, item := range x {
				s, ok := item.(string)
				if !ok || s == "" {
					return nil, fmt.Errorf("группа %v — не имя", item)
				}
				groups = append(groups, s)
			}
		default:
			return nil, fmt.Errorf("groups — имя или список имён, получено %T", v)
		}
	}
	return groups, nil
}

// ---------------------------------------------------------------------------
// Селектор
// ---------------------------------------------------------------------------

// Selector — условия через запятую, все сразу: group=db_new,host=db*.
// Значение — шаблон (*, ?, [...]); group — членство в группе, остальное —
// поле. Слово без «=» — имя хоста: db1, db*.
type Selector struct {
	text  string
	terms []term
}

type term struct{ key, pattern string }

// ParseSelector разбирает селектор.
func ParseSelector(text string) (Selector, error) {
	sel := Selector{text: strings.TrimSpace(text)}
	if sel.text == "" {
		return sel, errors.New("пустой селектор: ожидалось поле=значение, например group=db")
	}
	for _, part := range strings.Split(sel.text, ",") {
		part = strings.TrimSpace(part)
		key, pattern, found := strings.Cut(part, "=")
		if !found {
			key, pattern = "host", part
		}
		key, pattern = strings.TrimSpace(key), strings.TrimSpace(pattern)
		if key == "" || pattern == "" {
			return sel, fmt.Errorf("селектор %q: условие %q — поле=значение", text, part)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return sel, fmt.Errorf("селектор %q: шаблон %q: %v", text, pattern, err)
		}
		sel.terms = append(sel.terms, term{key: key, pattern: pattern})
	}
	return sel, nil
}

// ParseSelectors разбирает несколько селекторов: подходит то, что подходит
// хотя бы под один.
func ParseSelectors(texts []string) ([]Selector, error) {
	var out []Selector
	for _, text := range texts {
		sel, err := ParseSelector(text)
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	return out, nil
}

func (s Selector) String() string { return s.text }

// Match — все условия селектора выполнены.
func (s Selector) Match(l Labels) bool {
	for _, t := range s.terms {
		if t.key == "group" || t.key == "groups" {
			if !anyMatch(t.pattern, l.Groups) {
				return false
			}
			continue
		}
		value, ok := l.Fields[t.key]
		if !ok {
			return false
		}
		if matched, _ := path.Match(t.pattern, value); !matched {
			return false
		}
	}
	return true
}

func anyMatch(pattern string, values []string) bool {
	for _, v := range values {
		if matched, _ := path.Match(pattern, v); matched {
			return true
		}
	}
	return false
}

// MatchAny — подходит хотя бы под один селектор; пустой список — под любой.
func MatchAny(sels []Selector, l Labels) bool {
	if len(sels) == 0 {
		return true
	}
	for _, s := range sels {
		if s.Match(l) {
			return true
		}
	}
	return false
}

// Filter — хосты, подходящие хотя бы под один селектор.
func Filter(hosts []Host, sels []Selector) []Host {
	if len(sels) == 0 {
		return hosts
	}
	var out []Host
	for _, h := range hosts {
		if MatchAny(sels, h.Labels()) {
			out = append(out, h)
		}
	}
	return out
}

// Describe — селекторы одной строкой.
func Describe(sels []Selector) string {
	parts := make([]string, len(sels))
	for i, s := range sels {
		parts[i] = s.text
	}
	return strings.Join(parts, " | ")
}
