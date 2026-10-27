package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// TestMain позволяет тестовому бинарю изображать ochan: сервер запускает
// его вместо настоящего, а тот печатает полученные аргументы. Так тесты
// видят ровно ту командную строку, которую построил сервер.
func TestMain(m *testing.M) {
	if os.Getenv("OCHAN_MCP_FAKE") == "1" {
		fakeOchan()
		return
	}
	os.Exit(m.Run())
}

func fakeOchan() {
	if marker := os.Getenv("FAKE_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte("started"), 0o600)
	}
	if pause := os.Getenv("FAKE_SLEEP"); pause != "" {
		duration, _ := time.ParseDuration(pause)
		time.Sleep(duration)
	}
	if text := os.Getenv("FAKE_STDERR"); text != "" {
		fmt.Fprint(os.Stderr, text)
	}
	if os.Getenv("FAKE_PRINT_REMOTE") == "1" {
		fmt.Println("remote=" + os.Getenv("OCTOCHAN_REMOTE_ADDR"))
	} else if os.Getenv("FAKE_PRINT_ROLE") == "1" {
		fmt.Println("role=" + os.Getenv("OCTOCHAN_ROLE"))
	} else if text := os.Getenv("FAKE_STDOUT"); text != "" {
		fmt.Print(text)
	} else if os.Getenv("FAKE_ECHO_STDIN") == "1" {
		stdin, _ := io.ReadAll(os.Stdin)
		data, _ := json.Marshal(map[string]any{"args": os.Args[1:], "stdin": string(stdin)})
		fmt.Println(string(data))
	} else {
		data, _ := json.Marshal(os.Args[1:])
		fmt.Println(string(data))
	}
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(code)
}

// syncBuffer — буфер, в который можно писать из нескольких горутин.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testEnv struct {
	t       *testing.T
	root    string
	outside string
	srv     *server
	out     *syncBuffer
}

type envOption func(*runner, *[]string)

func withFakeEnv(vars ...string) envOption {
	return func(r *runner, _ *[]string) { r.env = append(r.env, vars...) }
}

func withLive(schemes ...string) envOption {
	return func(_ *runner, live *[]string) { *live = append(*live, schemes...) }
}

func withTimeout(timeout time.Duration) envOption {
	return func(r *runner, _ *[]string) { r.timeout = timeout }
}

func withMaxOutput(limit int) envOption {
	return func(r *runner, _ *[]string) { r.maxOutput = limit }
}

// newEnv готовит каталог с файлами и сервер, запускающий поддельный ochan.
func newEnv(t *testing.T, options ...envOption) *testEnv {
	t.Helper()

	base := t.TempDir()
	root := filepath.Join(base, "root")
	for _, dir := range []string{root, filepath.Join(root, "sub")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "a.conf"):        "max_connections = 100\n",
		filepath.Join(root, "b.conf"):        "max_connections = 300\n",
		filepath.Join(root, "scenario.yaml"): "service: pangolin_restart\n",
		filepath.Join(base, "outside.conf"):  "secret = 1\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	run := &runner{
		binary:    self,
		env:       []string{"OCHAN_MCP_FAKE=1"},
		timeout:   30 * time.Second,
		maxOutput: 100_000,
	}
	var live []string
	for _, option := range options {
		option(run, &live)
	}

	box, err := newSandbox([]string{root}, live)
	if err != nil {
		t.Fatal(err)
	}
	run.dir = box.roots[0]

	out := &syncBuffer{}
	srv, err := newServer(box, run, out, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	return &testEnv{t: t, root: box.roots[0], outside: filepath.Join(base, "outside.conf"), srv: srv, out: out}
}

// real возвращает реальный путь файла внутри корня.
func (e *testEnv) real(name string) string {
	e.t.Helper()
	path, err := filepath.EvalSymlinks(filepath.Join(e.root, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return path
}

// exchange передаёт серверу вход целиком и возвращает его ответы.
// Каждый обмен начинается с чистого вывода.
func (e *testEnv) exchange(input string) []json.RawMessage {
	e.t.Helper()
	e.out = &syncBuffer{}
	e.srv.out = e.out
	if err := e.srv.serve(context.Background(), strings.NewReader(input)); err != nil {
		e.t.Fatalf("serve: %v", err)
	}
	return parseLines(e.t, e.out.String())
}

func parseLines(t *testing.T, text string) []json.RawMessage {
	t.Helper()
	var lines []json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Fatalf("сервер вывел не JSON: %q", line)
		}
		lines = append(lines, json.RawMessage(line))
	}
	return lines
}

type testResponse struct {
	ID     json.RawMessage `json:"id"`
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError         bool             `json:"isError"`
		ProtocolVersion string           `json:"protocolVersion"`
		Instructions    string           `json:"instructions"`
		Capabilities    map[string]any   `json:"capabilities"`
		Tools           []map[string]any `json:"tools"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

func decode(t *testing.T, raw json.RawMessage) testResponse {
	t.Helper()
	var r testResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("ответ не разбирается: %v\n%s", err, raw)
	}
	return r
}

func request(id any, method string, params any) string {
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	return string(data) + "\n"
}

func call(id any, name string, args map[string]any) string {
	return request(id, "tools/call", map[string]any{"name": name, "arguments": args})
}

// callTool вызывает инструмент и возвращает текст результата и признак ошибки.
func (e *testEnv) callTool(name string, args map[string]any) (string, bool) {
	e.t.Helper()
	responses := e.exchange(call(1, name, args))
	if len(responses) != 1 {
		e.t.Fatalf("ожидался один ответ, получено %d", len(responses))
	}
	r := decode(e.t, responses[0])
	if r.Error != nil {
		e.t.Fatalf("ошибка протокола вместо результата: %+v", r.Error)
	}
	if len(r.Result.Content) != 1 || r.Result.Content[0].Type != "text" {
		e.t.Fatalf("неожиданное содержимое: %+v", r.Result.Content)
	}
	return r.Result.Content[0].Text, r.Result.IsError
}

// argvOf вызывает инструмент и возвращает аргументы, с которыми был
// запущен ochan.
func (e *testEnv) argvOf(name string, args map[string]any) []string {
	e.t.Helper()
	text, isError := e.callTool(name, args)
	if isError {
		e.t.Fatalf("%s: вызов отклонён: %s", name, text)
	}
	var argv []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &argv); err != nil {
		e.t.Fatalf("ochan не запускался или вывел не аргументы: %q", text)
	}
	return argv
}

func assertArgv(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("аргументы ochan:\n  получено %q\n  ожидалось %q", got, want)
	}
}

// assertRejected проверяет, что вызов отклонён до запуска ochan.
func (e *testEnv) assertRejected(name string, args map[string]any, reason string) {
	e.t.Helper()
	text, isError := e.callTool(name, args)
	if !isError {
		e.t.Fatalf("%s %v: вызов принят, а должен быть отклонён", name, args)
	}
	if !strings.HasPrefix(text, "The call was rejected before it ran") {
		e.t.Fatalf("%s: ochan запускался, хотя не должен был: %q", name, text)
	}
	if !strings.Contains(text, reason) {
		e.t.Errorf("%s: в причине нет %q: %q", name, reason, text)
	}
}

// ---------------------------------------------------------------------------
// Главный запрет: выполнять нельзя
// ---------------------------------------------------------------------------

func TestTableOnlyReadsOrRequests(t *testing.T) {
	// Список команд, которые что-то выполняют или меняют. Ни одна из них
	// не должна быть доступна модели — ни сейчас, ни после правки таблицы.
	forbidden := []string{
		"apply", "approve", "reject", "rollback", "patch", "tee", "auth", "timer",
		"!install", "inspect", "srs", "run", "eval", "shell", "if", "else", "logs", "logs files",
	}

	// Журнал самого ochan модели не показывается: разрешена только
	// подкоманда logs rank, и только она.
	for _, bad := range []tool{{Name: "x", Command: "logs"}, {Name: "x", Command: "logs", Fixed: []string{"files"}},
		{Name: "x", Command: "logs", Fixed: []string{"--tail=5"}}} {
		if err := checkTable([]tool{bad}); err == nil {
			t.Errorf("таблица с %s %v принята", bad.Command, bad.Fixed)
		}
	}

	for _, live := range [][]string{nil, {"postgres"}} {
		for _, tool := range buildTools(live) {
			for _, bad := range forbidden {
				if commandPath(tool) == bad {
					t.Errorf("инструмент %s вызывает запрещённую команду %s", tool.Name, bad)
				}
			}
			switch {
			case readCommands[commandPath(tool)] && !tool.Writes:
			case writeCommands[commandPath(tool)] && tool.Writes:
			default:
				t.Errorf("инструмент %s: команда %s вне разрешённых списков", tool.Name, tool.Command)
			}
		}
		if err := checkTable(buildTools(live)); err != nil {
			t.Errorf("таблица не прошла собственную проверку: %v", err)
		}
	}
}

func TestCheckTableRejectsViolations(t *testing.T) {
	cases := map[string][]tool{
		"выполнение":                {{Name: "x", Command: "apply"}},
		"заявка без пометки записи": {{Name: "x", Command: "request"}},
		"чтение с пометкой записи":  {{Name: "x", Command: "diff", Writes: true}},
		"дубликат имени":            {{Name: "x", Command: "diff"}, {Name: "x", Command: "stats"}},
	}
	for name, tools := range cases {
		if err := checkTable(tools); err == nil {
			t.Errorf("%s: таблица принята", name)
		}
	}
}

func TestNewServerAcceptsOwnTable(t *testing.T) {
	// Проверка таблицы стоит на пути запуска; собственная таблица её проходит.
	box, err := newSandbox([]string{t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newServer(box, &runner{}, io.Discard, log.New(io.Discard, "", 0))
	if err != nil || srv == nil {
		t.Fatalf("исправная таблица отвергнута: %v", err)
	}
}

func TestSchemasAreClosed(t *testing.T) {
	// Схема с закрытым набором полей — то же, что словарь языка:
	// всё, чего в ней нет, отвергается.
	for _, tool := range buildTools([]string{"postgres"}) {
		schema := tool.schema()
		if schema["additionalProperties"] != false {
			t.Errorf("%s: схема допускает лишние поля", tool.Name)
		}
		properties := schema["properties"].(map[string]any)
		for _, name := range schema["required"].([]string) {
			if _, ok := properties[name]; !ok {
				t.Errorf("%s: обязательное поле %s не описано", tool.Name, name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Командные строки
// ---------------------------------------------------------------------------

func TestDiffArgv(t *testing.T) {
	e := newEnv(t)
	argv := e.argvOf("diff", map[string]any{"left": "a.conf", "right": e.real("b.conf")})
	// --fast: сравнение через сервер не пишет отчётов и записей журнала.
	assertArgv(t, argv, "diff", "--fast", "--", e.real("a.conf"), e.real("b.conf"))

	argv = e.argvOf("diff", map[string]any{"left": "a.conf", "right": "b.conf", "include_defaults": true})
	assertArgv(t, argv, "diff", "--fast", "--all", "--", e.real("a.conf"), e.real("b.conf"))
}

func TestReadToolsArgv(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"validate", map[string]any{"source": "a.conf"}, []string{"validate", "--", e.real("a.conf")}},
		{"find", map[string]any{"source": "a.conf", "param": "max_connections"},
			[]string{"find", "--", e.real("a.conf"), "max_connections"}},
		{"stats", map[string]any{"source": "a.conf"}, []string{"stats", "--", e.real("a.conf")}},
		{"print", map[string]any{"file": "a.conf"}, []string{"print", "--", e.real("a.conf")}},
		{"services", nil, []string{"services"}},
		{"modules", nil, []string{"modules"}},
		{"aliases", map[string]any{"package": "девопс"}, []string{"aliases", "--", "девопс"}},
		{"history", map[string]any{"limit": 5}, []string{"history", "--limit=5"}},
		{"history", map[string]any{"id": "ABC1"}, []string{"history", "--", "abc1"}},
		{"requests", map[string]any{"id": "3ff2"}, []string{"requests", "--", "3ff2"}},
		{"task_status", map[string]any{"task_id": "1001"}, []string{"status", "--", "1001"}},
		{"task_status", map[string]any{"task_id": "job-17", "executor": "linux"}, []string{"status", "--executor=linux", "--", "job-17"}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			sub := *e
			sub.t = t
			assertArgv(t, sub.argvOf(c.tool, c.args), c.want...)
		})
	}
}

func TestPlanAndRequestArgv(t *testing.T) {
	e := newEnv(t)

	assertArgv(t, e.argvOf("plan_config", map[string]any{"base": "a.conf", "changes": "b.conf"}),
		"plan", "--", e.real("a.conf"), e.real("b.conf"))

	// Переопределения упорядочены: одинаковый вызов — одинаковая строка,
	// а значит, одинаковый план и одинаковая заявка.
	assertArgv(t, e.argvOf("plan_scenario", map[string]any{
		"scenario":  "scenario.yaml",
		"overrides": map[string]any{"restart": "instance", "confirm": true, "batch": 2},
	}), "plan", "--", e.real("scenario.yaml"), "batch=2", "confirm=true", "restart=instance")

	doc := filepath.Join(e.root, "заявка.docx")
	assertArgv(t, e.argvOf("request_config", map[string]any{"base": "a.conf", "changes": "b.conf", "doc": "заявка.docx"}),
		"request", "--doc="+doc, "--", e.real("a.conf"), e.real("b.conf"))

	assertArgv(t, e.argvOf("request_scenario", map[string]any{"scenario": "scenario.yaml"}),
		"request", "--", e.real("scenario.yaml"))
}

// ---------------------------------------------------------------------------
// Отказы до запуска
// ---------------------------------------------------------------------------

func TestPathsOutsideRootsRejected(t *testing.T) {
	e := newEnv(t)

	e.assertRejected("print", map[string]any{"file": e.outside}, "outside the allowed directories")
	e.assertRejected("print", map[string]any{"file": "../outside.conf"}, "outside the allowed directories")
	e.assertRejected("diff", map[string]any{"left": "a.conf", "right": "../outside.conf"}, "outside the allowed directories")
	e.assertRejected("print", map[string]any{"file": "нет.conf"}, "not found")
	e.assertRejected("print", map[string]any{"file": "sub"}, "not a regular file")
}

func TestSymlinkOutOfRootRejected(t *testing.T) {
	e := newEnv(t)
	link := filepath.Join(e.root, "link.conf")
	if err := os.Symlink(e.outside, link); err != nil {
		t.Skipf("символические ссылки недоступны: %v", err)
	}
	// Ссылка лежит внутри, а ведёт наружу: проверяется место назначения.
	e.assertRejected("print", map[string]any{"file": "link.conf"}, "outside the allowed directories")
}

func TestLiveSourcesOffByDefault(t *testing.T) {
	e := newEnv(t)
	e.assertRejected("diff", map[string]any{"left": "a.conf", "right": "postgres://dba@db1/postgres"},
		"--allow-live postgres")
}

func TestLiveSourcesWhenAllowed(t *testing.T) {
	e := newEnv(t, withLive("postgres"))
	assertArgv(t, e.argvOf("diff", map[string]any{"left": "a.conf", "right": "postgres://dba@db1/postgres"}),
		"diff", "--fast", "--", e.real("a.conf"), "postgres://dba@db1/postgres")

	e.assertRejected("diff", map[string]any{"left": "a.conf", "right": "mysql://db1/x"}, "is not allowed")
}

func TestChangesWithEqualsRejected(t *testing.T) {
	// ochan принял бы такой второй аргумент за переопределение key=value
	// и молча спланировал бы сценарий вместо пары конфигов.
	e := newEnv(t, withLive("postgres"))
	if err := os.WriteFile(filepath.Join(e.root, "x=y.conf"), []byte("a = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.assertRejected("plan_config", map[string]any{"base": "a.conf", "changes": "x=y.conf"}, "override")

	e.assertRejected("request_config",
		map[string]any{"base": "a.conf", "changes": "postgres://db1/postgres?sslmode=require"}, "override")
}

func TestDocumentNeverOverwritten(t *testing.T) {
	e := newEnv(t)
	existing := filepath.Join(e.root, "старая.docx")
	if err := os.WriteFile(existing, []byte("важный документ"), 0o600); err != nil {
		t.Fatal(err)
	}

	e.assertRejected("request_config", map[string]any{"base": "a.conf", "changes": "b.conf", "doc": "старая.docx"},
		"overwriting is not allowed")

	data, _ := os.ReadFile(existing)
	if string(data) != "важный документ" {
		t.Error("существующий документ изменён")
	}

	for doc, reason := range map[string]string{
		"заявка.txt":       ".docx",
		"нет/заявка.docx":  "does not exist",
		"../заявка.docx":   "outside the allowed directories",
		"sub/../../x.docx": "outside the allowed directories",
	} {
		e.assertRejected("request_config", map[string]any{"base": "a.conf", "changes": "b.conf", "doc": doc}, reason)
	}
}

func TestSlotValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		tool   string
		args   map[string]any
		reason string
	}{
		{"diff", map[string]any{"left": "a.conf"}, "a required parameter"},
		{"diff", map[string]any{"left": "a.conf", "right": "b.conf", "command": "apply"}, "unknown parameters: command"},
		{"history", map[string]any{"limit": "5"}, "an integer"},
		{"history", map[string]any{"limit": 0}, "an integer"},
		{"history", map[string]any{"limit": 2.5}, "an integer"},
		{"history", map[string]any{"id": "../x"}, "invalid value"},
		{"find", map[string]any{"source": "a.conf", "param": "--all"}, "invalid value"},
		{"find", map[string]any{"source": "a.conf", "param": 42}, "a string expected"},
		{"task_status", map[string]any{"task_id": "1; rm -rf /"}, "invalid value"},
		{"plan_scenario", map[string]any{"scenario": "scenario.yaml", "overrides": map[string]any{"--force": "1"}}, "invalid parameter name"},
		{"plan_scenario", map[string]any{"scenario": "scenario.yaml", "overrides": []any{"a=1"}}, "an object"},
		{"plan_scenario", map[string]any{"scenario": "scenario.yaml", "overrides": map[string]any{"a": map[string]any{}}}, "a string, a number"},
	}
	for _, c := range cases {
		e.assertRejected(c.tool, c.args, c.reason)
	}
}

// ---------------------------------------------------------------------------
// Результат выполнения
// ---------------------------------------------------------------------------

func TestFailureIsToolError(t *testing.T) {
	e := newEnv(t, withFakeEnv("FAKE_EXIT=1", "FAKE_STDERR=ошибка: файл повреждён", "FAKE_STDOUT=частичный вывод"))
	text, isError := e.callTool("validate", map[string]any{"source": "a.conf"})
	if !isError {
		t.Fatal("ненулевой код возврата не помечен как ошибка")
	}
	if !strings.HasPrefix(text, "ошибка: файл повреждён") || !strings.Contains(text, "частичный вывод") {
		t.Errorf("ошибка должна идти первой, за ней вывод: %q", text)
	}
}

func TestWarningsKeptOnSuccess(t *testing.T) {
	// Замечание «параметр ждёт перезапуска» приходит в stderr, но для
	// модели оно так же важно, как сам результат.
	e := newEnv(t, withFakeEnv("FAKE_STDERR=внимание: shared_buffers ждёт перезапуска", "FAKE_STDOUT=различий нет"))
	text, isError := e.callTool("validate", map[string]any{"source": "a.conf"})
	if isError {
		t.Fatalf("успешный вызов помечен как ошибка: %q", text)
	}
	if !strings.Contains(text, "различий нет") || !strings.Contains(text, "ждёт перезапуска") {
		t.Errorf("потеряна часть вывода: %q", text)
	}
}

func TestTimeoutStopsCommand(t *testing.T) {
	e := newEnv(t, withTimeout(300*time.Millisecond), withFakeEnv("FAKE_SLEEP=30s"))

	started := time.Now()
	text, isError := e.callTool("validate", map[string]any{"source": "a.conf"})
	if !isError || !strings.Contains(text, "Time limit exceeded") {
		t.Errorf("превышение времени не сообщено: %q", text)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("команда не остановлена вовремя: %s", elapsed)
	}
}

func TestOutputLimitKeepsUTF8(t *testing.T) {
	r := &runner{maxOutput: 101}
	text := r.limit(strings.Repeat("я", 300))
	if !utf8.ValidString(text) {
		t.Error("обрезка разрезала символ пополам")
	}
	if !strings.Contains(text, "output truncated") {
		t.Error("обрезка не отмечена")
	}
	if r.limit("коротко") != "коротко" {
		t.Error("короткий вывод изменён")
	}

	// То же целиком через сервер: длинный вывод ochan доходит до модели
	// обрезанным и валидным.
	e := newEnv(t, withMaxOutput(101), withFakeEnv("FAKE_STDOUT="+strings.Repeat("я", 300)))
	text, isError := e.callTool("print", map[string]any{"file": "a.conf"})
	if isError || !utf8.ValidString(text) || !strings.Contains(text, "output truncated") {
		t.Errorf("длинный вывод не обрезан корректно: ошибка=%v, %q", isError, text)
	}
}

// ---------------------------------------------------------------------------
// Протокол
// ---------------------------------------------------------------------------

func TestInitializeNegotiatesVersion(t *testing.T) {
	for requested, want := range map[string]string{
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2024-11-05",
		"2099-01-01": protocolVersions[0],
	} {
		e := newEnv(t)
		responses := e.exchange(request(1, "initialize", map[string]any{
			"protocolVersion": requested,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1"},
		}) + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")

		// На уведомление ответа нет: строка одна.
		if len(responses) != 1 {
			t.Fatalf("ожидался один ответ, получено %d", len(responses))
		}
		r := decode(t, responses[0])
		if r.Result.ProtocolVersion != want {
			t.Errorf("запрошена %s: ответ %s, ожидалась %s", requested, r.Result.ProtocolVersion, want)
		}
		if _, ok := r.Result.Capabilities["tools"]; !ok {
			t.Error("не объявлена поддержка инструментов")
		}
		if !strings.Contains(r.Result.Instructions, "No change can be carried out") {
			t.Errorf("в инструкциях нет запрета на выполнение: %q", r.Result.Instructions)
		}
	}
}

func TestToolsList(t *testing.T) {
	e := newEnv(t)
	r := decode(t, e.exchange(request(1, "tools/list", map[string]any{}))[0])

	names := make(map[string]bool)
	for _, descriptor := range r.Result.Tools {
		names[descriptor["name"].(string)] = true
		annotations := descriptor["annotations"].(map[string]any)
		writes := strings.HasPrefix(descriptor["name"].(string), "request_")
		if annotations["readOnlyHint"] == writes {
			t.Errorf("%s: readOnlyHint не соответствует назначению", descriptor["name"])
		}
	}
	for _, want := range []string{"diff", "plan_config", "plan_scenario", "request_config", "request_scenario", "history"} {
		if !names[want] {
			t.Errorf("в списке нет инструмента %s", want)
		}
	}
	if len(r.Result.Tools) != len(buildTools(nil)) {
		t.Errorf("инструментов %d, ожидалось %d", len(r.Result.Tools), len(buildTools(nil)))
	}
}

func TestProtocolErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
		code  int
	}{
		{"неизвестный метод", request(1, "resources/list", nil), codeMethodNotFound},
		{"неизвестный инструмент", call(1, "apply", map[string]any{}), codeInvalidParams},
		{"битый JSON", "{не json\n", codeParseError},
		{"null вместо id", `{"jsonrpc":"2.0","id":null,"method":"ping"}` + "\n", codeInvalidRequest},
		{"пустой пакет", "[]\n", codeInvalidRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			responses := e.exchange(c.input)
			if len(responses) != 1 {
				t.Fatalf("ожидался один ответ, получено %d", len(responses))
			}
			r := decode(t, responses[0])
			if r.Error == nil || r.Error.Code != c.code {
				t.Errorf("ожидалась ошибка %d, получено %+v", c.code, r.Error)
			}
		})
	}
}

func TestPingAndBatch(t *testing.T) {
	e := newEnv(t)
	responses := e.exchange(request(1, "ping", nil) +
		`[{"jsonrpc":"2.0","id":2,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":3,"method":"ping"}]` + "\n")

	if len(responses) != 2 {
		t.Fatalf("ожидалось два ответа (одиночный и пакет), получено %d", len(responses))
	}
	var batch []json.RawMessage
	for _, raw := range responses {
		if raw[0] == '[' {
			if err := json.Unmarshal(raw, &batch); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(batch) != 2 {
		t.Errorf("в ответе на пакет %d элементов, ожидалось 2 (уведомление без ответа)", len(batch))
	}
}

func TestLeadingBOMIgnored(t *testing.T) {
	// .NET открывает stdin дочернего процесса с меткой порядка байтов.
	// Найдено прогоном с настоящим клиентом: initialize терялся молча.
	e := newEnv(t)
	responses := e.exchange("\xef\xbb\xbf" + request(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "dotnet", "version": "1"},
	}))
	if len(responses) != 1 {
		t.Fatalf("ожидался один ответ, получено %d", len(responses))
	}
	if r := decode(t, responses[0]); r.Error != nil || r.Result.ProtocolVersion == "" {
		t.Errorf("initialize с BOM не обработан: %s", responses[0])
	}
}

func TestOversizedMessageSkipped(t *testing.T) {
	// Слишком длинное сообщение отбрасывается целиком, и следующее после
	// него разбирается как обычно.
	e := newEnv(t)
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", maxMessage) + `"}}` + "\n"
	responses := e.exchange(huge + request(2, "ping", nil))

	if len(responses) != 2 {
		t.Fatalf("ожидалось два ответа, получено %d", len(responses))
	}
	if r := decode(t, responses[0]); r.Error == nil || r.Error.Code != codeInvalidRequest {
		t.Errorf("длинное сообщение не отвергнуто: %+v", r.Error)
	}
	if r := decode(t, responses[1]); r.Error != nil || string(r.ID) != "2" {
		t.Errorf("следующее сообщение не обработано: %s", responses[1])
	}
}

func TestCancellation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	e := newEnv(t, withFakeEnv("FAKE_SLEEP=30s", "FAKE_MARKER="+marker))

	in, writer := io.Pipe()
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- e.srv.serve(context.Background(), in) }()

	fmt.Fprint(writer, call(7, "validate", map[string]any{"source": "a.conf"}))

	// Отменяем только после того, как команда действительно запустилась.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("поддельный ochan не запустился")
		}
		time.Sleep(20 * time.Millisecond)
	}

	fmt.Fprint(writer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"передумал"}}`+"\n")
	fmt.Fprint(writer, request(8, "ping", nil))
	writer.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("сервер не завершился после отмены")
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Errorf("отмена не остановила команду: %s", elapsed)
	}

	// На отменённый вызов ответа нет, на ping — есть.
	responses := parseLines(t, e.out.String())
	if len(responses) != 1 || string(decode(t, responses[0]).ID) != "8" {
		t.Errorf("ожидался только ответ на ping: %s", e.out.String())
	}
}

func TestMaskSecrets(t *testing.T) {
	cases := map[string]string{
		"diff -- a.conf postgres://dba:s3cret@db1:5432/postgres": "diff -- a.conf postgres://dba:***@db1:5432/postgres",
		"postgres://dba@db1/postgres":                            "postgres://dba@db1/postgres",
		"нет ссылок":                                             "нет ссылок",
	}
	for input, want := range cases {
		if got := maskSecrets(input); got != want {
			t.Errorf("maskSecrets(%q) = %q, ожидалось %q", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Сценарии на языке алиасов
// ---------------------------------------------------------------------------

// scriptCall вызывает инструмент со сценарием и возвращает аргументы ochan
// и то, что он получил на stdin.
func (e *testEnv) scriptCall(name string, args map[string]any) ([]string, string) {
	e.t.Helper()
	text, isError := e.callTool(name, args)
	if isError {
		e.t.Fatalf("%s: вызов отклонён: %s", name, text)
	}
	var got struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &got); err != nil {
		e.t.Fatalf("ochan не запускался или вывел не то: %q", text)
	}
	return got.Args, got.Stdin
}

func TestScriptToolsArgv(t *testing.T) {
	e := newEnv(t, withFakeEnv("FAKE_ECHO_STDIN=1"))
	script := "c = config(\"a.conf\")\nprint(c[\"max_connections\"])\n"

	// Текст сценария идёт на stdin, а в командной строке только «-»:
	// её видно в списке процессов, и сценарию там не место.
	args, stdin := e.scriptCall("check_script", map[string]any{"script": script})
	assertArgv(t, args, "check", "--", "-")
	if stdin != script {
		t.Errorf("check: на stdin %q", stdin)
	}

	// plan и request принимают ещё промпт и отчёт, поэтому stdin у них —
	// JSON-объект --bundle, даже если текст один.
	args, stdin = e.scriptCall("plan_script", map[string]any{"script": script})
	assertArgv(t, args, "plan", "--script", "--bundle", "--sandbox", "--root="+e.root, "--", "-")
	assertBundle(t, stdin, map[string]string{"script": script})

	doc := filepath.Join(e.root, "заявка.docx")
	args, stdin = e.scriptCall("request_script", map[string]any{
		"script": script, "name": "поднять_лимит.ochan", "doc": "заявка.docx"})
	assertArgv(t, args, "request", "--script", "--bundle", "--sandbox", "--root="+e.root,
		"--name=поднять_лимит.ochan", "--doc="+doc, "--", "-")
	assertBundle(t, stdin, map[string]string{"script": script})
}

func assertBundle(t *testing.T, stdin string, want map[string]string) {
	t.Helper()
	var got map[string]string
	if err := json.Unmarshal([]byte(stdin), &got); err != nil {
		t.Fatalf("на stdin не JSON: %q", stdin)
	}
	if len(got) != len(want) {
		t.Fatalf("на stdin %v, ожидалось %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("на stdin %s = %q, ожидалось %q", key, got[key], value)
		}
	}
}

// Сценарий, промпт и отчёт приходят ochan одним объектом: stdin один.
func TestScriptToolsCarryPromptAndReport(t *testing.T) {
	e := newEnv(t, withFakeEnv("FAKE_ECHO_STDIN=1"))
	script := "print(1)\n"
	prompt := "goal: tuned\nhard forbid save(except = {prod_conf})\n"
	report := "report:\n  q: limit\n    choice: high\n    basis: not_in_prompt\n"

	for _, name := range []string{"plan_script", "request_script"} {
		args, stdin := e.scriptCall(name, map[string]any{"script": script, "prompt": prompt, "report": report})
		if args[len(args)-1] != "-" || args[len(args)-2] != "--" {
			t.Errorf("%s: позиционно ожидался один «-»: %q", name, args)
		}
		assertBundle(t, stdin, map[string]string{"script": script, "prompt": prompt, "report": report})
	}

	e.assertRejected("plan_script", map[string]any{"script": script, "prompt": ""}, "prompt: empty text")
	e.assertRejected("plan_script", map[string]any{"script": script, "report": 1}, "report: text expected as a string")
	e.assertRejected("plan_script", map[string]any{"prompt": prompt}, "script: a required parameter is missing")
}

// Песочница ochan получает ровно то, что разрешено серверу: те же
// каталоги, те же схемы, те же переменные окружения.
func TestScriptSandboxMirrorsServer(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	box, err := newSandbox([]string{first, second}, []string{"postgres"}, "PGPASSWORD", "APP_ENV")
	if err != nil {
		t.Fatal(err)
	}
	if err := box.allowNet([]string{"Status.Example.com", "10.0.0.5:8080"}); err != nil {
		t.Fatal(err)
	}

	tools := buildTools(box.liveSchemes())
	byName := make(map[string]*tool)
	for i := range tools {
		byName[tools[i].Name] = &tools[i]
	}

	args, _, err := byName["plan_script"].argv(box, map[string]any{"script": "print(1)"})
	if err != nil {
		t.Fatal(err)
	}
	assertArgv(t, args, "plan", "--script", "--bundle", "--sandbox",
		"--root="+box.roots[0], "--root="+box.roots[1],
		"--allow-live=postgres", "--allow-env=APP_ENV", "--allow-env=PGPASSWORD",
		"--allow-net=10.0.0.5:8080", "--allow-net=status.example.com", "--", "-")

	for _, bad := range []string{"https://example.com", "example.com/path", "user@example.com", "-x", "a b"} {
		if err := box.allowNet([]string{bad}); err == nil {
			t.Errorf("хост %q принят", bad)
		}
	}

	// check только читает текст: песочница ему не нужна.
	args, _, err = byName["check_script"].argv(box, map[string]any{"script": "print(1)"})
	if err != nil {
		t.Fatal(err)
	}
	assertArgv(t, args, "check", "--", "-")
}

func TestScriptValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		value  any
		reason string
	}{
		{"", "empty text"},
		{"  \n\t", "empty text"},
		{42, "text expected as a string"},
		{"print(1)\x00", "invalid characters"},
		{strings.Repeat("#", maxScript+1), "longer than"},
	}
	for _, tc := range cases {
		e.assertRejected("plan_script", map[string]any{"script": tc.value}, tc.reason)
	}

	// Через JSON некорректный UTF-8 не приходит — декодер заменяет его
	// на U+FFFD, — поэтому проверка вызывается напрямую.
	if _, err := scriptText("script", "\xff\xfe"); err == nil || !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("некорректный UTF-8 принят: %v", err)
	}
	e.assertRejected("plan_script", map[string]any{}, "a required parameter is missing")
	for _, name := range []string{"../x.ochan", "a/b", "-x", "имя с пробелом"} {
		e.assertRejected("plan_script", map[string]any{"script": "print(1)", "name": name}, "invalid value")
	}
	e.assertRejected("plan_script", map[string]any{"script": "print(1)", "root": "/"}, "unknown parameters: root")
}

func TestScriptToolsRequireSandbox(t *testing.T) {
	cases := map[string]tool{
		"план без песочницы": {Name: "x", Command: "plan", Fixed: []string{"--script"},
			Slots: []slot{scriptSlot()}},
		"заявка без --script": {Name: "x", Command: "request", Writes: true, Sandboxed: true,
			Slots: []slot{scriptSlot()}},
		"сценарий в чужой команде": {Name: "x", Command: "diff", Fixed: []string{"--script"}, Sandboxed: true,
			Slots: []slot{scriptSlot()}},
		"check с выполнением": {Name: "x", Command: "check", Fixed: []string{"--script"},
			Slots: []slot{scriptSlot()}},
		"песочница без сценария": {Name: "x", Command: "plan", Sandboxed: true},
	}
	for name, bad := range cases {
		if err := checkTable([]tool{bad}); err == nil {
			t.Errorf("%s: таблица принята", name)
		}
	}
}

func TestSandboxRejectsBadEnvName(t *testing.T) {
	for _, name := range []string{"A=B", "1ABC", "A B", "-x"} {
		if _, err := newSandbox([]string{t.TempDir()}, nil, name); err == nil {
			t.Errorf("переменная %q принята", name)
		}
	}
}

func TestInstructionsDescribeScriptSandbox(t *testing.T) {
	e := newEnv(t)
	r := decode(t, e.exchange(request(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"}))[0])
	for _, want := range []string{"check_script", "plan_script", "request_script", "no environment variables"} {
		if !strings.Contains(r.Result.Instructions, want) {
			t.Errorf("в инструкциях нет %q: %s", want, r.Result.Instructions)
		}
	}
}

// ---------------------------------------------------------------------------
// Протокол и формальный промпт
// ---------------------------------------------------------------------------

func TestPromptToolsArgv(t *testing.T) {
	e := newEnv(t, withFakeEnv("FAKE_ECHO_STDIN=1"))
	prompt := "goal: tuned\nclasses:\n  State : \"tuned\"\n"

	args, stdin := e.scriptCall("check_prompt", map[string]any{"prompt": prompt, "name": "задача.fp"})
	assertArgv(t, args, "fp", "check", "--name=задача.fp", "--", "-")
	if stdin != prompt {
		t.Errorf("на stdin %q", stdin)
	}

	args, stdin = e.scriptCall("protocol", map[string]any{})
	assertArgv(t, args, "fp", "protocol")
	if stdin != "" {
		t.Errorf("протоколу stdin не нужен, получено %q", stdin)
	}

	e.assertRejected("check_prompt", map[string]any{"prompt": ""}, "empty text")

	// Словарь языка: модель смотрит, какие слова есть, прежде чем писать
	// отчёт, — новых слов в нём быть не может.
	args, _ = e.scriptCall("vocabulary", map[string]any{"skill": "pgconf", "word": "limit"})
	assertArgv(t, args, "fp", "words", "--skill=pgconf", "--", "limit")
	args, _ = e.scriptCall("vocabulary", map[string]any{})
	assertArgv(t, args, "fp", "words")
	e.assertRejected("vocabulary", map[string]any{"word": "-rf"}, "")
}

// Модель получает протокол первым — в instructions при подключении.
func TestInstructionsCarryProtocol(t *testing.T) {
	e := newEnv(t, withFakeEnv("FAKE_STDOUT=# Протокол ochan\nПервым делом check_prompt.\n"))
	e.srv.loadProtocol(context.Background())

	r := decode(t, e.exchange(request(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"}))[0])
	if !strings.Contains(r.Result.Instructions, "read the protocol below") ||
		!strings.HasSuffix(r.Result.Instructions, "# Протокол ochan\nПервым делом check_prompt.\n") {
		t.Fatalf("протокола нет в instructions: %q", r.Result.Instructions)
	}
}

func TestMissingProtocolIsSaid(t *testing.T) {
	e := newEnv(t, withFakeEnv("FAKE_EXIT=1", "FAKE_STDERR=unknown command fp"))
	e.srv.loadProtocol(context.Background())

	r := decode(t, e.exchange(request(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"}))[0])
	if !strings.Contains(r.Result.Instructions, "The protocol is unavailable") {
		t.Fatalf("о недоступном протоколе не сказано: %q", r.Result.Instructions)
	}
}

func TestOneStdinSlotPerTool(t *testing.T) {
	script := func(fixed []string, bundle bool, slots ...slot) tool {
		return tool{Name: "x", Command: "plan", Fixed: append([]string{"--script"}, fixed...),
			Sandboxed: true, Bundle: bundle, Slots: append([]slot{scriptSlot()}, slots...)}
	}
	cases := map[string]tool{
		"два текста без --bundle": {Name: "x", Command: "fp", Fixed: []string{"check"},
			Slots: []slot{promptSlot(), {Name: "other", Kind: slotText}}},
		"сценарий и промпт без --bundle": script(nil, false, scriptPromptSlot()),
		"Bundle без флага":               script(nil, true, scriptPromptSlot()),
		"флаг без Bundle":                script([]string{"--bundle"}, false),
		"ключ, которого ochan не знает":  script([]string{"--bundle"}, true, slot{Name: "notes", Kind: slotText}),
		"--bundle без сценария": {Name: "x", Command: "fp", Fixed: []string{"check", "--bundle"}, Bundle: true,
			Slots: []slot{promptSlot()}},
	}
	for name, bad := range cases {
		if err := checkTable([]tool{bad}); err == nil {
			t.Errorf("%s: таблица принята", name)
		}
	}
	if err := checkTable([]tool{script([]string{"--bundle"}, true, scriptPromptSlot(), reportSlot())}); err != nil {
		t.Fatalf("правильный инструмент отвергнут: %v", err)
	}
}

// ochan под сервером работает в роли llm, даже если в окружении сервера
// задана другая роль.
func TestOchanRunsAsLLM(t *testing.T) {
	t.Setenv("OCTOCHAN_ROLE", "master")
	e := newEnv(t, withFakeEnv("FAKE_PRINT_ROLE=1"))
	text, isError := e.callTool("services", map[string]any{})
	if isError || strings.TrimSpace(text) != "role=llm" {
		t.Fatalf("ochan запущен не в роли llm: %q", text)
	}
}

// rank_logs ранжирует журнал внутри разрешённых каталогов — и ничего
// больше: файл проверяется песочницей, уровень — шаблоном.
func TestRankLogsArgv(t *testing.T) {
	e := newEnv(t, withFakeEnv("FAKE_ECHO_STDIN=1"))
	if err := os.WriteFile(filepath.Join(e.root, "pg.log"), []byte("ERROR: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ := e.scriptCall("rank_logs", map[string]any{"file": "pg.log", "min": "error", "summary": true, "top": 3})
	assertArgv(t, args, "logs", "rank", "--min=error", "--summary", "--top=3", "--", filepath.Join(e.root, "pg.log"))

	// Существующий файл вне корня: /etc/passwd есть не везде, и на Windows
	// отказ приходил раньше — «файл не найден», а не «вне каталогов».
	e.assertRejected("rank_logs", map[string]any{"file": e.outside}, "outside the allowed directories")
	e.assertRejected("rank_logs", map[string]any{"file": "pg.log", "min": "--tail"}, "invalid value")
}

// Заголовки и описания инструментов и параметров читает модель, поэтому
// они, как и протокол, по-английски — при любых настройках сервера.
func TestToolDescriptionsAreEnglish(t *testing.T) {
	cyrillic := regexp.MustCompile(`[\p{Cyrillic}]`)
	for _, live := range [][]string{nil, {"postgres"}} {
		for _, tool := range buildTools(live) {
			texts := []string{tool.Title, tool.Description}
			for _, s := range tool.Slots {
				texts = append(texts, s.Description)
			}
			for _, text := range texts {
				if text == "" || cyrillic.MatchString(text) {
					t.Errorf("инструмент %s: описание пустое или не на английском: %q", tool.Name, text)
				}
			}
		}
	}
}

// Песочница нейросети не накрывает рабочий каталог ochan: там роли и
// ключ сервера.
func TestSandboxRefusesOchanDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".octochan"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox([]string{home}, nil); err == nil || !strings.Contains(err.Error(), "рабочий каталог ochan") {
		t.Fatalf("корень поверх ~/.octochan принят: %v", err)
	}
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox([]string{project}, nil); err != nil {
		t.Fatalf("каталог проекта отвергнут: %v", err)
	}
}
