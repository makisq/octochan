package module

import (
	"context"
	"fmt"
	"sync"

	"ochan/rlm"
)

// fakeAPI — подставной RLM для тестов модулей.
//
// Ради него и вводился интерфейс rlm.ScenarioAPI: пока модули принимали
// *rlm.APIClient, проверить их без живого сервера было нельзя, и тестов
// у них не было ни одного.
type fakeAPI struct {
	mu sync.Mutex

	// posts хранит все отправленные запросы в порядке отправки.
	posts []postCall
	// gets хранит адреса выполненных чтений.
	gets []string
	// waits хранит идентификаторы задач, которых ждали.
	waits []string

	// postFn задаёт ответ на создание задачи. Если nil, возвращается
	// последовательный идентификатор.
	postFn func(url string, body any) (map[string]any, error)
	// waitFn задаёт результат ожидания задачи.
	waitFn func(taskID string) (map[string]any, error)

	nextID int
}

type postCall struct {
	URL  string
	Body map[string]any
}

func newFakeAPI() *fakeAPI { return &fakeAPI{} }

func (f *fakeAPI) TasksURL() string { return "https://rlm.test/api/tasks.json" }
func (f *fakeAPI) IsDryRun() bool   { return false }

func (f *fakeAPI) Get(_ context.Context, url string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, url)
	return nil
}

func (f *fakeAPI) Post(_ context.Context, url string, body, out any) error {
	f.mu.Lock()
	bodyMap, _ := body.(map[string]any)
	f.posts = append(f.posts, postCall{URL: url, Body: bodyMap})
	f.nextID++
	id := f.nextID
	postFn := f.postFn
	f.mu.Unlock()

	var (
		response map[string]any
		err      error
	)
	if postFn != nil {
		response, err = postFn(url, body)
	} else {
		response = map[string]any{"id": float64(1000 + id)}
	}
	if err != nil {
		return err
	}

	if target, ok := out.(*map[string]any); ok {
		*target = response
	}
	return nil
}

func (f *fakeAPI) WaitForTask(_ context.Context, taskID string, _ rlm.WaitOptions) (map[string]any, error) {
	f.mu.Lock()
	f.waits = append(f.waits, taskID)
	waitFn := f.waitFn
	f.mu.Unlock()

	if waitFn != nil {
		return waitFn(taskID)
	}
	return map[string]any{"status": "completed"}, nil
}

// postCount возвращает число отправленных запросов.
func (f *fakeAPI) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// postedServices перечисляет сервисы отправленных запросов.
func (f *fakeAPI) postedServices() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	services := make([]string, 0, len(f.posts))
	for _, post := range f.posts {
		service, _ := post.Body["service"].(string)
		services = append(services, service)
	}
	return services
}

// apiError строит отказ RLM, как его увидел бы модуль.
func apiError(status int, body string) error {
	return &rlm.APIError{StatusCode: status, Body: body}
}

// requestParams достаёт блок params из запроса.
func requestParams(req *rlm.APIRequest) (map[string]any, error) {
	params, ok := req.Body["params"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("в запросе нет params: %#v", req.Body)
	}
	return params, nil
}

// requestItems достаёт items из запроса в виде списка карт.
func requestItems(req *rlm.APIRequest) ([]map[string]any, error) {
	switch list := req.Body["items"].(type) {
	case []map[string]string:
		out := make([]map[string]any, len(list))
		for i, item := range list {
			converted := make(map[string]any, len(item))
			for k, v := range item {
				converted[k] = v
			}
			out[i] = converted
		}
		return out, nil
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("неожиданный формат items: %T", req.Body["items"])
	}
}
