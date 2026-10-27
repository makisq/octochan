package rlm

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

// EnvTokenVar — прежняя переменная окружения с токеном RLM API. Процесс
// модуля наследует окружение ochan, поэтому она по-прежнему работает;
// новая — OCTOCHAN_TOKEN_RLM, её читает ядро.
const EnvTokenVar = "OCTOCHAN_API_TOKEN"

// Токен намеренно живёт вне viper.
//
// Раньше loadEnv() делал viper.Set("defaults.api_token", ...), а команда timer
// вызывала viper.WriteConfig() — и токен, аккуратно сохранённый в .env с
// правами 0600, оказывался в открытом config.yaml с правами 0644.
var (
	tokenMu  sync.RWMutex
	apiToken string
)

// SetAPIToken запоминает токен на время работы процесса.
func SetAPIToken(token string) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	apiToken = strings.TrimSpace(token)
}

// APIToken возвращает токен: сначала из памяти, затем из окружения.
func APIToken() string {
	tokenMu.RLock()
	token := apiToken
	tokenMu.RUnlock()

	if token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv(EnvTokenVar))
}

// LoadTokenFrom читает токен из файла вида .env и запоминает его. Так
// модуль RLM подхватывает ~/.octochan/.env, который до 0.2.0 писал ochan
// auth; новый токен ядро передаёт в вызове (ochan auth --executor rlm).
// Отсутствие файла ошибкой не считается.
func LoadTokenFrom(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		// Ключ API_TOKEN поддерживается ради файлов, созданных прежней версией.
		switch strings.TrimSpace(key) {
		case EnvTokenVar, "API_TOKEN":
			// TrimSpace снимает в том числе \r из файлов, побывавших в Windows.
			SetAPIToken(strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}

	return scanner.Err()
}
