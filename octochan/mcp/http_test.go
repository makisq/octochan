package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

type httpEnv struct {
	*testEnv
	transport *httpTransport
	url       string
}

func newHTTPEnv(t *testing.T, origins ...string) *httpEnv {
	t.Helper()
	e := newEnv(t)
	transport := newHTTPTransport(e.srv, "/mcp", testToken, origins, time.Hour, log.New(io.Discard, "", 0))
	server := httptest.NewServer(transport)
	t.Cleanup(server.Close)
	return &httpEnv{testEnv: e, transport: transport, url: server.URL + "/mcp"}
}

// post отправляет тело и возвращает ответ; headers — пары имя, значение.
func (h *httpEnv) post(body string, headers ...string) (*http.Response, string) {
	h.t.Helper()
	return h.send(http.MethodPost, h.url, body, append([]string{"Content-Type", "application/json"}, headers...)...)
}

func (h *httpEnv) send(method, url, body string, headers ...string) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"test","version":"1"}}}`

func (h *httpEnv) session() string {
	h.t.Helper()
	resp, body := h.post(initBody, "Authorization", "Bearer "+testToken)
	id := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != http.StatusOK || id == "" || !strings.Contains(body, `"protocolVersion":"2025-06-18"`) {
		h.t.Fatalf("initialize: %d %q %s", resp.StatusCode, id, body)
	}
	return id
}

func TestHTTPRequiresToken(t *testing.T) {
	h := newHTTPEnv(t)
	for name, headers := range map[string][]string{
		"без токена":      nil,
		"чужой токен":     {"Authorization", "Bearer " + strings.Repeat("0", 48)},
		"не Bearer":       {"Authorization", "Basic " + testToken},
		"токен в запросе": {"X-Token", testToken},
	} {
		resp, body := h.post(initBody, headers...)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	// Клиентам без заголовков — токен в пути.
	if resp, body := h.send(http.MethodPost, h.url+"/"+testToken, initBody, "Content-Type", "application/json"); resp.StatusCode != http.StatusOK {
		t.Fatalf("токен в пути: %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.send(http.MethodPost, h.url+"/nope", initBody, "Content-Type", "application/json"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("чужой токен в пути: %d", resp.StatusCode)
	}
	if resp, _ := h.send(http.MethodPost, h.url+"/"+testToken+"/x", initBody, "Content-Type", "application/json"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("лишний сегмент пути: %d", resp.StatusCode)
	}
}

func TestHTTPSessionsAndTools(t *testing.T) {
	h := newHTTPEnv(t)
	auth := []string{"Authorization", "Bearer " + testToken}
	sid := h.session()

	if resp, body := h.post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, auth...); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, "Mcp-Session-Id") {
		t.Fatalf("без сессии: %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, append(auth, "Mcp-Session-Id", "nope")...); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("чужая сессия: %d", resp.StatusCode)
	}

	with := append(auth, "Mcp-Session-Id", sid, "MCP-Protocol-Version", "2025-06-18")
	if resp, body := h.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, with...); resp.StatusCode != http.StatusAccepted || body != "" {
		t.Fatalf("уведомление: %d %q", resp.StatusCode, body)
	}

	resp, body := h.post(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, with...)
	var list struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || len(list.Result.Tools) == 0 {
		t.Fatalf("tools/list: %d %s", resp.StatusCode, body)
	}

	// Вызов идёт тем же путём, что по stdio: ochan в роли llm, и ochan
	// знает адрес клиента — для журнала аудита.
	t.Setenv("FAKE_PRINT_REMOTE", "1")
	resp, body = h.post(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"diff","arguments":{"left":"a.conf","right":"b.conf"}}}`, with...)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "remote=mcp 127.0.0.1") {
		t.Fatalf("tools/call: %d %s", resp.StatusCode, body)
	}
	resp, body = h.post(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"print","arguments":{"file":"`+filepath.ToSlash(h.outside)+`"}}}`, with...)
	if !strings.Contains(body, "outside the allowed directories") {
		t.Fatalf("файл вне корня: %d %s", resp.StatusCode, body)
	}

	// Пакет: ответы массивом.
	resp, body = h.post(`[{"jsonrpc":"2.0","id":6,"method":"ping"},{"jsonrpc":"2.0","id":7,"method":"ping"}]`, with...)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "[") || strings.Count(body, `"id"`) != 2 {
		t.Fatalf("пакет: %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.post(`[`+initBody+`,{"jsonrpc":"2.0","id":8,"method":"ping"}]`, auth...); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("initialize в пакете: %d", resp.StatusCode)
	}

	if resp, _ := h.send(http.MethodDelete, h.url, "", with...); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	if resp, _ := h.post(`{"jsonrpc":"2.0","id":9,"method":"ping"}`, with...); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("после DELETE: %d", resp.StatusCode)
	}
}

func TestHTTPRejectsBadRequests(t *testing.T) {
	h := newHTTPEnv(t, "https://claude.ai")
	auth := []string{"Authorization", "Bearer " + testToken}
	sid := h.session()
	with := append(auth, "Mcp-Session-Id", sid)

	cases := []struct {
		name    string
		method  string
		body    string
		headers []string
		status  int
	}{
		{"чужой Origin", http.MethodPost, initBody, append(auth, "Origin", "https://evil.example"), http.StatusForbidden},
		{"форма вместо JSON", http.MethodPost, initBody, append(auth, "Content-Type", "application/x-www-form-urlencoded"), http.StatusUnsupportedMediaType},
		{"неизвестная версия", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
			append(with, "Content-Type", "application/json", "MCP-Protocol-Version", "1999-01-01"), http.StatusBadRequest},
		{"слишком длинное", http.MethodPost, `{"x":"` + strings.Repeat("a", maxMessage) + `"}`, append(with, "Content-Type", "application/json"), http.StatusRequestEntityTooLarge},
		{"GET", http.MethodGet, "", with, http.StatusMethodNotAllowed},
		{"пустое тело", http.MethodPost, " ", append(with, "Content-Type", "application/json"), http.StatusBadRequest},
	}
	for _, c := range cases {
		resp, body := h.send(c.method, h.url, c.body, c.headers...)
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d, ожидалось %d: %s", c.name, resp.StatusCode, c.status, body)
		}
	}
	if resp, _ := h.post(initBody, append(auth, "Origin", "https://claude.ai")...); resp.StatusCode != http.StatusOK {
		t.Fatalf("разрешённый Origin: %d", resp.StatusCode)
	}
}

func TestHTTPSessionsExpire(t *testing.T) {
	h := newHTTPEnv(t)
	now := time.Now()
	h.transport.now = func() time.Time { return now }
	sid := h.session()
	with := []string{"Authorization", "Bearer " + testToken, "Mcp-Session-Id", sid}
	now = now.Add(59 * time.Minute)
	if resp, _ := h.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, with...); resp.StatusCode != http.StatusOK {
		t.Fatalf("живая сессия: %d", resp.StatusCode)
	}
	now = now.Add(61 * time.Minute)
	if resp, _ := h.post(`{"jsonrpc":"2.0","id":2,"method":"ping"}`, with...); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("простаивающая сессия жива: %d", resp.StatusCode)
	}

	h.transport.maxSessions = 1
	h.session()
	if resp, _ := h.post(initBody, "Authorization", "Bearer "+testToken); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("предел сессий: %d", resp.StatusCode)
	}
}

// Отмена вызова одним клиентом не трогает вызов другого с тем же id.
func TestCallKeysAreSessionScoped(t *testing.T) {
	id := json.RawMessage(`7`)
	a := callKey(withSession(context.Background(), "a"), id)
	b := callKey(withSession(context.Background(), "b"), id)
	if a == b {
		t.Fatal("ключи вызовов разных сессий совпали")
	}
}

func TestHTTPRefusesPlaintextOnNetwork(t *testing.T) {
	e := newEnv(t)
	t.Setenv(envToken, testToken)
	err := serveHTTP(context.Background(), e.srv, httpOptions{listen: "0.0.0.0:0", path: "/mcp"}, time.Minute,
		log.New(io.Discard, "", 0))
	if err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("сетевой адрес без TLS: %v", err)
	}
	err = serveHTTP(context.Background(), e.srv, httpOptions{listen: "127.0.0.1:0", tlsCert: "x"}, time.Minute,
		log.New(io.Discard, "", 0))
	if err == nil || !strings.Contains(err.Error(), "--tls-key") {
		t.Fatalf("сертификат без ключа: %v", err)
	}
}

func TestTokenFile(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	path := filepath.Join(t.TempDir(), "conf", "token")
	t.Setenv(envToken, "")

	first, err := loadToken(path, logger)
	if err != nil || len(first) != 64 {
		t.Fatalf("новый токен: %q %v", first, err)
	}
	info, err := os.Stat(path)
	if err != nil || (os.PathSeparator == '/' && info.Mode().Perm() != 0o600) {
		t.Fatalf("права файла токена: %v %v", info.Mode(), err)
	}
	if again, _ := loadToken(path, logger); again != first {
		t.Fatal("токен сменился при повторном чтении")
	}
	if os.PathSeparator == '/' {
		os.Chmod(path, 0o644)
		if _, err := loadToken(path, logger); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("файл токена, открытый всем: %v", err)
		}
	}
	t.Setenv(envToken, "short")
	if _, err := loadToken(path, logger); err == nil {
		t.Fatal("короткий токен из окружения принят")
	}
	t.Setenv(envToken, testToken)
	if token, err := loadToken(path, logger); err != nil || token != testToken {
		t.Fatalf("токен из окружения: %q %v", token, err)
	}
}
