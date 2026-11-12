package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// Claude — модели Anthropic через официальный SDK.
//
// Ответ читается потоком: ход модели на трудной задаче бывает долгим, и
// обычный запрос упёрся бы в предел времени соединения. Размышления модели
// наружу не выводятся, но их блоки возвращаются ей в истории без изменений
// — так требует API. Глубину размышлений задаёт llm.chat.effort.

// DefaultClaudeModel — модель, когда llm.chat.model не задан.
const DefaultClaudeModel = "claude-opus-5-5"

// claudeMaxTokens — предел ответа, когда llm.chat.max_tokens не задан:
// потоку предел времени не мешает, а оборванный ответ пришлось бы повторять.
const claudeMaxTokens = 64000

type claude struct {
	client   anthropic.Client
	params   anthropic.BetaMessageNewParams
	messages []anthropic.BetaMessageParam
}

// claudeFallback — у модели есть запасная на стороне сервера: отказ по
// политике перезапускается на ней внутри того же запроса.
func claudeFallback(model string) bool {
	for _, prefix := range []string{"claude-fable-5-2", "claude-fable-5-1", "claude-mythos-5-2", "claude-mythos-5-1",
		"claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5"} {
		if model == prefix {
			return true
		}
	}
	return false
}

func newClaude(o Options) (Conversation, error) {
	model := strings.TrimSpace(o.Model)
	if model == "" {
		model = DefaultClaudeModel
	}
	var opts []option.RequestOption
	// Без ключа в настройках SDK берёт его сам: ANTHROPIC_API_KEY,
	// ANTHROPIC_AUTH_TOKEN или профиль ant auth login.
	if o.Key != "" {
		opts = append(opts, option.WithAPIKey(o.Key))
	}
	if o.URL != "" {
		opts = append(opts, option.WithBaseURL(o.URL))
	}
	if o.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(o.Timeout))
	}
	if o.CA != "" {
		transport, err := transportWithCA(o.CA)
		if err != nil {
			return nil, err
		}
		opts = append(opts, option.WithHTTPClient(&http.Client{Transport: transport}))
	}
	c := &claude{client: anthropic.NewClient(opts...)}
	c.params = anthropic.BetaMessageNewParams{Model: model, MaxTokens: claudeMaxTokens}
	if o.MaxTokens > 0 {
		c.params.MaxTokens = int64(o.MaxTokens)
	}
	if o.System != "" {
		// Инструменты и протокол — неизменное начало каждого запроса:
		// кешируются вместе.
		c.params.System = []anthropic.BetaTextBlockParam{{Text: o.System, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}}
	}
	switch effort := strings.ToLower(strings.TrimSpace(o.Effort)); effort {
	case "":
	case "low", "medium", "high", "xhigh", "max":
		c.params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(effort)}
	default:
		return nil, fmt.Errorf("llm.chat.effort: %q — low, medium, high, xhigh или max", o.Effort)
	}
	if o.Fallbacks && claudeFallback(model) {
		c.params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		c.params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}
	for _, t := range o.Tools {
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		extra := map[string]any{}
		if len(t.Schema) > 0 {
			if err := json.Unmarshal(t.Schema, &schema); err != nil {
				return nil, fmt.Errorf("инструмент %s: схема входа: %w", t.Name, err)
			}
			_ = json.Unmarshal(t.Schema, &extra)
		}
		for _, known := range []string{"type", "properties", "required"} {
			delete(extra, known)
		}
		if schema.Properties == nil {
			schema.Properties = map[string]any{}
		}
		tool := anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: schema.Properties, Required: schema.Required, ExtraFields: extra},
			// Вход инструмента идёт потоком по мере написания; годность его
			// JSON проверяет Turn перед вызовом.
			EagerInputStreaming: anthropic.Bool(true),
		}
		c.params.Tools = append(c.params.Tools, anthropic.BetaToolUnionParam{OfTool: &tool})
	}
	return c, nil
}

func (c *claude) Describe() string { return "Anthropic, " + string(c.params.Model) }

func (c *claude) Say(text string) {
	c.messages = append(c.messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
}

func (c *claude) Results(results []Result) {
	// Все результаты хода — одним сообщением: так модель и дальше вызывает
	// инструменты разом.
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(r.ID, r.Text, r.IsError))
	}
	c.messages = append(c.messages, anthropic.NewBetaUserMessage(blocks...))
}

func (c *claude) Next(ctx context.Context) (Reply, error) {
	params := c.params
	params.Messages = c.messages
	stream := c.client.Beta.Messages.NewStreaming(ctx, params)
	message := anthropic.BetaMessage{}
	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			return Reply{}, fmt.Errorf("ответ Anthropic: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return Reply{}, claudeError(err)
	}
	// Ответ уходит в историю целиком, с блоками размышлений.
	c.messages = append(c.messages, message.ToParam())

	reply := Reply{Model: string(message.Model), Stop: StopEnd}
	reply.Substitute = reply.Model != "" && reply.Model != string(c.params.Model)
	var text []string
	for _, block := range message.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text = append(text, b.Text)
		case anthropic.BetaToolUseBlock:
			reply.Calls = append(reply.Calls, Call{ID: b.ID, Name: b.Name, Input: json.RawMessage(b.JSON.Input.Raw())})
		}
	}
	reply.Text = strings.Join(text, "\n")
	switch message.StopReason {
	case anthropic.BetaStopReasonToolUse:
		reply.Stop = StopTools
	case anthropic.BetaStopReasonMaxTokens, anthropic.BetaStopReasonModelContextWindowExceeded:
		reply.Stop = StopLength
	case anthropic.BetaStopReasonRefusal:
		reply.Stop = StopRefusal
		reply.Note = strings.TrimSpace(string(message.StopDetails.Category) + " " + message.StopDetails.Explanation)
	}
	return reply, nil
}

// claudeError — ошибка API словами, по которым понятно, что делать.
func claudeError(err error) error {
	var api *anthropic.Error
	if !errors.As(err, &api) {
		return fmt.Errorf("запрос к Anthropic: %w", err)
	}
	switch api.StatusCode {
	case 401:
		return fmt.Errorf("Anthropic не принял ключ (401): ochan auth --executor llm или ANTHROPIC_API_KEY — %w", err)
	case 403:
		return fmt.Errorf("у ключа нет права на этот запрос (403): %w", err)
	case 404:
		return fmt.Errorf("Anthropic не знает такой модели или адреса (404) — проверьте llm.chat.model и llm.chat.url: %w", err)
	case 429:
		return fmt.Errorf("предел запросов Anthropic исчерпан (429) — повторите позже: %w", err)
	}
	if api.StatusCode >= 500 {
		return fmt.Errorf("сбой на стороне Anthropic (%d) — повторите позже: %w", api.StatusCode, err)
	}
	return fmt.Errorf("запрос к Anthropic отклонён (%d): %w", api.StatusCode, err)
}
