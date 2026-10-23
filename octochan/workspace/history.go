package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"octochan/config"
)

// EntryKind — вид записи истории.
type EntryKind string

const (
	// EntryDiff — выполнено сравнение, изменений не вносилось.
	EntryDiff EntryKind = "diff"
	// EntryPatch — изменён конфигурационный файл.
	EntryPatch EntryKind = "patch"
	// EntryApply — во внешней системе созданы задачи.
	EntryApply EntryKind = "apply"
	// EntryScript — сценарий на языке алиасов записал файлы.
	EntryScript EntryKind = "script"
)

func (k EntryKind) Russian() string {
	switch k {
	case EntryDiff:
		return "сравнение"
	case EntryPatch:
		return "правка файла"
	case EntryApply:
		return "запуск сценария"
	case EntryScript:
		return "сценарий"
	default:
		return string(k)
	}
}

// Entry — запись о том, что произошло.
//
// Это то, во что превратились снапшоты: раньше diff складывал их в файлы,
// которые никто не читал, потому что LoadSnapshot и ApplySnapshot не были
// подключены ни к одной команде.
type Entry struct {
	ID        string          `json:"id"`
	Timestamp time.Time       `json:"timestamp"`
	Author    string          `json:"author"`
	Kind      EntryKind       `json:"kind"`
	Command   string          `json:"command"`
	Source    string          `json:"source,omitempty"`
	Target    string          `json:"target,omitempty"`
	Summary   string          `json:"summary"`
	Changes   []config.Change `json:"changes,omitempty"`

	// Before — полное состояние файла до изменения.
	//
	// Хранится целиком, а не как обратный diff: конфиги малы, а откат
	// по diff ломается, если файл успели поправить руками между операциями.
	Before config.ConfigMap `json:"before,omitempty"`

	// Files — файлы, которые записала операция, в состоянии до неё. Так
	// откатывается сценарий: он пишет несколько файлов, не обязательно
	// конфиги, и некоторые создаёт.
	Files []FileState `json:"files,omitempty"`

	// Reversible означает, что операцию можно отменить командой rollback.
	// Созданные во внешней системе задачи необратимы: перезапуск СУБД
	// уже произошёл, и отменять в октохане нечего.
	Reversible bool `json:"reversible"`

	// TaskIDs — идентификаторы задач, созданных во внешней системе.
	TaskIDs []string `json:"task_ids,omitempty"`

	// RevertedBy — идентификатор записи, которой этот шаг был отменён.
	RevertedBy string `json:"reverted_by,omitempty"`
	// RevertOf — идентификатор записи, которую этот шаг отменяет.
	RevertOf string `json:"revert_of,omitempty"`
}

// FileState — файл до операции.
type FileState struct {
	Path string `json:"path"`
	// Existed ложно, если файла не было: откат его удаляет.
	Existed bool   `json:"existed"`
	Content []byte `json:"content,omitempty"`
	// After — то, что операция оставила в файле (см. StateHash). Если
	// файл с тех пор поменяли, откат стёр бы чужую правку.
	After string `json:"after"`
}

// StateHash — отпечаток состояния файла: сумма SHA-256 содержимого или
// пустая строка, если файла нет.
func StateHash(exists bool, content []byte) string {
	if !exists {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

// Reverted сообщает, был ли шаг уже отменён.
func (e *Entry) Reverted() bool { return e.RevertedBy != "" }

// Describe возвращает однострочное описание для списка.
func (e *Entry) Describe() string {
	parts := []string{e.Timestamp.Local().Format("2006-01-02 15:04:05"), e.ID, e.Kind.Russian()}
	if e.Target != "" {
		parts = append(parts, e.Target)
	}
	parts = append(parts, e.Summary)
	return strings.Join(parts, "  ")
}

// History — журнал операций на диске.
type History struct {
	dir string
}

// NewHistory открывает журнал в указанной директории.
func NewHistory(dir string) (*History, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("не удалось создать директорию истории: %w", err)
	}
	return &History{dir: dir}, nil
}

// Dir возвращает путь к журналу.
func (h *History) Dir() string { return h.dir }

// fileName формирует имя файла так, чтобы лексикографический порядок
// совпадал с хронологическим: это позволяет не читать все записи,
// чтобы отсортировать их.
func (h *History) fileName(e *Entry) string {
	return fmt.Sprintf("%s-%s.json", e.Timestamp.Format("20060102T150405.000"), e.ID)
}

// Append записывает событие в журнал.
func (h *History) Append(e *Entry) error {
	if e.ID == "" {
		id, err := generateID()
		if err != nil {
			return err
		}
		e.ID = id
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.Author == "" {
		e.Author = currentUser()
	}

	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("не удалось сериализовать запись истории: %w", err)
	}

	path := filepath.Join(h.dir, h.fileName(e))
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("не удалось записать историю: %w", err)
	}
	return nil
}

// List возвращает записи от новых к старым. limit <= 0 означает «все».
func (h *History) List(limit int) ([]*Entry, error) {
	files, err := h.files()
	if err != nil {
		return nil, err
	}

	// Файлы отсортированы по возрастанию времени; читаем с конца,
	// чтобы не разбирать весь журнал ради последних десяти записей.
	entries := make([]*Entry, 0, len(files))
	for i := len(files) - 1; i >= 0; i-- {
		if limit > 0 && len(entries) >= limit {
			break
		}

		entry, err := h.read(files[i])
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Get находит запись по идентификатору или его началу.
//
// Полный идентификатор набирать неудобно, поэтому принимается префикс,
// но неоднозначный префикс — ошибка, а не выбор наугад.
func (h *History) Get(id string) (*Entry, error) {
	if id == "" {
		return nil, fmt.Errorf("не указан идентификатор записи")
	}

	files, err := h.files()
	if err != nil {
		return nil, err
	}

	var matches []*Entry
	for _, file := range files {
		entry, err := h.read(file)
		if err != nil {
			return nil, err
		}
		if entry.ID == id {
			return entry, nil
		}
		if strings.HasPrefix(entry.ID, id) {
			matches = append(matches, entry)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("запись %q не найдена", id)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, entry := range matches {
			ids[i] = entry.ID
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("префикс %q подходит нескольким записям: %s",
			id, strings.Join(ids, ", "))
	}
}

// MarkReverted помечает запись как отменённую.
func (h *History) MarkReverted(entry *Entry, revertID string) error {
	entry.RevertedBy = revertID

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("не удалось сериализовать запись истории: %w", err)
	}
	return os.WriteFile(filepath.Join(h.dir, h.fileName(entry)), data, filePerm)
}

func (h *History) files() ([]string, error) {
	dirEntries, err := os.ReadDir(h.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("не удалось прочитать историю: %w", err)
	}

	names := make([]string, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() && filepath.Ext(dirEntry.Name()) == ".json" {
			names = append(names, dirEntry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (h *History) read(name string) (*Entry, error) {
	path := filepath.Join(h.dir, name)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать %s: %w", path, err)
	}

	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("повреждена запись истории %s: %w", path, err)
	}
	return &entry, nil
}

// generateID возвращает короткий случайный идентификатор.
//
// Прежние снапшоты использовали UnixNano и давали коллизии при создании
// нескольких записей в одном цикле.
func generateID() (string, error) {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("не удалось сгенерировать идентификатор: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	if name := os.Getenv("USERNAME"); name != "" {
		return name
	}
	return "неизвестно"
}
