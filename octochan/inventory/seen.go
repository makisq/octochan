package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Seen — хосты, которые уже видели: по ним новый сервер отличается от
// известного. Хранится в рабочем каталоге ochan.
type Seen struct {
	path string
	// First — когда хост увидели впервые: «источник/имя» → время.
	First map[string]time.Time `json:"first"`
}

// LoadSeen читает, что уже видели; файла нет — не видели ничего.
func LoadSeen(path string) (*Seen, error) {
	s := &Seen{path: path, First: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("%s повреждён: %w", path, err)
	}
	if s.First == nil {
		s.First = map[string]time.Time{}
	}
	return s, nil
}

func seenKey(h Host) string { return h.Source + "/" + h.Name }

// New — хосты, которых ещё не видели.
func (s *Seen) New(hosts []Host) []Host {
	var out []Host
	for _, h := range hosts {
		if _, ok := s.First[seenKey(h)]; !ok {
			out = append(out, h)
		}
	}
	return out
}

// IsNew — хоста ещё не видели.
func (s *Seen) IsNew(h Host) bool {
	_, ok := s.First[seenKey(h)]
	return !ok
}

// Mark отмечает хосты увиденными и сохраняет.
func (s *Seen) Mark(hosts []Host, at time.Time) error {
	for _, h := range hosts {
		if _, ok := s.First[seenKey(h)]; !ok {
			s.First[seenKey(h)] = at.UTC()
		}
	}
	return s.save()
}

func (s *Seen) save() error {
	keys := make([]string, 0, len(s.First))
	for k := range s.First {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
