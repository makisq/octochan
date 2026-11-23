package chat

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GigaChat (Сбер). Отличия от интерфейса OpenAI, из-за которых это свой
// поставщик:
//   - токен доступа выдаётся на полчаса по ключу авторизации (OAuth) и
//     обновляется здесь же;
//   - инструменты называются functions; вызов за ход один, без номера, а
//     его аргументы — объект JSON, не строка;
//   - результат функции возвращается сообщением роли function, и его текст
//     должен быть JSON.
//
// Сертификаты серверов Сбера выданы центром Минцифры: его корневой
// сертификат добавляют настройкой llm.chat.ca — проверку сертификата ochan
// не выключает.

const (
	gigaChatURL     = "https://gigachat.devices.sberbank.ru/api/v1"
	gigaChatAuthURL = "https://ngw.devices.sberbank.ru:9443/api/v2/oauth"
	gigaChatScope   = "GIGACHAT_API_PERS"
	gigaChatModel   = "GigaChat"
)

type gigaFunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type gigaMessage struct {
	Role         string            `json:"role"`
	Content      string            `json:"content"`
	Name         string            `json:"name,omitempty"`
	FunctionCall *gigaFunctionCall `json:"function_call,omitempty"`
	// StateID — состояние функций хода: возвращается серверу с ответом
	// модели, как пришло.
	StateID string `json:"functions_state_id,omitempty"`
}

type gigaChat struct {
	client              *http.Client
	url, authURL, scope string
	key, model          string
	max                 int
	functions           []map[string]any
	messages            []gigaMessage

	mu      sync.Mutex
	token   string
	expires time.Time
	calls   int
}

func newGigaChat(o Options) (Conversation, error) {
	if o.Key == "" {
		return nil, errors.New("GigaChat: нужен ключ авторизации — ochan auth --executor llm (или переменная из llm.chat.key_env)")
	}
	client, err := httpClient(o)
	if err != nil {
		return nil, err
	}
	c := &gigaChat{client: client, url: strings.TrimRight(o.URL, "/"), authURL: o.AuthURL, scope: o.Scope, key: o.Key,
		model: o.Model, max: o.MaxTokens}
	if c.url == "" {
		c.url = gigaChatURL
	}
	if c.authURL == "" {
		c.authURL = gigaChatAuthURL
	}
	if c.scope == "" {
		c.scope = gigaChatScope
	}
	if c.model == "" {
		c.model = gigaChatModel
	}
	if o.System != "" {
		c.messages = append(c.messages, gigaMessage{Role: "system", Content: o.System})
	}
	for _, t := range o.Tools {
		c.functions = append(c.functions, map[string]any{"name": t.Name, "description": t.Description, "parameters": schemaOrEmpty(t.Schema)})
	}
	return c, nil
}

func (c *gigaChat) Describe() string { return "GigaChat, " + c.model }

func (c *gigaChat) Say(text string) {
	c.messages = append(c.messages, gigaMessage{Role: "user", Content: text})
}

func (c *gigaChat) Results(results []Result) {
	for _, r := range results {
		// Текст результата функции — JSON: обычный текст заворачивается.
		key := "result"
		if r.IsError {
			key = "error"
		}
		var content strings.Builder
		enc := json.NewEncoder(&content)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]string{key: r.Text})
		c.messages = append(c.messages, gigaMessage{Role: "function", Name: r.Name, Content: strings.TrimSpace(content.String())})
	}
}

// uuid4 — случайный номер запроса: его требует сервер выдачи токенов.
func uuid4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// accessToken — токен доступа: действующий или новый, по ключу авторизации.
func (c *gigaChat) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	id, err := uuid4()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL, strings.NewReader(url.Values{"scope": {c.scope}}.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("RqUID", id)
	req.Header.Set("Authorization", "Basic "+c.key)
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"` // миллисекунды
	}
	if err := doJSON(c.client, "GigaChat (выдача токена)", req, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("GigaChat не выдал токен доступа: проверьте ключ авторизации и llm.chat.scope")
	}
	c.token, c.expires = out.AccessToken, time.UnixMilli(out.ExpiresAt)
	if out.ExpiresAt == 0 {
		c.expires = time.Now().Add(25 * time.Minute)
	}
	return c.token, nil
}

func (c *gigaChat) Next(ctx context.Context) (Reply, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return Reply{}, err
	}
	body := map[string]any{"model": c.model, "messages": c.messages}
	if len(c.functions) > 0 {
		body["functions"], body["function_call"] = c.functions, "auto"
	}
	if c.max > 0 {
		body["max_tokens"] = c.max
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      gigaMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := postJSON(ctx, c.client, "GigaChat", c.url+"/chat/completions", map[string]string{"Authorization": "Bearer " + token},
		body, &resp); err != nil {
		return Reply{}, err
	}
	if len(resp.Choices) == 0 {
		return Reply{}, errors.New("ответ GigaChat без вариантов (choices)")
	}
	choice := resp.Choices[0]
	msg := choice.Message
	msg.Role = "assistant"
	c.messages = append(c.messages, msg)

	reply := Reply{Text: msg.Content, Model: resp.Model, Stop: StopEnd}
	if call := msg.FunctionCall; call != nil {
		// Номера у вызова нет — он один за ход; свой нужен только Turn.
		c.calls++
		input := call.Arguments
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		reply.Calls = []Call{{ID: fmt.Sprintf("call-%d", c.calls), Name: call.Name, Input: input}}
		reply.Stop = StopTools
	}
	switch choice.FinishReason {
	case "length":
		reply.Stop = StopLength
	case "blacklist":
		reply.Stop, reply.Note = StopRefusal, "запрос отклонён фильтром GigaChat"
	}
	return reply, nil
}
