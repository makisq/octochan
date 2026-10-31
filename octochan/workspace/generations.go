package workspace

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Generation — запись журнала генераций встроенной модели: что кольцо 0 и
// маска языка сделали с ответом. По этим записям fp journal пишет, сколько
// отклонено на кольце 0: в режиме протокола такой строки нет, и по отчёту
// видно, что гарантии другие.
type Generation struct {
	Time   time.Time `json:"time"`
	Role   string    `json:"role,omitempty"`
	Engine string    `json:"engine,omitempty"`
	Model  string    `json:"model,omitempty"`
	Tau    float64   `json:"tau"`
	// OK — ответ получен; иначе Reason — почему его нет.
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`

	Statements    int `json:"statements"`
	Tokens        int `json:"tokens"`
	Calls         int `json:"calls"`
	Ring0         int `json:"ring0"`
	Masked        int `json:"masked"`
	OutOfLanguage int `json:"out_of_language"`
	Dead          int `json:"dead"`
	Rollbacks     int `json:"rollbacks"`
	EarlyEnd      int `json:"early_end,omitempty"`
}

// Generations — журнал генераций: строка JSON на генерацию.
type Generations struct{ path string }

// NewGenerations открывает журнал в файле path.
func NewGenerations(path string) *Generations { return &Generations{path: path} }

// Append дописывает запись.
func (g *Generations) Append(rec Generation) error {
	if err := os.MkdirAll(filepath.Dir(g.path), dirPerm); err != nil {
		return err
	}
	unlock, err := lockPath(g.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(g.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// All — все записи по порядку.
func (g *Generations) All() ([]Generation, error) {
	f, err := os.Open(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Generation
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; scanner.Scan(); n++ {
		var rec Generation
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("журнал генераций %s, строка %d: %w", g.path, n, err)
		}
		out = append(out, rec)
	}
	return out, scanner.Err()
}
