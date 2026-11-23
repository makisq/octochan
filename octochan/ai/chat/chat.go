// Package chat — разговор оператора с внешней нейросетью из оболочки ochan.
//
// ochan здесь — хозяин разговора: он отправляет реплику модели поставщика
// (Claude, GigaChat, любой сервер с интерфейсом OpenAI), а когда модель
// просит инструмент — вызывает его сам и возвращает ей результат. Инструменты
// — те же, что у ochan-mcp, и вызываются через него: файлы только в
// разрешённом каталоге, роль llm, песочница, заявка вместо выполнения.
// Модель предлагает, человек решает (П6) — разговор этого не меняет.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Tool — инструмент, который модель может вызвать.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema входа — объект
}

// Call — вызов инструмента, о котором просит модель.
type Call struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Result — результат вызова, который возвращается модели.
type Result struct {
	ID      string
	Name    string
	Text    string
	IsError bool
}

// Stop — чем кончился ход модели.
type Stop string

const (
	StopEnd     Stop = "end"     // ответ закончен
	StopTools   Stop = "tools"   // модель ждёт результатов инструментов
	StopLength  Stop = "length"  // ответ оборван по пределу токенов
	StopRefusal Stop = "refusal" // модель отказалась отвечать
)

// Reply — ход модели.
type Reply struct {
	Text  string
	Calls []Call
	Stop  Stop
	Note  string // пояснение отказа
	Model string // модель, которая ответила (после запасной — она)
	// Substitute — ответила запасная модель, а не та, которую просили.
	Substitute bool
}

// Conversation — разговор с моделью одного поставщика. Историю каждый
// поставщик хранит в своём виде: у Claude в неё входят блоки размышлений,
// которые возвращаются модели без изменений.
type Conversation interface {
	// Say добавляет реплику человека.
	Say(text string)
	// Results добавляет результаты инструментов прошлого хода.
	Results(results []Result)
	// Next — ход модели по истории.
	Next(ctx context.Context) (Reply, error)
	// Describe — поставщик и модель одной строкой.
	Describe() string
}

// Options — настройки модели: llm.chat.* в config.yaml.
type Options struct {
	Provider  string // anthropic | openai | gigachat
	URL       string // адрес API; пусто — адрес поставщика
	AuthURL   string // gigachat: адрес выдачи токена
	Scope     string // gigachat: область доступа
	Model     string
	Key       string // ключ; пусто — как принято у поставщика (окружение)
	MaxTokens int
	Effort    string        // anthropic: low | medium | high | xhigh | max
	Timeout   time.Duration // предел одного запроса
	CA        string        // файл корневых сертификатов (PEM) в дополнение к системным
	Fallbacks bool          // anthropic: запасная модель при отказе по политике
	System    string        // протокол работы — системный промпт
	Tools     []Tool
}

// Providers — поставщики по порядку.
var Providers = []string{"anthropic", "openai", "gigachat"}

// New — разговор с моделью поставщика.
func New(o Options) (Conversation, error) {
	switch strings.ToLower(strings.TrimSpace(o.Provider)) {
	case "anthropic", "claude":
		return newClaude(o)
	case "openai":
		return newOpenAI(o)
	case "gigachat":
		return newGigaChat(o)
	case "":
		return nil, errors.New("поставщик модели не задан: llm.chat.provider — " + strings.Join(Providers, ", "))
	}
	return nil, fmt.Errorf("llm.chat.provider: %q — нужен один из: %s", o.Provider, strings.Join(Providers, ", "))
}

// Toolbox — инструменты ochan для модели и протокол работы с ними.
type Toolbox interface {
	Instructions() string
	Tools() []Tool
	Call(ctx context.Context, name string, input json.RawMessage) (text string, isError bool, err error)
	Close() error
}

// Events — что видит оператор, пока модель работает.
type Events struct {
	Text func(text string)         // текст модели
	Call func(call Call)           // модель вызывает инструмент
	Done func(call Call, r Result) // инструмент ответил
	// Other — ответила не та модель, которую просили: запасная после
	// отказа по политике.
	Other func(model string)
}

// ErrRefused — модель отказалась отвечать.
var ErrRefused = errors.New("модель отказалась отвечать")

// Turn — ход разговора: модель отвечает на последнюю реплику, вызывая
// инструменты, пока не закончит; maxSteps — предел обращений к модели.
func Turn(ctx context.Context, conv Conversation, box Toolbox, maxSteps int, ev Events) error {
	for step := 0; ; step++ {
		if step >= maxSteps {
			return fmt.Errorf("модель сделала %d обращений подряд и не закончила — остановлено (llm.chat.max_steps)", maxSteps)
		}
		reply, err := conv.Next(ctx)
		if err != nil {
			return err
		}
		if reply.Substitute && ev.Other != nil {
			ev.Other(reply.Model)
		}
		if text := strings.TrimSpace(reply.Text); text != "" && ev.Text != nil {
			ev.Text(text)
		}
		switch reply.Stop {
		case StopRefusal:
			// Вызовы инструментов такого хода не выполняются: отказ мог
			// оборвать их на середине.
			if reply.Note != "" {
				return fmt.Errorf("%w: %s", ErrRefused, reply.Note)
			}
			return ErrRefused
		case StopLength:
			return errors.New("ответ модели оборван по пределу токенов — увеличьте llm.chat.max_tokens; " +
				"инструменты оборванного хода не вызваны")
		}
		if len(reply.Calls) == 0 {
			return nil
		}
		if box == nil {
			return errors.New("модель просит инструмент, а инструментов у разговора нет")
		}
		results := make([]Result, 0, len(reply.Calls))
		for _, call := range reply.Calls {
			if ev.Call != nil {
				ev.Call(call)
			}
			res := Result{ID: call.ID, Name: call.Name}
			// Вход инструмента — объект JSON; иное возвращается модели
			// ошибкой, чтобы она повторила вызов, а инструмент не запускается.
			if input := strings.TrimSpace(string(call.Input)); !json.Valid(call.Input) || !strings.HasPrefix(input, "{") {
				broken, _ := json.Marshal(map[string]string{"INVALID_JSON": string(call.Input)})
				res.Text, res.IsError = string(broken), true
			} else if res.Text, res.IsError, err = box.Call(ctx, call.Name, call.Input); err != nil {
				return fmt.Errorf("инструмент %s: %w", call.Name, err)
			}
			if ev.Done != nil {
				ev.Done(call, res)
			}
			results = append(results, res)
		}
		conv.Results(results)
	}
}
