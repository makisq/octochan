package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// Версии протокола MCP, которые понимает сервер, от новой к старой.
// Сервер использует только инструменты, а они во всех этих версиях
// устроены одинаково.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const serverVersion = "0.1.0"

// Коды ошибок JSON-RPC 2.0.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// maxMessage — предел длины одного сообщения. Аргументы инструментов —
// это пути и имена, мегабайт для них с большим запасом.
const maxMessage = 1 << 20

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// server — сервер MCP поверх stdio.
type server struct {
	tools  []tool
	byName map[string]*tool
	box    *sandbox
	runner *runner
	logger *log.Logger

	outMu sync.Mutex
	out   io.Writer

	mu      sync.Mutex
	running map[string]context.CancelFunc
	calls   sync.WaitGroup

	// protocol — текст протокола формального промпта. Модель получает его
	// первым: в instructions при подключении.
	protocol string
}

func newServer(box *sandbox, run *runner, out io.Writer, logger *log.Logger) (*server, error) {
	tools := buildTools(box.liveSchemes())
	if err := checkTable(tools); err != nil {
		return nil, err
	}

	byName := make(map[string]*tool, len(tools))
	for i := range tools {
		byName[tools[i].Name] = &tools[i]
	}
	return &server{
		tools:   tools,
		byName:  byName,
		box:     box,
		runner:  run,
		logger:  logger,
		out:     out,
		running: make(map[string]context.CancelFunc),
	}, nil
}

// serve читает сообщения до конца входа.
//
// Конец входа не прерывает начатые вызовы: сервер дожидается их и
// отправляет ответы, а уже потом завершается. Иначе `echo запрос |
// ochan-mcp` не получил бы ответа никогда. Каждый вызов ограничен своим
// таймаутом, а прервать всё сразу можно сигналом — тогда отменяется ctx.
func (s *server) serve(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.calls.Wait()

	reader := bufio.NewReaderSize(in, 64*1024)
	for {
		line, tooLong, err := readLine(reader)
		switch {
		case tooLong:
			s.write(errorResponse(nil, codeInvalidRequest,
				fmt.Sprintf("сообщение длиннее %d байт отброшено", maxMessage)))
		case len(bytes.TrimSpace(line)) > 0:
			s.dispatch(ctx, line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readLine читает одно сообщение. Слишком длинное дочитывается до конца
// строки и отбрасывается целиком, чтобы не сбить разбор следующих.
func readLine(reader *bufio.Reader) ([]byte, bool, error) {
	var (
		line    []byte
		tooLong bool
	)
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxMessage {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, tooLong, err
	}
}

// utf8BOM — метка порядка байтов. Её дописывают в начало потока клиенты на
// .NET и скрипты Windows; для JSON это мусор, и без обработки первое же
// сообщение — initialize — отвергалось бы как некорректное.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// dispatch разбирает строку: одиночное сообщение или пакет.
func (s *server) dispatch(ctx context.Context, line []byte) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), utf8BOM))
	if len(trimmed) == 0 {
		return
	}

	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil || len(batch) == 0 {
			s.write(errorResponse(nil, codeInvalidRequest, "некорректный пакет JSON-RPC"))
			return
		}
		// Пакет обрабатывается целиком в одной горутине: ответы на него
		// отправляются одним массивом, когда готовы все.
		s.calls.Add(1)
		go func() {
			defer s.calls.Done()
			var responses []*response
			for _, raw := range batch {
				if r := s.handle(ctx, raw); r != nil {
					responses = append(responses, r)
				}
			}
			if len(responses) > 0 {
				s.write(responses)
			}
		}()
		return
	}

	var msg message
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		s.write(errorResponse(nil, codeParseError, "некорректный JSON: "+err.Error()))
		return
	}

	// Вызов инструмента может идти долго, поэтому выполняется отдельно:
	// пока он работает, клиент должен иметь возможность его отменить.
	if msg.Method == "tools/call" && len(msg.ID) > 0 {
		s.calls.Add(1)
		go func() {
			defer s.calls.Done()
			if r := s.handle(ctx, trimmed); r != nil {
				s.write(r)
			}
		}()
		return
	}

	if r := s.handle(ctx, trimmed); r != nil {
		s.write(r)
	}
}

// handle обрабатывает одно сообщение и возвращает ответ или nil, если
// отвечать не нужно (уведомление, отменённый вызов).
func (s *server) handle(ctx context.Context, raw json.RawMessage) *response {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return errorResponse(nil, codeParseError, "некорректный JSON: "+err.Error())
	}

	// Ответы клиента на наши запросы: мы запросов не шлём, читать нечего.
	if msg.Method == "" {
		if len(msg.ID) == 0 {
			return errorResponse(nil, codeInvalidRequest, "нет поля method")
		}
		return nil
	}

	notification := len(msg.ID) == 0
	if !notification && string(msg.ID) == "null" {
		return errorResponse(nil, codeInvalidRequest, "идентификатор запроса не может быть null")
	}

	if notification {
		s.notification(msg)
		return nil
	}

	switch msg.Method {
	case "initialize":
		return s.initialize(msg)
	case "ping":
		return result(msg.ID, map[string]any{})
	case "tools/list":
		return s.listTools(msg)
	case "tools/call":
		return s.callTool(ctx, msg)
	default:
		return errorResponse(msg.ID, codeMethodNotFound, "метод не поддерживается: "+msg.Method)
	}
}

func (s *server) notification(msg message) {
	if msg.Method != "notifications/cancelled" {
		return
	}
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return
	}

	s.mu.Lock()
	cancel, ok := s.running[idKey(params.RequestID)]
	s.mu.Unlock()
	if ok {
		s.logger.Printf("отмена вызова %s: %s", params.RequestID, params.Reason)
		cancel()
	}
}

func (s *server) initialize(msg message) *response {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	_ = json.Unmarshal(msg.Params, &params)

	// Если клиент просит знакомую версию — отвечаем ею же; иначе
	// предлагаем свою новейшую, а решает клиент.
	version := protocolVersions[0]
	for _, known := range protocolVersions {
		if params.ProtocolVersion == known {
			version = known
			break
		}
	}
	s.logger.Printf("клиент %s %s, протокол %s", params.ClientInfo.Name, params.ClientInfo.Version, version)

	return result(msg.ID, map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "ochan-mcp",
			"title":   "octochan",
			"version": serverVersion,
		},
		"instructions": s.instructions(),
	})
}

// loadProtocol берёт текст протокола у ochan.
//
// Протокол живёт в ochan, рядом с проверкой языка, а не копией здесь:
// иначе модель читала бы одно, а проверка требовала бы другого. Без
// протокола сервер работает, но говорит об этом в журнале и в instructions.
func (s *server) loadProtocol(ctx context.Context) {
	result := s.runner.run(ctx, []string{"fp", "protocol"}, "")
	if result.code != 0 || strings.TrimSpace(result.stdout) == "" {
		s.logger.Printf("протокол недоступен: ochan fp protocol завершился с кодом %d: %s",
			result.code, strings.TrimSpace(result.stderr))
		return
	}
	s.protocol = result.stdout
}

func (s *server) instructions() string {
	text := "Сервер даёт доступ к octochan на чтение, планирование и создание заявок. " +
		"Выполнить изменение через этот сервер нельзя: заявку согласует и выполняет человек " +
		"(ochan approve, затем ochan apply --request). " +
		"Файлы принимаются только внутри каталогов: " + strings.Join(s.box.roots, ", ") + "."
	if live := s.box.liveSchemes(); len(live) > 0 {
		text += " Живые источники разрешены: " + strings.Join(live, ", ") + "."
	}
	text += " Сценарии на языке алиасов: сначала check_script, затем plan_script, затем request_script. " +
		"Сценарий выполняется в песочнице: файлы только внутри тех же каталогов"
	if len(s.box.env) > 0 {
		text += ", переменные окружения только " + strings.Join(s.box.env, ", ")
	} else {
		text += ", переменные окружения недоступны"
	}
	text += "."

	if s.protocol == "" {
		return text + " Протокол формального промпта недоступен: ochan не отдал его при запуске сервера."
	}
	return text + " Прежде чем отвечать оператору, прочитайте протокол ниже: он определяет, как вы здесь работаете.\n\n" +
		s.protocol
}

func (s *server) listTools(msg message) *response {
	liveEnabled := len(s.box.liveAllow) > 0
	descriptors := make([]map[string]any, 0, len(s.tools))
	for i := range s.tools {
		descriptors = append(descriptors, s.tools[i].descriptor(liveEnabled))
	}
	return result(msg.ID, map[string]any{"tools": descriptors})
}

func (s *server) callTool(ctx context.Context, msg message) *response {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return errorResponse(msg.ID, codeInvalidParams, "некорректные параметры tools/call: "+err.Error())
	}

	t, ok := s.byName[params.Name]
	if !ok {
		return errorResponse(msg.ID, codeInvalidParams, "неизвестный инструмент: "+params.Name)
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}

	// Ошибка в аргументах — результат с isError, а не ошибка протокола:
	// так её видит модель и может исправить вызов сама.
	args, stdin, err := t.argv(s.box, params.Arguments)
	if err != nil {
		s.logger.Printf("%s: отказ: %v", t.Name, err)
		return toolResult(msg.ID, "Вызов отклонён до запуска: "+err.Error(), true)
	}

	key := idKey(msg.ID)
	callCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.running[key] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, key)
		s.mu.Unlock()
		cancel()
	}()

	started := time.Now()
	outcome := s.runner.run(callCtx, args, stdin)
	script := ""
	if stdin != "" {
		script = fmt.Sprintf(" (сценарий %d байт)", len(stdin))
	}
	s.logger.Printf("%s: ochan %s%s → код %d за %s", t.Name, maskSecrets(strings.Join(args, " ")),
		script, outcome.code, time.Since(started).Round(time.Millisecond))

	// Отменённый вызов остаётся без ответа: так требует протокол, клиент
	// его уже не ждёт.
	if errors.Is(callCtx.Err(), context.Canceled) && !outcome.timedOut {
		return nil
	}

	text, isError := s.runner.text(outcome)
	return toolResult(msg.ID, text, isError)
}

// write отправляет сообщение одной строкой.
func (s *server) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.logger.Printf("не удалось сериализовать ответ: %v", err)
		return
	}
	data = append(data, '\n')

	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(data); err != nil {
		s.logger.Printf("не удалось отправить ответ: %v", err)
	}
}

func result(id json.RawMessage, value any) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: value}
}

func toolResult(id json.RawMessage, text string, isError bool) *response {
	return result(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	})
}

func errorResponse(id json.RawMessage, code int, text string) *response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: text}}
}

// idKey приводит идентификатор к виду, пригодному для сравнения:
// 7 и 7 с пробелами вокруг — один и тот же запрос.
func idKey(id json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, id); err != nil {
		return string(id)
	}
	return b.String()
}

// maskSecrets скрывает пароль в ссылках вида scheme://user:password@host.
func maskSecrets(text string) string {
	var b strings.Builder
	for {
		start := strings.Index(text, "://")
		if start < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:start+3])
		rest := text[start+3:]

		end := strings.IndexAny(rest, " /")
		if end < 0 {
			end = len(rest)
		}
		authority := rest[:end]
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			if colon := strings.Index(authority[:at], ":"); colon >= 0 {
				authority = authority[:colon] + ":***" + authority[at:]
			}
		}
		b.WriteString(authority)
		text = rest[end:]
	}
}
