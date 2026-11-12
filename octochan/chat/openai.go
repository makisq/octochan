package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Сервер с интерфейсом OpenAI (chat completions с tools): сам OpenAI и всё,
// что говорит так же, — локальные серверы моделей (Ollama, vLLM, llama.cpp),
// шлюзы и модели других поставщиков. Адрес — llm.chat.url, модель —
// llm.chat.model; ключ серверу без проверки доступа не нужен.

const openAIURL = "https://api.openai.com/v1"

type openAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAI struct {
	client   *http.Client
	url, key string
	model    string
	max      int
	tools    []map[string]any
	messages []openAIMessage
}

func newOpenAI(o Options) (Conversation, error) {
	if strings.TrimSpace(o.Model) == "" {
		return nil, errors.New("llm.chat.model: у поставщика openai модель называют явно — имя модели сервера")
	}
	client, err := httpClient(o)
	if err != nil {
		return nil, err
	}
	c := &openAI{client: client, url: strings.TrimRight(o.URL, "/"), key: o.Key, model: o.Model, max: o.MaxTokens}
	if c.url == "" {
		c.url = openAIURL
	}
	if o.System != "" {
		c.messages = append(c.messages, openAIMessage{Role: "system", Content: o.System})
	}
	for _, t := range o.Tools {
		c.tools = append(c.tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": schemaOrEmpty(t.Schema)}})
	}
	return c, nil
}

// schemaOrEmpty — схема входа инструмента; без схемы — объект без полей.
func schemaOrEmpty(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return schema
}

func (c *openAI) Describe() string { return c.url + ", " + c.model }

func (c *openAI) Say(text string) {
	c.messages = append(c.messages, openAIMessage{Role: "user", Content: text})
}

func (c *openAI) Results(results []Result) {
	for _, r := range results {
		text := r.Text
		if r.IsError {
			text = "ERROR: " + text
		}
		c.messages = append(c.messages, openAIMessage{Role: "tool", ToolCallID: r.ID, Content: text})
	}
}

func (c *openAI) Next(ctx context.Context) (Reply, error) {
	body := map[string]any{"model": c.model, "messages": c.messages}
	if len(c.tools) > 0 {
		body["tools"] = c.tools
	}
	if c.max > 0 {
		body["max_tokens"] = c.max
	}
	headers := map[string]string{}
	if c.key != "" {
		headers["Authorization"] = "Bearer " + c.key
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      openAIMessage `json:"message"`
			FinishReason string        `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := postJSON(ctx, c.client, c.url, c.url+"/chat/completions", headers, body, &resp); err != nil {
		return Reply{}, err
	}
	if len(resp.Choices) == 0 {
		return Reply{}, fmt.Errorf("ответ %s без вариантов (choices)", c.url)
	}
	choice := resp.Choices[0]
	msg := choice.Message
	msg.Role = "assistant"
	c.messages = append(c.messages, msg)

	reply := Reply{Text: msg.Content, Model: resp.Model, Stop: StopEnd}
	for _, call := range msg.ToolCalls {
		reply.Calls = append(reply.Calls, Call{ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(call.Function.Arguments)})
	}
	switch {
	case choice.FinishReason == "length":
		reply.Stop = StopLength
	case choice.FinishReason == "content_filter":
		reply.Stop, reply.Note = StopRefusal, "фильтр содержимого поставщика"
	case len(reply.Calls) > 0:
		reply.Stop = StopTools
	}
	return reply, nil
}
