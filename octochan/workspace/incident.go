package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Инциденты контура HA-AI.
//
// Инцидент открывает мониторинг или сама модель, и модель идёт чинить. У
// инцидента уровень критичности, а у уровня — срок: сколько модель чинит
// сама. Не успела или сама признала, что не справится, — инцидент
// эскалируется инженерам: письмо с описанием, всем, что модель сделала, и
// журналами. После эскалации модель по инциденту больше не действует
// сама: срок нужен как раз для того, чтобы она не ушла в цикл.

// IncidentState — состояние инцидента.
type IncidentState string

const (
	IncidentOpen      IncidentState = "open"
	IncidentResolved  IncidentState = "resolved"
	IncidentEscalated IncidentState = "escalated"
)

// Russian — состояние по-русски.
func (s IncidentState) Russian() string {
	switch s {
	case IncidentOpen:
		return "открыт"
	case IncidentResolved:
		return "решён"
	case IncidentEscalated:
		return "эскалирован"
	}
	return string(s)
}

// IncidentStep — что сделано по инциденту: кем, когда и что.
type IncidentStep struct {
	Time time.Time `json:"time"`
	Who  string    `json:"who"`
	// Kind — open, note, request, apply, attach, resolve, escalate.
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Ref — заявка или файл, к которому относится шаг.
	Ref string `json:"ref,omitempty"`
}

// IncidentLog — приложенный журнал.
type IncidentLog struct {
	Name    string    `json:"name"`
	Added   time.Time `json:"added"`
	Content []byte    `json:"content"`
}

// Пределы приложенных журналов: в письмо инженерам — не архив диска.
const (
	MaxIncidentLog  = 256 << 10
	MaxIncidentLogs = 1 << 20
)

// Incident — инцидент.
type Incident struct {
	ID    string `json:"id"`
	Level string `json:"level"`
	// Timer — срок уровня текстом; Deadline — до какого времени модель
	// чинит сама.
	Timer    string         `json:"timer"`
	Opened   time.Time      `json:"opened"`
	Deadline time.Time      `json:"deadline"`
	Source   string         `json:"source"`
	Hosts    []string       `json:"hosts,omitempty"`
	Summary  string         `json:"summary"`
	State    IncidentState  `json:"state"`
	Steps    []IncidentStep `json:"steps"`
	Logs     []IncidentLog  `json:"logs,omitempty"`
	// Closed и Outcome — когда и чем кончилось: решение или причина
	// эскалации.
	Closed  time.Time `json:"closed,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	// Notified — кому ушло письмо об эскалации; пусто и NotifyError —
	// не ушло и почему.
	Notified    []string `json:"notified,omitempty"`
	NotifyError string   `json:"notify_error,omitempty"`
}

// Overdue — открыт, а срок вышел.
func (i *Incident) Overdue(now time.Time) bool {
	return i.State == IncidentOpen && !now.Before(i.Deadline)
}

// Step дописывает шаг.
func (i *Incident) Step(who, kind, text, ref string) {
	i.Steps = append(i.Steps, IncidentStep{Time: time.Now().UTC(), Who: who, Kind: kind, Text: text, Ref: ref})
}

// Incidents — хранилище инцидентов: файл на инцидент.
type Incidents struct{ dir string }

// NewIncidents открывает хранилище.
func NewIncidents(dir string) (*Incidents, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}
	return &Incidents{dir: dir}, nil
}

func (s *Incidents) path(id string) string { return filepath.Join(s.dir, id+".json") }

// Create записывает новый инцидент.
func (s *Incidents) Create(i *Incident) error {
	id, err := generateID()
	if err != nil {
		return err
	}
	i.ID, i.State = id, IncidentOpen
	if i.Opened.IsZero() {
		i.Opened = time.Now().UTC()
	}
	return writeJSONAtomic(s.path(i.ID), i)
}

func (s *Incidents) read(id string) (*Incident, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var i Incident
	if err := json.Unmarshal(data, &i); err != nil {
		return nil, fmt.Errorf("повреждён инцидент %s: %w", id, err)
	}
	return &i, nil
}

// List — инциденты от новых к старым.
func (s *Incidents) List() ([]*Incident, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Incident
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		i, err := s.read(id)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Opened.After(out[b].Opened) })
	return out, nil
}

// Get — инцидент по номеру или его началу.
func (s *Incidents) Get(id string) (*Incident, error) {
	if id == "" {
		return nil, errors.New("не указан номер инцидента")
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var matches []*Incident
	for _, i := range all {
		if i.ID == id {
			return i, nil
		}
		if strings.HasPrefix(i.ID, id) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("инцидент %q не найден", id)
	case 1:
		return matches[0], nil
	}
	return nil, fmt.Errorf("номер %q подходит к нескольким инцидентам — укажите длиннее", id)
}

// Update меняет инцидент под замком: два процесса не эскалируют его оба.
func (s *Incidents) Update(id string, change func(*Incident) error) (*Incident, error) {
	unlock, err := lockPath(s.path(id) + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	i, err := s.read(id)
	if err != nil {
		return nil, err
	}
	if err := change(i); err != nil {
		return nil, err
	}
	return i, writeJSONAtomic(s.path(id), i)
}

// Attach прикладывает журнал в пределах размера.
func (i *Incident) Attach(name string, content []byte) error {
	if len(content) > MaxIncidentLog {
		return fmt.Errorf("журнал %s больше %d КиБ: приложите нужный кусок (ochan logs rank … --min warning)", name, MaxIncidentLog>>10)
	}
	total := len(content)
	for _, l := range i.Logs {
		total += len(l.Content)
	}
	if total > MaxIncidentLogs {
		return fmt.Errorf("журналов у инцидента больше %d МиБ: письмо инженерам — не архив", MaxIncidentLogs>>20)
	}
	i.Logs = append(i.Logs, IncidentLog{Name: name, Added: time.Now().UTC(), Content: content})
	return nil
}
