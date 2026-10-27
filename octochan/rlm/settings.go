package rlm

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Settings — настройки клиента RLM.
//
// Своего конфига у модуля RLM нет: ядро передаёт настройки в каждом вызове
// исполнителя (executors.rlm в config.yaml ochan поверх defaults), и модуль
// кладёт их сюда. Раньше пакет читал viper ядра и потому мог жить только
// внутри него.
type Settings struct {
	// APIURL — адрес создания задач, …/tasks.json.
	APIURL string
	// HTTPTimeout — предел одного запроса; 0 — 30 секунд.
	HTTPTimeout time.Duration
	// MaxParallel — одновременных запросов при отправке задач; 0 — 5.
	MaxParallel int
}

var (
	settingsMu sync.RWMutex
	settings   Settings
)

// Configure задаёт настройки клиента.
func Configure(s Settings) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	settings = s
}

// ConfigureFrom разбирает настройки из словаря, который ядро передаёт
// исполнителю: api_url, http_timeout, max_parallel_tasks. Нераспознанное
// значение — ноль, и действует умолчание.
func ConfigureFrom(values map[string]string) {
	s := Settings{APIURL: strings.TrimSpace(values["api_url"])}
	if d, err := time.ParseDuration(strings.TrimSpace(values["http_timeout"])); err == nil {
		s.HTTPTimeout = d
	}
	if n, err := strconv.Atoi(strings.TrimSpace(values["max_parallel_tasks"])); err == nil {
		s.MaxParallel = n
	}
	Configure(s)
}

func currentSettings() Settings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return settings
}
