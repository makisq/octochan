package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Токены исполнителей: файл на исполнителя в ~/.octochan/tokens с правами
// 0600 — как .env с токеном RLM, только у каждого исполнителя свой.
// В config.yaml токены не попадают никогда.

// EnvExecutorToken — переменная окружения с токеном исполнителя вместо
// файла: OCTOCHAN_TOKEN_SSH, OCTOCHAN_TOKEN_MY_API.
func EnvExecutorToken(executor string) string {
	return "OCTOCHAN_TOKEN_" + strings.ToUpper(strings.ReplaceAll(executor, "-", "_"))
}

func (fm *FileManager) executorTokenPath(executor string) (string, error) {
	if executor == "" || strings.IndexFunc(executor, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	}) >= 0 {
		return "", fmt.Errorf("имя исполнителя %q: допустимы буквы, цифры, «-» и «_»", executor)
	}
	return filepath.Join(fm.baseDir, "tokens", executor), nil
}

// SaveExecutorToken сохраняет токен исполнителя и возвращает путь файла.
func (fm *FileManager) SaveExecutorToken(executor, token string) (string, error) {
	path, err := fm.executorTokenPath(executor)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("пустой токен")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return "", fmt.Errorf("не удалось создать каталог токенов: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), filePerm); err != nil {
		return "", fmt.Errorf("не удалось сохранить токен: %w", err)
	}
	return path, nil
}

// ExecutorToken — токен исполнителя: из окружения, иначе из файла; пусто —
// токена нет.
func (fm *FileManager) ExecutorToken(executor string) string {
	if token := strings.TrimSpace(os.Getenv(EnvExecutorToken(executor))); token != "" {
		return token
	}
	path, err := fm.executorTokenPath(executor)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
