package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// MCP — инструменты ochan через ochan-mcp: сервер запускается рядом и
// говорит по stdio (JSON-RPC, сообщение на строку). Через него у модели те
// же границы, что у любого клиента MCP: каталог --root, роль llm,
// песочница, закрытая таблица инструментов.
type MCP struct {
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	lines        chan []byte
	readErr      error
	mu           sync.Mutex
	next         int
	instructions string
	tools        []Tool
	stderr       *tailBuffer
}

// tailBuffer — конец журнала сервера: в сообщение об ошибке.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.data = append(t.data, p...); len(t.data) > 2000 {
		t.data = t.data[len(t.data)-2000:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.data))
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// StartMCP запускает сервер MCP и знакомится с ним: протокол работы и
// таблица инструментов.
func StartMCP(ctx context.Context, binary string, args, env []string, client, version string) (*MCP, error) {
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	m := &MCP{cmd: cmd, stdin: stdin, lines: make(chan []byte, 16), stderr: &tailBuffer{}}
	cmd.Stderr = m.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("сервер инструментов %s не запустился: %w", binary, err)
	}
	go func() {
		reader := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := reader.ReadBytes('\n')
			if len(strings.TrimSpace(string(line))) > 0 {
				m.lines <- line
			}
			if err != nil {
				m.readErr = err
				close(m.lines)
				return
			}
		}
	}()

	var hello struct {
		Instructions string `json:"instructions"`
	}
	if err := m.request(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": client, "version": version}}, &hello); err != nil {
		m.Close()
		return nil, err
	}
	m.instructions = hello.Instructions
	if err := m.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		m.Close()
		return nil, err
	}
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := m.request(ctx, "tools/list", map[string]any{}, &list); err != nil {
		m.Close()
		return nil, err
	}
	for _, t := range list.Tools {
		m.tools = append(m.tools, Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	return m, nil
}

func (m *MCP) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = m.stdin.Write(append(data, '\n'))
	return err
}

// request — запрос и ответ на него; уведомления сервера между ними
// пропускаются.
func (m *MCP) request(ctx context.Context, method string, params, out any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	id := m.next
	if err := m.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return m.failed(err)
	}
	want := fmt.Sprint(id)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-m.lines:
			if !ok {
				return m.failed(m.readErr)
			}
			var resp rpcResponse
			if json.Unmarshal(line, &resp) != nil || strings.TrimSpace(string(resp.ID)) != want {
				continue
			}
			if resp.Error != nil {
				return fmt.Errorf("сервер инструментов: %s", resp.Error.Message)
			}
			if out == nil {
				return nil
			}
			return json.Unmarshal(resp.Result, out)
		}
	}
}

// failed — сервер пропал: к ошибке приписывается конец его журнала.
func (m *MCP) failed(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		err = errors.New("сервер инструментов закрыл соединение")
	}
	if log := m.stderr.String(); log != "" {
		return fmt.Errorf("%w: %s", err, log)
	}
	return err
}

// Instructions — протокол работы, который сервер даёт модели первым.
func (m *MCP) Instructions() string { return m.instructions }

// Tools — таблица инструментов сервера.
func (m *MCP) Tools() []Tool { return m.tools }

// Call вызывает инструмент: текст результата и признак ошибки инструмента.
func (m *MCP) Call(ctx context.Context, name string, input json.RawMessage) (string, bool, error) {
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := m.request(ctx, "tools/call", map[string]any{"name": name, "arguments": input}, &res); err != nil {
		// Ошибка протокола (нет такого инструмента, негодные аргументы) —
		// модели, чтобы она поправила вызов; пропавший сервер — оператору.
		if strings.HasPrefix(err.Error(), "сервер инструментов: ") {
			return err.Error(), true, nil
		}
		return "", false, err
	}
	var text []string
	for _, c := range res.Content {
		if c.Type == "text" {
			text = append(text, c.Text)
		}
	}
	return strings.Join(text, "\n"), res.IsError, nil
}

// Close останавливает сервер.
func (m *MCP) Close() error {
	_ = m.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- m.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = m.cmd.Process.Kill()
		<-done
	}
	return nil
}
