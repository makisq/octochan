package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Окна триггеров.
//
// Триггер — событие внешней системы (CI начал релиз), которое на время
// открывает роль: роль, названная в триггере, действует только в окне
// события и только над целями его области. Окно закрывается событием
// конца или само, по max_window: завис релиз — роль гаснет, и для этого не
// нужен ни человек, ни CI.

// TriggerWindow — окно, открытое событием.
type TriggerWindow struct {
	// Trigger — имя триггера в config.yaml; Event — номер события у его
	// источника (номер сборки CI).
	Trigger string `json:"trigger"`
	Event   string `json:"event"`
	Role    string `json:"role"`
	// Scope — селекторы инвентаря: цели роли в окне — только под ними.
	Scope []string `json:"scope,omitempty"`
	// Window — max_window триггера текстом, как его записали.
	Window string    `json:"window"`
	Start  time.Time `json:"start"`
	Until  time.Time `json:"until"`
	// By — кто открыл окно: роль и пользователь, адрес вебхука.
	By string `json:"by"`
	// Ended — когда окно закрылось; Outcome — почему. Пусто — открыто или
	// истекло, но ещё не закрыто (Settle).
	Ended   time.Time `json:"ended,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
}

// ID — release#1234.
func (w *TriggerWindow) ID() string { return w.Trigger + "#" + w.Event }

// Open — окно действует в момент now.
func (w *TriggerWindow) Open(now time.Time) bool {
	return w.Ended.IsZero() && now.Before(w.Until)
}

// keepWindows — сколько окон помнить: история, а не архив навсегда.
const keepWindows = 500

// Triggers — окна триггеров в одном файле.
type Triggers struct{ path string }

// NewTriggers открывает хранилище окон.
func NewTriggers(path string) *Triggers { return &Triggers{path: path} }

// Path — файл окон.
func (t *Triggers) Path() string { return t.path }

// Windows — все окна от старых к новым.
func (t *Triggers) Windows() ([]TriggerWindow, error) {
	data, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var windows []TriggerWindow
	if err := json.Unmarshal(data, &windows); err != nil {
		return nil, fmt.Errorf("повреждён файл окон триггеров %s: %w", t.path, err)
	}
	return windows, nil
}

// update меняет окна под замком файла.
func (t *Triggers) update(change func([]TriggerWindow) ([]TriggerWindow, error)) error {
	unlock, err := lockPath(t.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	windows, err := t.Windows()
	if err != nil {
		return err
	}
	windows, err = change(windows)
	if err != nil {
		return err
	}
	if len(windows) > keepWindows {
		windows = windows[len(windows)-keepWindows:]
	}
	return writeJSONAtomic(t.path, windows)
}

// Fire открывает окно. То же событие второй раз окна не продлевает: CI,
// повторивший событие, не должен растягивать срок роли.
func (t *Triggers) Fire(w TriggerWindow) error {
	return t.update(func(windows []TriggerWindow) ([]TriggerWindow, error) {
		for _, old := range windows {
			if old.ID() == w.ID() {
				state := "уже закрыто: " + old.Outcome
				if old.Open(w.Start) {
					state = "уже открыто до " + old.Until.Local().Format("15:04")
				} else if old.Ended.IsZero() {
					state = "уже истекло"
				}
				return nil, fmt.Errorf("окно %s %s — то же событие второй раз окна не открывает", w.ID(), state)
			}
		}
		return append(windows, w), nil
	})
}

// End закрывает открытое окно.
func (t *Triggers) End(id, outcome string, now time.Time) (*TriggerWindow, error) {
	var ended *TriggerWindow
	err := t.update(func(windows []TriggerWindow) ([]TriggerWindow, error) {
		for i := range windows {
			w := &windows[i]
			if w.ID() != id {
				continue
			}
			if !w.Open(now) {
				return nil, fmt.Errorf("окно %s уже не действует", id)
			}
			w.Ended, w.Outcome = now, outcome
			copy := *w
			ended = &copy
			return windows, nil
		}
		return nil, fmt.Errorf("окна %s нет", id)
	})
	return ended, err
}

// Settle закрывает истёкшие окна и возвращает их: истечение — событие,
// которое журнал аудита должен помнить, а не только вывод.
func (t *Triggers) Settle(now time.Time) ([]TriggerWindow, error) {
	windows, err := t.Windows()
	if err != nil {
		return nil, err
	}
	due := false
	for _, w := range windows {
		if w.Ended.IsZero() && !now.Before(w.Until) {
			due = true
		}
	}
	if !due {
		return nil, nil
	}
	var expired []TriggerWindow
	err = t.update(func(windows []TriggerWindow) ([]TriggerWindow, error) {
		for i := range windows {
			w := &windows[i]
			if w.Ended.IsZero() && !now.Before(w.Until) {
				w.Ended, w.Outcome = w.Until, fmt.Sprintf("истекло (max_window %s)", w.Window)
				expired = append(expired, *w)
			}
		}
		return windows, nil
	})
	return expired, err
}

// ForRole — открытые окна роли и последнее окно вообще.
func ForRole(windows []TriggerWindow, role string, now time.Time) (open []TriggerWindow, last *TriggerWindow) {
	for i := range windows {
		w := windows[i]
		if w.Role != role {
			continue
		}
		if w.Open(now) {
			open = append(open, w)
		}
		if last == nil || w.Start.After(last.Start) {
			last = &windows[i]
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].Start.Before(open[j].Start) })
	return open, last
}

// lockPath — замок на файле: создаётся исключительно, замок старше минуты
// оставлен пропавшим процессом и снимается.
func lockPath(path string) (func(), error) {
	for i := 0; ; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > time.Minute {
			os.Remove(path)
			continue
		}
		if i >= 200 {
			return nil, fmt.Errorf("%s занят другим процессом", filepath.Base(path))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeJSONAtomic пишет JSON во временный файл и переименованием: оборванная
// запись не оставит полфайла.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
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
