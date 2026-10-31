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

// Почта ochan: письма агентов друг другу и людям.
//
// Агенту письмо ложится в ящик — его читает модель агента. Людям оно
// уходит по почте. Хранится одно письмо одним файлом: кому ушло, кому
// легло и не было ли ошибки — в нём же. Каталог копируется на standby.

// Message — письмо.
type Message struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// From — кто написал: роль и, если пишет модель агента, агент.
	From      string `json:"from"`
	FromAgent string `json:"from_agent,omitempty"`
	// To — адресаты, как их назвали: агенты, группы, адреса.
	To []string `json:"to"`
	// Agents — чьи ящики получили письмо; Emails — адреса, куда оно ушло.
	Agents []string `json:"agents,omitempty"`
	Emails []string `json:"emails,omitempty"`
	// Lang — formal (формальный промпт) или natural.
	Lang    string `json:"lang"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
	// Error — письмо людям не ушло: почему.
	Error string `json:"error,omitempty"`
}

// Mailbox — хранилище писем.
type Mailbox struct{ dir string }

// NewMailbox открывает хранилище.
func NewMailbox(dir string) (*Mailbox, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}
	return &Mailbox{dir: dir}, nil
}

func (m *Mailbox) path(id string) string { return filepath.Join(m.dir, id+".json") }

// Create записывает новое письмо.
func (m *Mailbox) Create(msg *Message) error {
	id, err := generateID()
	if err != nil {
		return err
	}
	msg.ID = id
	if msg.Time.IsZero() {
		msg.Time = time.Now().UTC()
	}
	return writeJSONAtomic(m.path(id), msg)
}

// Save перезаписывает письмо: отметка, ушло ли оно людям.
func (m *Mailbox) Save(msg *Message) error { return writeJSONAtomic(m.path(msg.ID), msg) }

// List — письма от новых к старым.
func (m *Mailbox) List() ([]*Message, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	var out []*Message
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		data, err := os.ReadFile(m.path(id))
		if err != nil {
			return nil, err
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("повреждено письмо %s: %w", id, err)
		}
		out = append(out, &msg)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Time.After(out[b].Time) })
	return out, nil
}

// Get — письмо по номеру или его началу.
func (m *Mailbox) Get(id string) (*Message, error) {
	if id == "" {
		return nil, errors.New("не указан номер письма")
	}
	all, err := m.List()
	if err != nil {
		return nil, err
	}
	var matches []*Message
	for _, msg := range all {
		if msg.ID == id {
			return msg, nil
		}
		if strings.HasPrefix(msg.ID, id) {
			matches = append(matches, msg)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("письма %q нет", id)
	case 1:
		return matches[0], nil
	}
	return nil, fmt.Errorf("номер %q подходит к нескольким письмам — укажите длиннее", id)
}

// MessagesFor — письма в ящике агента.
func MessagesFor(all []*Message, agent string) []*Message {
	var out []*Message
	for _, msg := range all {
		for _, a := range msg.Agents {
			if a == agent {
				out = append(out, msg)
				break
			}
		}
	}
	return out
}
