package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State — то, что ochan помнит между запусками, кроме конфига, ролей и
// модулей (у них свои файлы): история команд оболочки.
//
// Выход из ochan не стирает сессию: после перезапуска стрелка вверх
// достаёт прежние команды, и !history показывает их.
type State struct {
	History []string `json:"history,omitempty"`
}

// StatePath — файл состояния.
func (fm *FileManager) StatePath() string { return filepath.Join(fm.baseDir, "state.json") }

// LoadState читает состояние; файла нет — пустое состояние.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("не удалось прочитать состояние %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("повреждён файл состояния %s: %w", path, err)
	}
	return &s, nil
}

// Save записывает состояние целиком: во временный файл и переименованием,
// чтобы оборванная запись не оставила половину файла.
func (s *State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("не удалось записать состояние: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("не удалось записать состояние: %w", err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// AppendHistory дописывает команду в историю на диске и оставляет
// последние limit команд. Файл перечитывается перед записью: две
// оболочки, открытые одновременно, не затирают команды друг друга.
// Повтор предыдущей команды не записывается.
func AppendHistory(path, command string, limit int) error {
	s, err := LoadState(path)
	if err != nil {
		return err
	}
	if n := len(s.History); n > 0 && s.History[n-1] == command {
		return nil
	}
	s.History = append(s.History, command)
	if limit > 0 && len(s.History) > limit {
		s.History = append([]string(nil), s.History[len(s.History)-limit:]...)
	}
	return s.Save(path)
}
