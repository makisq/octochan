package rlm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	// Тело ошибочного ответа читаем с ограничением, чтобы случайная HTML-страница
	// от прокси не утекла целиком в лог.
	maxErrorBodySize = 8 << 10
)

// APIError описывает неуспешный HTTP-ответ RLM API.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
	URL        string
}

func (e *APIError) Error() string {
	body := e.Body
	if body == "" {
		body = e.Status
	}
	return fmt.Sprintf("ошибка API (код %d): %s", e.StatusCode, body)
}

// IsNotFound сообщает, что API ответил 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsTableIDError распознаёт отказ API из-за неверного table_id.
// Раньше это делалось через strings.Contains по тексту любой ошибки, из-за чего
// сетевой сбой мог быть принят за «не тот table_id».
func IsTableIDError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	body := strings.ToLower(apiErr.Body)
	return strings.Contains(body, "table_id") || strings.Contains(body, "идентификатор таблицы")
}

// IsTableRowIDError распознаёт отказ API из-за отсутствующего table_row_id.
func IsTableRowIDError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		strings.Contains(strings.ToLower(apiErr.Body), "table_row_id")
}

// APIClient — единственная точка похода в RLM API.
// Заголовок авторизации проставляется здесь, поэтому модулям сценариев больше
// не нужно тащить токен через свои структуры.
type APIClient struct {
	hc       *http.Client
	token    string
	tasksURL string
	baseURL  string
}

// ErrDryRun возвращается, когда в режиме плана делается изменяющий запрос.
var ErrDryRun = errors.New("режим плана: изменяющие запросы запрещены")

// IsDryRun у настоящего клиента всегда ложно: запрет накладывает обёртка
// DryRun, а не сам клиент. Раньше это было изменяемое поле, которое можно
// было забыть сбросить.
func (c *APIClient) IsDryRun() bool { return false }

// NewAPIClient собирает клиента из текущих настроек (Configure) и токена.
func NewAPIClient() (*APIClient, error) {
	s := currentSettings()
	tasksURL := strings.TrimSpace(s.APIURL)
	if tasksURL == "" {
		return nil, errors.New("не задан адрес RLM API: executors.rlm.api_url (или defaults.api_url) в config.yaml ochan")
	}

	token := APIToken()
	if token == "" {
		return nil, fmt.Errorf("токен RLM не установлен: ochan auth --executor rlm "+
			"или переменная окружения OCTOCHAN_TOKEN_RLM (прежняя — %s)", EnvTokenVar)
	}

	timeout := s.HTTPTimeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}

	base := strings.TrimSuffix(tasksURL, ".json")
	base = strings.TrimSuffix(base, "/")

	return &APIClient{
		hc:       &http.Client{Timeout: timeout},
		token:    token,
		tasksURL: tasksURL,
		baseURL:  base,
	}, nil
}

// TasksURL — адрес создания задач.
func (c *APIClient) TasksURL() string { return c.tasksURL }

// TaskURL — адрес конкретной задачи.
func (c *APIClient) TaskURL(taskID string) string {
	return fmt.Sprintf("%s/%s/", c.baseURL, taskID)
}

// TaskEventsURL — адрес журнала событий задачи.
func (c *APIClient) TaskEventsURL(taskID string) string {
	return fmt.Sprintf("%s/%s/events/", c.baseURL, taskID)
}

// Post отправляет JSON и, если out != nil, разбирает ответ в out.
func (c *APIClient) Post(ctx context.Context, url string, body, out any) error {
	return c.do(ctx, http.MethodPost, url, body, out)
}

// Get забирает JSON по адресу url.
func (c *APIClient) Get(ctx context.Context, url string, out any) error {
	return c.do(ctx, http.MethodGet, url, nil, out)
}

func (c *APIClient) do(ctx context.Context, method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("ошибка формирования JSON: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Token "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка выполнения запроса %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	// Успехом считается любой 2xx: создание задачи отдаёт 201, а не 200.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(errBody)),
			URL:        url,
		}
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ошибка чтения ответа: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("пустой ответ сервера (%s %s)", method, url)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("ошибка парсинга ответа: %w", err)
	}
	return nil
}

// ExtractTaskID достаёт идентификатор задачи из ответа API.
// JSON-числа приходят как float64, поэтому строковый и числовой вид
// обрабатываются одинаково.
func ExtractTaskID(payload map[string]any) (string, error) {
	raw, ok := payload["id"]
	if !ok || raw == nil {
		return "", errors.New("ID задачи отсутствует в ответе сервера")
	}

	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return "", errors.New("ID задачи пуст")
		}
		return v, nil
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), nil
		}
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case json.Number:
		return v.String(), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	default:
		return "", fmt.Errorf("неподдерживаемый формат ID: %T", raw)
	}
}
