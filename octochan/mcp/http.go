package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Транспорт Streamable HTTP (MCP 2025-03-26 и 2025-06-18).
//
// По stdio сервер говорит с одним клиентом на своей машине. По HTTP — с
// клиентами, которые живут у поставщика модели: коннектор MCP в Claude API,
// инструмент mcp в OpenAI Responses API, ChatGPT, Gemini CLI, Cursor и
// другие. Им нужен адрес https, а найти его может кто угодно, поэтому:
//
//   - каждый запрос — с токеном: заголовок Authorization: Bearer, а для
//     клиентов, которые не умеют задавать заголовки, — путь /mcp/<токен>;
//   - не на loopback — только TLS, если не сказано, что TLS снимает прокси;
//   - запрос из браузера (заголовок Origin) — только с разрешённых адресов:
//     так страница, перепривязавшая своё имя на 127.0.0.1, не достучится;
//   - сессии: Mcp-Session-Id выдаётся при initialize, без него вызовы не
//     принимаются, простаивающая сессия истекает.
//
// Таблица инструментов и роль llm — те же, что по stdio: удалённая модель
// может ровно то же, что локальная, — читать, планировать и оформлять
// заявки, которые согласует человек.
//
// Тексты ответов протокола — по-английски: их читают клиент и модель.

// httpTransport — обработчик Streamable HTTP поверх server.
type httpTransport struct {
	srv     *server
	path    string
	token   [sha256.Size]byte
	origins map[string]bool
	logger  *log.Logger
	// idle — сколько живёт сессия без запросов; maxSessions — предел.
	idle        time.Duration
	maxSessions int
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]*httpSession
}

type httpSession struct {
	client  string
	created time.Time
	seen    time.Time
}

func newHTTPTransport(srv *server, path, token string, origins []string, idle time.Duration, logger *log.Logger) *httpTransport {
	allowed := make(map[string]bool, len(origins))
	for _, origin := range origins {
		allowed[strings.TrimSuffix(origin, "/")] = true
	}
	return &httpTransport{
		srv: srv, path: "/" + strings.Trim(path, "/"), token: sha256.Sum256([]byte(token)),
		origins: allowed, logger: logger, idle: idle, maxSessions: 100, now: time.Now,
		sessions: make(map[string]*httpSession),
	}
}

func (h *httpTransport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tokenInPath := false
	switch {
	case r.URL.Path == h.path:
	case strings.HasPrefix(r.URL.Path, h.path+"/") && !strings.Contains(r.URL.Path[len(h.path)+1:], "/"):
		tokenInPath = true
	default:
		http.NotFound(w, r)
		return
	}

	if origin := r.Header.Get("Origin"); origin != "" && !h.origins[origin] {
		h.logger.Printf("http: %s: отказ: Origin %s не разрешён", clientOf(r), origin)
		h.fail(w, http.StatusForbidden, "origin not allowed")
		return
	}
	if !h.authorized(r, tokenInPath) {
		// Путь с токеном в журнал не пишется: в нём и есть секрет.
		h.logger.Printf("http: %s: отказ: нет токена или он неверный", clientOf(r))
		w.Header().Set("WWW-Authenticate", `Bearer realm="ochan-mcp"`)
		h.fail(w, http.StatusUnauthorized, "a valid bearer token is required")
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.post(w, r)
	case http.MethodDelete:
		h.close(w, r)
	default:
		// Потока событий от сервера нет: уведомлений сервер не шлёт.
		w.Header().Set("Allow", "POST, DELETE")
		h.fail(w, http.StatusMethodNotAllowed, "this server offers no event stream: use POST")
	}
}

// authorized сравнивает токен за постоянное время: по времени ответа его
// не подобрать посимвольно.
func (h *httpTransport) authorized(r *http.Request, tokenInPath bool) bool {
	var got string
	if tokenInPath {
		got = r.URL.Path[len(h.path)+1:]
	} else if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		got = strings.TrimSpace(bearer)
	}
	sum := sha256.Sum256([]byte(got))
	return got != "" && subtle.ConstantTimeCompare(sum[:], h.token[:]) == 1
}

func (h *httpTransport) post(w http.ResponseWriter, r *http.Request) {
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !knownVersion(version) {
		h.fail(w, http.StatusBadRequest, "unsupported MCP-Protocol-Version "+version+
			"; supported: "+strings.Join(protocolVersions, ", "))
		return
	}
	if kind := r.Header.Get("Content-Type"); kind != "" {
		if media, _, err := mime.ParseMediaType(kind); err != nil || media != "application/json" {
			h.fail(w, http.StatusUnsupportedMediaType, "the body must be application/json")
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMessage+1))
	switch {
	case err != nil:
		h.fail(w, http.StatusBadRequest, "the body could not be read")
		return
	case len(body) > maxMessage:
		h.fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a message longer than %d bytes is rejected", maxMessage))
		return
	}
	body = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(body), utf8BOM))
	if len(body) == 0 {
		h.fail(w, http.StatusBadRequest, "empty body")
		return
	}

	batch := body[0] == '['
	raws := []json.RawMessage{body}
	if batch {
		if err := json.Unmarshal(body, &raws); err != nil || len(raws) == 0 {
			h.fail(w, http.StatusBadRequest, "malformed JSON-RPC batch")
			return
		}
	}
	initialize := false
	for _, raw := range raws {
		var msg message
		if json.Unmarshal(raw, &msg) == nil && msg.Method == "initialize" {
			initialize = true
		}
	}

	client := clientOf(r)
	session := r.Header.Get("Mcp-Session-Id")
	switch {
	case initialize && len(raws) > 1:
		h.fail(w, http.StatusBadRequest, "initialize must be sent on its own, not in a batch")
		return
	case initialize:
		id, err := h.open(client)
		if err != nil {
			h.fail(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		session = id
		w.Header().Set("Mcp-Session-Id", id)
	case session == "":
		h.fail(w, http.StatusBadRequest, "the Mcp-Session-Id header is required: send initialize first")
		return
	case !h.touch(session):
		h.fail(w, http.StatusNotFound, "the session is unknown or expired: send initialize again")
		return
	}

	ctx := withClient(withSession(r.Context(), session), client)
	var responses []*response
	for _, raw := range raws {
		if resp := h.srv.handle(ctx, raw); resp != nil {
			responses = append(responses, resp)
		}
	}
	// Одни уведомления и ответы клиента — принято, отвечать нечем.
	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var payload any = responses[0]
	if batch {
		payload = responses
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		h.logger.Printf("http: %s: ответ не отправлен: %v", client, err)
	}
}

// close завершает сессию по просьбе клиента.
func (h *httpTransport) close(w http.ResponseWriter, r *http.Request) {
	session := r.Header.Get("Mcp-Session-Id")
	h.mu.Lock()
	_, ok := h.sessions[session]
	delete(h.sessions, session)
	h.mu.Unlock()
	if !ok {
		h.fail(w, http.StatusNotFound, "the session is unknown or expired")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *httpTransport) open(client string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("no randomness for a session id")
	}
	id := hex.EncodeToString(raw)

	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prune(now)
	if len(h.sessions) >= h.maxSessions {
		return "", errors.New("too many sessions: try again later")
	}
	h.sessions[id] = &httpSession{client: client, created: now, seen: now}
	h.logger.Printf("http: сессия с %s открыта", client)
	return id, nil
}

func (h *httpTransport) touch(id string) bool {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prune(now)
	sess, ok := h.sessions[id]
	if ok {
		sess.seen = now
	}
	return ok
}

// prune убирает простаивающие сессии. Вызывается под mu.
func (h *httpTransport) prune(now time.Time) {
	for id, sess := range h.sessions {
		if now.Sub(sess.seen) > h.idle {
			delete(h.sessions, id)
		}
	}
}

// fail отвечает ошибкой JSON-RPC с кодом HTTP: так её поймёт и клиент MCP,
// и человек с curl.
func (h *httpTransport) fail(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse(nil, codeHTTP, text))
}

// codeHTTP — ошибка транспорта: диапазон -32000…-32099 JSON-RPC отдан
// под ошибки сервера.
const codeHTTP = -32000

func knownVersion(version string) bool {
	for _, known := range protocolVersions {
		if version == known {
			return true
		}
	}
	return false
}

func clientOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type clientKey struct{}

func withClient(ctx context.Context, client string) context.Context {
	return context.WithValue(ctx, clientKey{}, client)
}

// clientAddr — адрес клиента HTTP; пусто — клиент на stdio.
func clientAddr(ctx context.Context) string {
	client, _ := ctx.Value(clientKey{}).(string)
	return client
}

// httpOptions — настройки HTTP-режима из флагов.
type httpOptions struct {
	listen         string
	path           string
	tokenFile      string
	tlsCert        string
	tlsKey         string
	allowPlaintext bool
	origins        []string
	idle           time.Duration
}

// serveHTTP слушает адрес до отмены ctx.
func serveHTTP(ctx context.Context, srv *server, opts httpOptions, callTimeout time.Duration, logger *log.Logger) error {
	if (opts.tlsCert == "") != (opts.tlsKey == "") {
		return errors.New("--tls-cert и --tls-key задаются вместе")
	}
	token, err := loadToken(opts.tokenFile, logger)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("не удалось слушать %s: %w", opts.listen, err)
	}
	secure := opts.tlsCert != ""
	if !secure && !loopback(listener.Addr()) && !opts.allowPlaintext {
		listener.Close()
		return fmt.Errorf("%s доступен из сети, а TLS не задан: токен ушёл бы открытым текстом. "+
			"Укажите --tls-cert и --tls-key или, если TLS снимает прокси перед сервером, --allow-plaintext",
			listener.Addr())
	}

	transport := newHTTPTransport(srv, opts.path, token, opts.origins, opts.idle, logger)
	server := &http.Server{
		Handler:           transport,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		// Вызов инструмента идёт до --timeout, ответ отправляется после.
		WriteTimeout:   callTimeout + 30*time.Second,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
		ErrorLog:       logger,
	}
	scheme := "http"
	if secure {
		scheme = "https"
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	logger.Printf("слушаю %s://%s%s", scheme, listener.Addr(), transport.path)

	done := make(chan error, 1)
	go func() {
		if secure {
			done <- server.ServeTLS(listener, opts.tlsCert, opts.tlsKey)
		} else {
			done <- server.Serve(listener)
		}
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func loopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// envToken — токен окружением, вместо файла: так его отдают контейнеру.
const envToken = "OCHAN_MCP_TOKEN"

// minToken — короче токен перебирается.
const minToken = 32

// loadToken берёт токен из окружения или файла. Файла нет — создаётся
// новый токен: 256 случайных бит, файл 0600.
func loadToken(path string, logger *log.Logger) (string, error) {
	if token := strings.TrimSpace(os.Getenv(envToken)); token != "" {
		if len(token) < minToken {
			return "", fmt.Errorf("%s короче %d знаков", envToken, minToken)
		}
		return token, nil
	}
	if path == "" {
		return "", fmt.Errorf("нужен токен: --token-file или %s", envToken)
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		token := hex.EncodeToString(raw)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("токен: %w", err)
		}
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("токен: %w", err)
		}
		logger.Printf("создан токен доступа: %s", path)
		return token, nil
	}
	if err != nil {
		return "", fmt.Errorf("токен: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		return "", fmt.Errorf("файл токена %s доступен не только владельцу (%s): chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	token := strings.TrimSpace(string(data))
	if len(token) < minToken {
		return "", fmt.Errorf("токен в %s короче %d знаков", path, minToken)
	}
	return token, nil
}

// defaultTokenFile — файл токена по умолчанию: каталог настроек
// пользователя, а не рабочий каталог ochan — туда нейросети и её серверу
// хода нет.
func defaultTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ochan-mcp", "token")
}
