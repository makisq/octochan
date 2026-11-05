package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ochan/embedded"
	"ochan/own"

	"github.com/spf13/viper"
)

// Где модель.
//
//	llm:
//	  mode: embedded
//	  model: ~/models/own       # своя модель ℒ: ochan загружает её в свой процесс
//	  endpoint: http://127.0.0.1:8093   # модель на другой машине (ochan llm serve там)
//
// Своя модель исполняется в процессе ochan: словарь, сеть, логика и кольца
// — пакеты own, fp и rings, одна реализация, которой ochan судит и любую
// другую модель. llm.model — каталог её весов; без него — движок по адресу
// llm.endpoint по протоколу v1: своя модель другой машины (ochan llm serve)
// или чужой движок, которого ведёт декодер ochan.

// llmBackend — модель, с которой говорят команды: в процессе или по адресу.
type llmBackend interface {
	Info(ctx context.Context) (embedded.Info, error)
	embedded.Engine
	embedded.Scorer
	embedded.Generator
	// Where — где модель: «процесс ochan» или адрес.
	Where() string
}

type httpBackend struct{ *embedded.HTTP }

func (h httpBackend) Where() string { return h.Endpoint }

// localBackend — своя модель в процессе ochan.
type localBackend struct{ *own.Model }

func (l localBackend) Info(context.Context) (embedded.Info, error) { return l.Model.Info(), nil }
func (l localBackend) Where() string                               { return "процесс ochan" }

var (
	loadedMu sync.Mutex
	loaded   = map[string]*own.Model{}
)

// llmModelDir — каталог своей модели из llm.model; пусто — модель по адресу.
func llmModelDir() string { return expandHome(strings.TrimSpace(viper.GetString("llm.model"))) }

// loadOwnModel — своя модель каталога dir; в одном процессе — один раз.
func loadOwnModel(dir string) (*own.Model, error) {
	loadedMu.Lock()
	defer loadedMu.Unlock()
	if m, ok := loaded[dir]; ok {
		return m, nil
	}
	if !own.IsModel(dir) {
		return nil, fmt.Errorf("llm.model: %s — не своя модель ℒ (нет %s): каталог её весов и словаря "+
			"(ochan llm init, затем обучение); модель другой машины — llm.endpoint без llm.model", dir, own.TokenizerFile)
	}
	m, err := own.Load(dir, modulesDir())
	if err != nil {
		return nil, fmt.Errorf("llm.model: %w", err)
	}
	loaded[dir] = m
	return m, nil
}

// llmEngine — модель для команд: своя в процессе (llm.model) или по адресу
// (llm.endpoint).
func llmEngine() (llmBackend, error) {
	if dir := llmModelDir(); dir != "" {
		m, err := loadOwnModel(dir)
		if err != nil {
			return nil, err
		}
		return localBackend{m}, nil
	}
	endpoint := strings.TrimSpace(viper.GetString("llm.endpoint"))
	if endpoint == "" {
		return nil, embedded.ErrNoEngine
	}
	return httpBackend{embedded.NewHTTP(endpoint, viper.GetDuration("llm.timeout"))}, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~"); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}
