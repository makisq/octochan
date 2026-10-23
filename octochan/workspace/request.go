package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"octochan/config"
)

// RequestState — состояние заявки.
type RequestState string

const (
	// RequestPending — заявка ждёт решения.
	RequestPending RequestState = "pending"
	// RequestApproved — заявка согласована и может быть выполнена.
	RequestApproved RequestState = "approved"
	// RequestRejected — заявка отклонена.
	RequestRejected RequestState = "rejected"
	// RequestApplied — заявка выполнена.
	RequestApplied RequestState = "applied"
)

func (s RequestState) Russian() string {
	switch s {
	case RequestPending:
		return "ждёт решения"
	case RequestApproved:
		return "согласована"
	case RequestRejected:
		return "отклонена"
	case RequestApplied:
		return "выполнена"
	default:
		return string(s)
	}
}

// Request — заявка на изменение.
//
// RBAC здесь нет и пока не нужно: заявка отвечает не на вопрос «кому
// в принципе можно», а на вопрос «на это конкретное изменение сказали да».
// Аудитору нужен именно второй ответ, и он же соответствует тому процессу,
// который в организациях уже есть на бумаге.
type Request struct {
	ID      string       `json:"id"`
	State   RequestState `json:"state"`
	Created time.Time    `json:"created"`
	Author  string       `json:"author"`
	// Role — роль, в которой заявку создали. Своя заявка роли не
	// согласуется ею же, а заявку нейросети (llm) согласуют с явным
	// подтверждением.
	Role string `json:"role,omitempty"`

	// Source — откуда взят план: файл сценария или пара конфигов.
	Source string `json:"source"`
	// Service непуст, если заявка на выполнение сценария.
	Service string `json:"service,omitempty"`
	// Target — куда применяется, если это правка конфига.
	Target  string       `json:"target,omitempty"`
	Summary string       `json:"summary"`
	Plan    *config.Plan `json:"plan"`

	// Payload — исходный текст сценария, чтобы выполнить ровно то,
	// что согласовали, а не перечитанный с диска изменившийся файл.
	Payload string `json:"payload,omitempty"`

	// Script непуст, если заявка на выполнение сценария на языке алиасов.
	Script *ScriptRun `json:"script,omitempty"`

	Decision   string    `json:"decision,omitempty"`
	DecidedBy  string    `json:"decided_by,omitempty"`
	DecidedAt  time.Time `json:"decided_at,omitempty"`
	AppliedAt  time.Time `json:"applied_at,omitempty"`
	AppliedRef []string  `json:"applied_refs,omitempty"`
}

// ScriptRun — то, что нужно, чтобы выполнить сценарий на языке алиасов
// так же, как его планировали.
//
// Текста сценария мало. Относительные пути отсчитываются от каталога,
// в котором заявку создавали, а не от того, где её потом выполнят;
// песочница, в которой строили план, должна действовать и при выполнении;
// алиасы библиотеки, которые сценарий вызывает, могли поменяться.
type ScriptRun struct {
	// Dir — каталог, от которого отсчитываются относительные пути.
	Dir string `json:"dir"`

	// Sandboxed означает, что сценарий выполняется в песочнице с
	// разрешёнными каталогами Roots, переменными Env и схемами Live.
	Sandboxed bool     `json:"sandboxed"`
	Roots     []string `json:"roots,omitempty"`
	Env       []string `json:"env,omitempty"`
	Live      []string `json:"live,omitempty"`

	// Libraries — файлы алиасов, которые вызывает сценарий, и их суммы
	// SHA-256 на момент создания заявки.
	Libraries map[string]string `json:"libraries,omitempty"`

	// Packages — пакеты алиасов, которые вызывает сценарий, и их версии;
	// пустая строка — пакет без версии. Код сверяется по Libraries, а
	// версия нужна тому, кто согласует: по ней видно, с чем строился план.
	Packages map[string]string `json:"packages,omitempty"`

	// Effects — отпечатки действий из плана, по порядку. Перед выполнением
	// план строится заново и сверяется с ними.
	Effects []string `json:"effects"`

	// Prompt непуст, если сценарий написан по формальному промпту.
	Prompt *ScriptPrompt `json:"prompt,omitempty"`
}

// ScriptPrompt — формальный промпт, по которому модель написала сценарий,
// и её отчёт о решениях, которые промпт оставил открытыми.
//
// Согласующий видит не только сценарий, но и задачу, и то, что модель
// решила сама. Жёсткие правила промпта проверены по плану при оформлении
// заявки и проверяются снова перед выполнением.
type ScriptPrompt struct {
	Name       string `json:"name"`
	Text       string `json:"text"`
	ReportName string `json:"report_name,omitempty"`
	Report     string `json:"report,omitempty"`
	// Rules — жёсткие правила промпта, которые план соблюдает.
	Rules []string `json:"rules,omitempty"`
}

// Describe возвращает однострочное описание для списка.
func (r *Request) Describe() string {
	parts := []string{
		r.Created.Local().Format("2006-01-02 15:04"),
		r.ID,
		fmt.Sprintf("%-12s", r.State.Russian()),
	}
	if r.Service != "" {
		parts = append(parts, r.Service)
	} else if r.Target != "" {
		parts = append(parts, r.Target)
	}
	parts = append(parts, r.Summary)
	return strings.Join(parts, "  ")
}

// Requests — хранилище заявок на диске.
type Requests struct {
	dir string
}

// NewRequests открывает хранилище заявок.
func NewRequests(dir string) (*Requests, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("не удалось создать директорию заявок: %w", err)
	}
	return &Requests{dir: dir}, nil
}

// Dir возвращает путь к хранилищу.
func (r *Requests) Dir() string { return r.dir }

func (r *Requests) path(id string) string {
	return filepath.Join(r.dir, id+".json")
}

// Create сохраняет новую заявку.
func (r *Requests) Create(request *Request) error {
	if request.ID == "" {
		id, err := generateID()
		if err != nil {
			return err
		}
		request.ID = id
	}
	if request.Created.IsZero() {
		request.Created = time.Now().UTC()
	}
	if request.Author == "" {
		request.Author = currentUser()
	}
	if request.State == "" {
		request.State = RequestPending
	}
	return r.save(request)
}

func (r *Requests) save(request *Request) error {
	data, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return fmt.Errorf("не удалось сериализовать заявку: %w", err)
	}
	if err := os.WriteFile(r.path(request.ID), data, filePerm); err != nil {
		return fmt.Errorf("не удалось записать заявку: %w", err)
	}
	return nil
}

// Get находит заявку по идентификатору или его началу.
func (r *Requests) Get(id string) (*Request, error) {
	if id == "" {
		return nil, fmt.Errorf("не указан идентификатор заявки")
	}

	all, err := r.List()
	if err != nil {
		return nil, err
	}

	var matches []*Request
	for _, request := range all {
		if request.ID == id {
			return request, nil
		}
		if strings.HasPrefix(request.ID, id) {
			matches = append(matches, request)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("заявка %q не найдена", id)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, request := range matches {
			ids[i] = request.ID
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("префикс %q подходит нескольким заявкам: %s", id, strings.Join(ids, ", "))
	}
}

// List возвращает заявки от новых к старым.
func (r *Requests) List() ([]*Request, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("не удалось прочитать заявки: %w", err)
	}

	requests := make([]*Request, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(r.dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("не удалось прочитать %s: %w", entry.Name(), err)
		}

		var request Request
		if err := json.Unmarshal(data, &request); err != nil {
			return nil, fmt.Errorf("повреждена заявка %s: %w", entry.Name(), err)
		}
		requests = append(requests, &request)
	}

	// Порядок задан полностью: одного времени создания мало, потому что
	// часы грубее, чем цикл создания заявок, и одинаковые отметки времени —
	// обычное дело. Без второго ключа один и тот же набор заявок выводился
	// бы в разном порядке от запуска к запуску, а это журнал согласований.
	sort.Slice(requests, func(i, j int) bool {
		if !requests[i].Created.Equal(requests[j].Created) {
			return requests[i].Created.After(requests[j].Created)
		}
		return requests[i].ID < requests[j].ID
	})
	return requests, nil
}

// Decide переводит заявку в согласованную или отклонённую.
//
// Решение принимается один раз: повторное согласование уже выполненной
// заявки или пересмотр отклонённой — это новая заявка, а не правка старой.
// Иначе журнал перестаёт отвечать на вопрос, что именно было одобрено.
func (r *Requests) Decide(request *Request, state RequestState, who, comment string) error {
	if state != RequestApproved && state != RequestRejected {
		return fmt.Errorf("недопустимое решение: %s", state)
	}
	if request.State != RequestPending {
		return fmt.Errorf("заявка %s уже %s — решение принимается один раз",
			request.ID, request.State.Russian())
	}

	request.State = state
	request.Decision = comment
	request.DecidedBy = who
	if request.DecidedBy == "" {
		request.DecidedBy = currentUser()
	}
	request.DecidedAt = time.Now().UTC()

	return r.save(request)
}

// MarkApplied отмечает заявку выполненной.
func (r *Requests) MarkApplied(request *Request, refs []string) error {
	if request.State != RequestApproved {
		return fmt.Errorf("заявка %s не согласована (%s): выполнять нечего",
			request.ID, request.State.Russian())
	}

	request.State = RequestApplied
	request.AppliedAt = time.Now().UTC()
	request.AppliedRef = refs

	return r.save(request)
}
