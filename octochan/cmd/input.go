package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"ochan/config"
)

// Прежняя схема конвейеров была двухслойной и нерабочей:
// main перехватывал "|" в аргументах и перезапускал сам себя подпроцессом,
// а PipeWrapper вычитывал stdin и подставлял команде аргумент "-", после чего
// команда читала уже пустой stdin. Теперь используется обычный конвейер
// оболочки, а stdin читается один раз и кешируется.
var (
	stdinOnce  sync.Once
	stdinBytes []byte
	stdinErr   error
)

// readStdin читает stdin целиком; повторные вызовы отдают тот же буфер.
func readStdin() ([]byte, error) {
	stdinOnce.Do(func() {
		stdinBytes, stdinErr = io.ReadAll(os.Stdin)
		if stdinErr != nil {
			stdinErr = fmt.Errorf("ошибка чтения stdin: %w", stdinErr)
		}
	})
	return stdinBytes, stdinErr
}

// stdinIsPipe сообщает, что на stdin подан не терминал.
func stdinIsPipe() bool {
	// Ошибка Stat раньше игнорировалась, а затем stat.Mode() разыменовывал nil.
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

// source описывает прочитанный источник данных.
type source struct {
	Name    string
	Content string
}

// readSource читает файл или stdin (если path == "-").
func readSource(path string) (source, error) {
	if path == "-" {
		data, err := readStdin()
		if err != nil {
			return source{}, err
		}
		return source{Name: "stdin", Content: string(data)}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return source{}, fmt.Errorf("не удалось прочитать %s: %w", path, err)
	}
	return source{Name: path, Content: string(data)}, nil
}

// loadConfigArg загружает конфиг из аргумента команды: это может быть
// путь к файлу, "-" для stdin или ссылка на живой источник вида
// postgres://user@host:5432/db.
func loadConfigArg(ctx context.Context, ref string, opts config.SourceOptions) (*config.LoadedConfig, error) {
	if ref == "-" {
		data, err := readStdin()
		if err != nil {
			return nil, err
		}
		return config.LoadConfigFromText("stdin", string(data))
	}
	return config.LoadConfigFrom(ctx, ref, opts)
}

// argOrStdin возвращает аргумент по индексу, а при его отсутствии — "-",
// если на stdin что-то подано. Так `cat a.conf | ochan validate` работает
// без явного дефиса.
func argOrStdin(args []string, index int) (string, error) {
	if index < len(args) {
		return args[index], nil
	}
	if stdinIsPipe() {
		return "-", nil
	}
	return "", fmt.Errorf("не указан файл (аргумент #%d) и на stdin ничего не подано", index+1)
}
