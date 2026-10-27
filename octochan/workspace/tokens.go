package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Токены исполнителей: файл на исполнителя в ~/.octochan/tokens с правами
// 0600. До 0.2.0 токен был один — RLM, в ~/.octochan/.env; теперь у
// каждого исполнителя свой. В config.yaml токены не попадают никогда.

// EnvExecutorToken — переменная окружения с токеном исполнителя вместо
// файла: OCTOCHAN_TOKEN_SSH, OCTOCHAN_TOKEN_MY_API.
func EnvExecutorToken(executor string) string {
	return "OCTOCHAN_TOKEN_" + strings.ToUpper(strings.ReplaceAll(executor, "-", "_"))
}

func (fm *FileManager) tokensDir() string { return filepath.Join(fm.baseDir, "tokens") }

// ExecutorTokenPath — файл токена исполнителя.
func (fm *FileManager) ExecutorTokenPath(executor string) (string, error) {
	if executor == "" || strings.IndexFunc(executor, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	}) >= 0 {
		return "", fmt.Errorf("имя исполнителя %q: допустимы буквы, цифры, «-» и «_»", executor)
	}
	return filepath.Join(fm.tokensDir(), executor), nil
}

// LatestToken — исполнитель, чей токен сохранили последним, и когда; false
// — токенов не сохраняли. По нему оболочка напоминает о смене токена.
func (fm *FileManager) LatestToken() (executor string, saved time.Time, ok bool) {
	entries, err := os.ReadDir(fm.tokensDir())
	if err != nil {
		return "", time.Time{}, false
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() || info.Size() == 0 {
			continue
		}
		if !ok || info.ModTime().After(saved) {
			executor, saved, ok = e.Name(), info.ModTime(), true
		}
	}
	return executor, saved, ok
}

// SaveExecutorToken сохраняет токен исполнителя и возвращает путь файла.
func (fm *FileManager) SaveExecutorToken(executor, token string) (string, error) {
	path, err := fm.ExecutorTokenPath(executor)
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
	path, err := fm.ExecutorTokenPath(executor)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
