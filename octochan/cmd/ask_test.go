package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/viper"
)

// fakeToolServer — поддельный ochan-mcp для тестов разговора: тестовая
// программа, запущенная с OCHAN_TEST_AS_MCP=1 (TestMain).
func fakeToolServer() {
	in := bufio.NewReader(os.Stdin)
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
			result = map[string]any{"protocolVersion": "2025-06-18", "instructions": "PROTOCOL: propose a request, never apply."}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "requests", "description": "List requests",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"state": map[string]any{"type": "string"}}}}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": "заявок, ждущих решения: 1 (7f3a)"}}}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		fmt.Println(string(out))
	}
}

// fakeModel — сервер с интерфейсом OpenAI: на реплику со словом «заявк»
// просит инструмент requests и отвечает по его результату, на остальное —
// сразу; запоминает запросы.
type fakeModel struct {
	mu       sync.Mutex
	requests []map[string]any
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.mu.Unlock()
	messages := body["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	switch {
	case last["role"] == "tool":
		fmt.Fprintf(w, `{"model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"По данным ochan: %s"}}]}`, last["content"])
	case strings.Contains(fmt.Sprint(last["content"]), "заявк"):
		fmt.Fprint(w, `{"model":"m","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"Смотрю заявки.",`+
			`"tool_calls":[{"id":"c1","type":"function","function":{"name":"requests","arguments":"{\"state\":\"pending\"}"}}]}}]}`)
	default:
		fmt.Fprintf(w, `{"model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Реплик в разговоре: %d"}}]}`, len(messages))
	}
}

func chatEnv(t *testing.T) (*scriptEnv, *fakeModel) {
	t.Helper()
	e := newScriptEnv(t)
	model := &fakeModel{}
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)
	self, _ := os.Executable()
	t.Setenv("OCHAN_TEST_AS_MCP", "1")
	for key, value := range map[string]any{"llm.mode": "natural", "llm.chat.provider": "openai", "llm.chat.url": srv.URL,
		"llm.chat.model": "m", "llm.chat.mcp": self} {
		setting(t, key, value)
	}
	t.Cleanup(closeShellChat)
	return e, model
}

// ask — одна реплика: модель вызывает инструмент ochan и отвечает; без
// настройки — понятный отказ; своя модель ℒ разговор не ведёт.
func TestAsk(t *testing.T) {
	e, model := chatEnv(t)
	out := e.must("ask", "какие", "заявки", "ждут", "решения?")
	if out != "Смотрю заявки.\n  → requests {\"state\":\"pending\"}\nПо данным ochan: заявок, ждущих решения: 1 (7f3a)\n" {
		t.Fatalf("ask:\n%q", out)
	}
	first := model.requests[0]
	messages := first["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "PROTOCOL: propose a request, never apply." ||
		messages[1].(map[string]any)["content"] != "какие заявки ждут решения?" ||
		first["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] != "requests" {
		t.Fatalf("запрос модели: %v", first)
	}
	// ask — без истории: следующий вызов начинает разговор заново.
	if out := e.must("ask", "сколько", "реплик"); out != "Реплик в разговоре: 2\n" {
		t.Fatalf("второй ask:\n%q", out)
	}
	e.fails("opmode — команда оболочки", "opmode", "llm")

	setting(t, "llm.chat.provider", "")
	e.fails("модель для разговора не настроена", "ask", "привет")
	setting(t, "llm.chat.provider", "openai")
	setting(t, "llm.mode", "embedded")
	e.fails("своя модель знает только формальный промпт", "ask", "привет")
	setting(t, "llm.mode", "natural")

	// Право — группа ask: роли без неё разговор закрыт.
	e.must("role", "create", "reader", "--allow", "read,plan,request")
	e.must("set", "role", "reader")
	e.fails("ask", "ask", "привет")
	e.must("set", "role", "master")
}

// opmode llm в оболочке без терминала: строки — реплики с историей,
// «?» — команды ochan и инструкции языка, exmode llm — обратно.
func TestOpmodeLLM(t *testing.T) {
	e, model := chatEnv(t)
	t.Cleanup(shellReset)
	t.Cleanup(func() { setContext(rootCmd, context.Background()) })
	run := func(script string) (string, error) {
		t.Helper()
		defer setContext(rootCmd, context.Background())
		var err error
		out := captureStdout(t, func() {
			// Вывод команд оболочки — в stdout, как у настоящего ochan.
			rootCmd.SetOut(os.Stdout)
			err = runLineShell(context.Background(), strings.NewReader(script), false)
		})
		return out, err
	}
	out, err := run("opmode llm\n" +
		"привет\n" +
		"какие заявки ждут?\n" +
		"# комментарий модели не уходит\n" +
		"?print(\"из языка\");\n" +
		"?ping\n" +
		"?n = 2\n" +
		"?if n > 1 {\n  print(\"блок\")\n}\n" +
		"сколько реплик\n" +
		"exmode llm;\n" +
		"ping\n")
	if err != nil {
		t.Fatalf("оболочка: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Модель: " + viperString("llm.chat.url") + ", m; инструментов ochan — 1, роль llm, изменения — заявкой\n",
		"Режим нейросети: строка — реплика модели",
		"Реплик в разговоре: 2\n", // system + привет
		"Смотрю заявки.\n  → requests {\"state\":\"pending\"}\nПо данным ochan: заявок, ждущих решения: 1 (7f3a)\n",
		"из языка\npong\nблок\n",
		"Реплик в разговоре: 8\n", // история разговора идёт дальше
		"Режим нейросети закрыт\npong\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	if shellChat != nil || shellActive {
		t.Fatal("режим нейросети не закрыт после ввода")
	}
	for _, req := range model.requests {
		for _, m := range req["messages"].([]any) {
			if text := fmt.Sprint(m.(map[string]any)["content"]); strings.Contains(text, "комментарий") || strings.Contains(text, "print(") {
				t.Fatalf("модели ушло не то: %q", text)
			}
		}
	}

	// Конец ввода закрывает режим; exmode вне режима — не ошибка; другой
	// режим — отказ. Ошибка модели — ошибка строки.
	if out, err := run("opmode llm;\nпривет\n"); err != nil || shellChat != nil || !strings.Contains(out, "Реплик в разговоре: 2") {
		t.Fatalf("конец ввода в режиме: %v\n%s", err, out)
	}
	if out, err := run("exmode llm\n?ping\n"); err == nil || !strings.Contains(out, "Режим нейросети не включён") {
		t.Fatalf("exmode вне режима: %v\n%s", err, out)
	}
	if _, err := run("opmode gpt\n"); err == nil {
		t.Fatal("неизвестный режим принят")
	}
	setting(t, "llm.chat.url", "http://127.0.0.1:1")
	if out, err := run("opmode llm\nпривет\nexmode\nping\n"); err == nil || !strings.Contains(err.Error(), "строк с ошибкой: 1") ||
		!strings.HasSuffix(out, "Режим нейросети закрыт\npong\n") {
		t.Fatalf("недоступная модель: %v\n%s", err, out)
	}
	_ = e
}

func viperString(key string) string {
	return strings.TrimRight(viper.GetString(key), "/")
}

// Согласовать и выполнить одной командой: apply --approve.
func TestApplyApprove(t *testing.T) {
	e := newScriptEnv(t)
	created := e.must("request", "tune.ochan")
	id := requestIDPattern.FindStringSubmatch(created)[1]
	out := e.must("apply", "--approve", id)
	if !strings.Contains(out, "Заявка "+id+": согласована") || !strings.Contains(e.read("prod.conf"), "max_connections = 200") {
		t.Fatalf("apply --approve:\n%s\n%s", out, e.read("prod.conf"))
	}
	// Нет права approve — нет и apply --approve.
	created = e.must("request", "tune.ochan")
	id = requestIDPattern.FindStringSubmatch(created)[1]
	e.must("role", "create", "runner", "--allow", "read,plan,apply")
	e.must("set", "role", "runner")
	e.fails("approve", "apply", "--approve", id)
	e.must("set", "role", "master")
	e.fails("разные заявки", "apply", "--approve", id, "--request", "другая")
}
