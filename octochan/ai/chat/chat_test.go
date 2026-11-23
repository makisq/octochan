package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Тестовая программа — она же поддельный сервер инструментов: так клиент
// MCP проверяется на настоящем процессе и настоящем stdio.
func TestMain(m *testing.M) {
	if os.Getenv("CHAT_TEST_AS_MCP") == "1" {
		fakeMCP()
		return
	}
	os.Exit(m.Run())
}

func fakeMCP() {
	in := bufio.NewReader(os.Stdin)
	fmt.Fprintln(os.Stderr, "fake-mcp: запущен")
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &msg) != nil || len(msg.ID) == 0 {
			continue
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "instructions": "PROTOCOL: propose, never apply."}
		case "tools/list":
			// Уведомление между запросом и ответом клиент пропускает.
			fmt.Println(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`)
			result = map[string]any{"tools": []map[string]any{{"name": "plan_script", "description": "Build a plan",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}},
					"required": []string{"script"}, "additionalProperties": false}}}}
		case "tools/call":
			switch msg.Params.Name {
			case "plan_script":
				result = map[string]any{"content": []map[string]string{{"type": "text", "text": "plan for " + fmt.Sprint(msg.Params.Arguments["script"])}}}
			case "broken":
				result = map[string]any{"content": []map[string]string{{"type": "text", "text": "no such file"}}, "isError": true}
			case "die":
				fmt.Fprintln(os.Stderr, "fake-mcp: падаю")
				os.Exit(3)
			default:
				out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32602, "message": "unknown tool " + msg.Params.Name}})
				fmt.Println(string(out))
				continue
			}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		fmt.Println(string(out))
	}
}

func startFakeMCP(t *testing.T) *MCP {
	t.Helper()
	self, _ := os.Executable()
	m, err := StartMCP(context.Background(), self, nil, append(os.Environ(), "CHAT_TEST_AS_MCP=1"), "ochan-test", "0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// Клиент MCP: знакомство, таблица инструментов, вызов; ошибка инструмента и
// ошибка протокола идут модели, пропавший сервер — оператору с его журналом.
func TestMCPClient(t *testing.T) {
	m := startFakeMCP(t)
	if m.Instructions() != "PROTOCOL: propose, never apply." || len(m.Tools()) != 1 || m.Tools()[0].Name != "plan_script" ||
		!strings.Contains(string(m.Tools()[0].Schema), `"additionalProperties":false`) {
		t.Fatalf("знакомство: %q, %+v", m.Instructions(), m.Tools())
	}
	ctx := context.Background()
	text, isErr, err := m.Call(ctx, "plan_script", json.RawMessage(`{"script":"tune.ochan"}`))
	if err != nil || isErr || text != "plan for tune.ochan" {
		t.Fatalf("вызов: %q, %v, %v", text, isErr, err)
	}
	if text, isErr, err := m.Call(ctx, "broken", json.RawMessage(`{}`)); err != nil || !isErr || text != "no such file" {
		t.Fatalf("ошибка инструмента: %q, %v, %v", text, isErr, err)
	}
	if text, isErr, err := m.Call(ctx, "nope", json.RawMessage(`{}`)); err != nil || !isErr || !strings.Contains(text, "unknown tool nope") {
		t.Fatalf("ошибка протокола: %q, %v, %v", text, isErr, err)
	}
	if _, _, err := m.Call(ctx, "die", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "fake-mcp: падаю") {
		t.Fatalf("пропавший сервер: %v", err)
	}
}

// scripted — модель по сценарию: ходы заданы заранее.
type scripted struct {
	replies []Reply
	said    []string
	results [][]Result
}

func (s *scripted) Describe() string { return "scripted" }
func (s *scripted) Say(text string)  { s.said = append(s.said, text) }
func (s *scripted) Results(r []Result) {
	s.results = append(s.results, r)
}
func (s *scripted) Next(context.Context) (Reply, error) {
	if len(s.replies) == 0 {
		return Reply{}, errors.New("сценарий кончился")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

// Ход разговора: инструменты вызываются, пока модель не закончит; негодный
// вход инструмента возвращается ей ошибкой, отказ и оборванный ответ
// инструментов не запускают, бесконечный цикл обрывается.
func TestTurn(t *testing.T) {
	box := startFakeMCP(t)
	ctx := context.Background()
	conv := &scripted{replies: []Reply{
		{Text: "Смотрю план.", Stop: StopTools, Calls: []Call{
			{ID: "1", Name: "plan_script", Input: json.RawMessage(`{"script":"a.ochan"}`)},
			{ID: "2", Name: "plan_script", Input: json.RawMessage(`{"script": "b.och`)},
		}},
		{Text: "Готово: заявка оформлена.", Stop: StopEnd},
	}}
	var seen []string
	err := Turn(ctx, conv, box, 8, Events{
		Text: func(text string) { seen = append(seen, "text:"+text) },
		Call: func(c Call) { seen = append(seen, "call:"+c.Name) },
		Done: func(c Call, r Result) { seen = append(seen, fmt.Sprintf("done:%s:%v", c.ID, r.IsError)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "text:Смотрю план.|call:plan_script|done:1:false|call:plan_script|done:2:true|text:Готово: заявка оформлена."
	if got := strings.Join(seen, "|"); got != want {
		t.Fatalf("события:\n%s\nожидалось:\n%s", got, want)
	}
	if r := conv.results[0]; len(r) != 2 || r[0].Text != "plan for a.ochan" || !r[1].IsError || !strings.Contains(r[1].Text, "INVALID_JSON") {
		t.Fatalf("результаты: %+v", r)
	}

	for name, c := range map[string]struct {
		reply Reply
		want  string
	}{
		"отказ":   {Reply{Stop: StopRefusal, Note: "cyber", Calls: []Call{{ID: "1", Name: "die", Input: json.RawMessage(`{}`)}}}, "модель отказалась отвечать: cyber"},
		"обрыв":   {Reply{Stop: StopLength, Calls: []Call{{ID: "1", Name: "die", Input: json.RawMessage(`{}`)}}}, "оборван по пределу токенов"},
		"без дна": {Reply{Stop: StopTools, Calls: []Call{{ID: "1", Name: "plan_script", Input: json.RawMessage(`{"script":"x"}`)}}}, "3 обращений подряд"},
	} {
		conv := &scripted{replies: []Reply{c.reply, c.reply, c.reply, c.reply}}
		if err := Turn(ctx, conv, box, 3, Events{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := Turn(ctx, &scripted{replies: []Reply{{Stop: StopRefusal}}}, box, 3, Events{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("отказ без пояснения: %v", err)
	}
}

func decode(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("тело запроса: %v\n%s", err, raw)
	}
	return body
}

var testTools = []Tool{{Name: "plan_script", Description: "Build a plan",
	Schema: json.RawMessage(`{"type":"object","properties":{"script":{"type":"string"}},"required":["script"],"additionalProperties":false}`)}}

// twoSteps — разговор в два хода модели: вызов инструмента, затем ответ.
func twoSteps(t *testing.T, conv Conversation) (first, second Reply) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conv.Say("Подними work_mem на server1")
	first, err := conv.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stop != StopTools || len(first.Calls) != 1 || first.Calls[0].Name != "plan_script" ||
		!strings.Contains(string(first.Calls[0].Input), "tune.ochan") {
		t.Fatalf("первый ход: %+v", first)
	}
	conv.Results([]Result{{ID: first.Calls[0].ID, Name: "plan_script", Text: "изменить work_mem: 4MB -> 32MB"}})
	second, err = conv.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Stop != StopEnd || len(second.Calls) != 0 {
		t.Fatalf("второй ход: %+v", second)
	}
	return first, second
}

// Сервер с интерфейсом OpenAI: инструменты — tools, результат — сообщение
// роли tool с номером вызова; ключ — Bearer.
func TestOpenAI(t *testing.T) {
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decode(t, r)
		step++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k-123" || body["model"] != "local-model" {
			t.Errorf("запрос: %s %s %v", r.URL.Path, r.Header.Get("Authorization"), body["model"])
		}
		messages := body["messages"].([]any)
		if step == 1 {
			tool := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
			if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || tool["name"] != "plan_script" || body["max_tokens"] != float64(900) {
				t.Errorf("первый запрос: %v", body)
			}
			fmt.Fprint(w, `{"model":"local-model","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"",`+
				`"tool_calls":[{"id":"c1","type":"function","function":{"name":"plan_script","arguments":"{\"script\":\"tune.ochan\"}"}}]}}]}`)
			return
		}
		last := messages[len(messages)-1].(map[string]any)
		if len(messages) != 4 || last["role"] != "tool" || last["tool_call_id"] != "c1" || last["content"] != "изменить work_mem: 4MB -> 32MB" {
			t.Errorf("второй запрос: %v", messages)
		}
		fmt.Fprint(w, `{"model":"local-model","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Заявка 7f оформлена."}}]}`)
	}))
	defer srv.Close()
	conv, err := New(Options{Provider: "openai", URL: srv.URL + "/v1/", Model: "local-model", Key: "k-123", MaxTokens: 900, System: "PROTOCOL", Tools: testTools})
	if err != nil {
		t.Fatal(err)
	}
	if _, second := twoSteps(t, conv); second.Text != "Заявка 7f оформлена." {
		t.Fatalf("ответ: %+v", second)
	}
	if _, err := New(Options{Provider: "openai"}); err == nil || !strings.Contains(err.Error(), "llm.chat.model") {
		t.Fatalf("без модели: %v", err)
	}
	if _, err := New(Options{Provider: "nope"}); err == nil || !strings.Contains(err.Error(), "anthropic, openai, gigachat") {
		t.Fatalf("неизвестный поставщик: %v", err)
	}
}

// GigaChat: токен по ключу авторизации — один на разговор; инструменты —
// functions, аргументы вызова — объект, результат — сообщение роли function
// с текстом JSON.
func TestGigaChat(t *testing.T) {
	tokens, step := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth" {
			tokens++
			raw, _ := io.ReadAll(r.Body)
			if r.Header.Get("Authorization") != "Basic a2V5" || len(r.Header.Get("RqUID")) != 36 || string(raw) != "scope=GIGACHAT_API_CORP" {
				t.Errorf("выдача токена: %s %s %s", r.Header.Get("Authorization"), r.Header.Get("RqUID"), raw)
			}
			fmt.Fprintf(w, `{"access_token":"tok-1","expires_at":%d}`, time.Now().Add(30*time.Minute).UnixMilli())
			return
		}
		body := decode(t, r)
		step++
		if r.URL.Path != "/api/chat/completions" || r.Header.Get("Authorization") != "Bearer tok-1" || body["model"] != "GigaChat" {
			t.Errorf("запрос: %s %s %v", r.URL.Path, r.Header.Get("Authorization"), body["model"])
		}
		messages := body["messages"].([]any)
		if step == 1 {
			if fn := body["functions"].([]any)[0].(map[string]any); fn["name"] != "plan_script" || body["function_call"] != "auto" {
				t.Errorf("функции: %v", body)
			}
			fmt.Fprint(w, `{"model":"GigaChat:2","choices":[{"finish_reason":"function_call","message":{"role":"assistant","content":"",`+
				`"function_call":{"name":"plan_script","arguments":{"script":"tune.ochan"}},"functions_state_id":"st-9"}}]}`)
			return
		}
		echo, last := messages[len(messages)-2].(map[string]any), messages[len(messages)-1].(map[string]any)
		if echo["functions_state_id"] != "st-9" || echo["function_call"].(map[string]any)["name"] != "plan_script" ||
			last["role"] != "function" || last["name"] != "plan_script" || last["content"] != `{"result":"изменить work_mem: 4MB -> 32MB"}` {
			t.Errorf("второй запрос: %v", messages)
		}
		fmt.Fprint(w, `{"model":"GigaChat:2","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Заявка оформлена."}}]}`)
	}))
	defer srv.Close()
	conv, err := New(Options{Provider: "gigachat", URL: srv.URL + "/api", AuthURL: srv.URL + "/oauth", Scope: "GIGACHAT_API_CORP", Key: "a2V5",
		System: "PROTOCOL", Tools: testTools})
	if err != nil {
		t.Fatal(err)
	}
	if _, second := twoSteps(t, conv); second.Text != "Заявка оформлена." || second.Model != "GigaChat:2" || tokens != 1 {
		t.Fatalf("ответ: %+v, токенов выдано %d", second, tokens)
	}
	if _, err := New(Options{Provider: "gigachat"}); err == nil || !strings.Contains(err.Error(), "ключ авторизации") {
		t.Fatalf("без ключа: %v", err)
	}
}

// sse — событие потока ответа Anthropic.
func sse(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

// Claude через официальный SDK: поток ответа, инструменты с потоковым
// входом, кеш неизменного начала, запасная модель при отказе по политике,
// история — с блоками размышлений модели, как пришли.
func TestClaude(t *testing.T) {
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decode(t, r)
		step++
		// Ключ SDK передаёт сам: заголовком X-Api-Key или Authorization — по версии API.
		key := r.Header.Get("X-Api-Key") + r.Header.Get("Authorization")
		if r.URL.Path != "/v1/messages" || !strings.HasSuffix(key, "sk-test") ||
			!strings.Contains(r.Header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") ||
			body["model"] != "claude-fable-5-2" || body["stream"] != true || body["fallbacks"] != "default" ||
			body["max_tokens"] != float64(claudeMaxTokens) || body["thinking"] != nil || body["tool_choice"] != nil {
			t.Errorf("запрос %d: %s beta=%q %v", step, r.URL.Path, r.Header.Get("Anthropic-Beta"), body)
		}
		tool := body["tools"].([]any)[0].(map[string]any)
		schema := tool["input_schema"].(map[string]any)
		system := body["system"].([]any)[0].(map[string]any)
		if tool["name"] != "plan_script" || tool["eager_input_streaming"] != true || schema["additionalProperties"] != false ||
			schema["type"] != "object" || system["text"] != "PROTOCOL" || system["cache_control"] == nil ||
			body["output_config"].(map[string]any)["effort"] != "high" {
			t.Errorf("инструменты и протокол: %v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		messages := body["messages"].([]any)
		if step == 1 {
			sse(w, "message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-fable-5-2","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`)
			sse(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
			sse(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`)
			sse(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
			sse(w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
			sse(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Строю план."}}`)
			sse(w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
			sse(w, "content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"plan_script","input":{}}}`)
			sse(w, "content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"script\":"}}`)
			sse(w, "content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"tune.ochan\"}"}}`)
			sse(w, "content_block_stop", `{"type":"content_block_stop","index":2}`)
			sse(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`)
			sse(w, "message_stop", `{"type":"message_stop"}`)
			return
		}
		// История: ответ модели целиком — размышления с подписью, текст,
		// вызов — и результат инструмента одним сообщением.
		raw, _ := json.Marshal(messages)
		assistant := messages[1].(map[string]any)["content"].([]any)
		result := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
		if len(messages) != 3 || len(assistant) != 3 || assistant[0].(map[string]any)["type"] != "thinking" ||
			assistant[0].(map[string]any)["signature"] != "sig-abc" || assistant[2].(map[string]any)["id"] != "toolu_1" ||
			result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" || !strings.Contains(string(raw), "изменить work_mem") {
			t.Errorf("история второго запроса: %s", raw)
		}
		sse(w, "message_start", `{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":30,"output_tokens":0}}}`)
		sse(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		sse(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Заявка оформлена."}}`)
		sse(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		sse(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}`)
		sse(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	conv, err := New(Options{Provider: "anthropic", URL: srv.URL, Model: "claude-fable-5-2", Key: "sk-test", Effort: "high", Fallbacks: true,
		System: "PROTOCOL", Tools: testTools})
	if err != nil {
		t.Fatal(err)
	}
	first, second := twoSteps(t, conv)
	if first.Text != "Строю план." || first.Calls[0].ID != "toolu_1" || first.Substitute || second.Text != "Заявка оформлена." ||
		second.Model != "claude-opus-4-8" || !second.Substitute {
		t.Fatalf("ходы: %+v / %+v", first, second)
	}
	if conv.Describe() != "Anthropic, claude-fable-5-2" {
		t.Fatalf("описание: %s", conv.Describe())
	}

	// Отказ: причина — в пояснении; ошибка API — словами.
	refusal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decode(t, r)
		if body["model"] != DefaultClaudeModel || body["fallbacks"] != nil || r.Header.Get("Anthropic-Beta") != "" {
			t.Errorf("без запасной модели: beta=%q %v", r.Header.Get("Anthropic-Beta"), body["fallbacks"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", `{"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)
		sse(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"declined"}},"usage":{"output_tokens":0}}`)
		sse(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer refusal.Close()
	conv, err = New(Options{Provider: "anthropic", URL: refusal.URL, Key: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	conv.Say("x")
	if reply, err := conv.Next(context.Background()); err != nil || reply.Stop != StopRefusal || !strings.Contains(reply.Note, "cyber") {
		t.Fatalf("отказ: %+v, %v", reply, err)
	}
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer denied.Close()
	conv, _ = New(Options{Provider: "anthropic", URL: denied.URL, Key: "bad"})
	conv.Say("x")
	if _, err := conv.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "не принял ключ (401)") {
		t.Fatalf("негодный ключ: %v", err)
	}
	if _, err := New(Options{Provider: "anthropic", Effort: "huge"}); err == nil || !strings.Contains(err.Error(), "llm.chat.effort") {
		t.Fatalf("негодная глубина: %v", err)
	}
}
