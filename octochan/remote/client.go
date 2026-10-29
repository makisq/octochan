package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// AuthNeeded — сервер ждёт логин или пароль.
type AuthNeeded struct {
	Login    bool
	Password bool
	Message  string
}

func (e *AuthNeeded) Error() string { return e.Message }

// Client — подключение к серверу octochan.
type Client struct {
	base  string
	http  *http.Client
	token string
	plain bool
}

// Dial готовит подключение к addr (host:port).
//
// Без шифрования клиент соединяется только с этой машиной: иначе пароль
// ушёл бы по сети открытым текстом. allowPlaintext снимает запрет.
func Dial(addr string, settings Settings, allowPlaintext bool) (*Client, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("адрес сервера %q: нужен вид host:port", addr)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0, // команды бывают долгими
		IdleConnTimeout:       time.Minute,
	}
	scheme := "http"
	if settings.SSL {
		config, err := ClientTLS(settings, host)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = config
		scheme = "https"
	} else if !isLoopback(addr) && !allowPlaintext {
		return nil, fmt.Errorf("ssl выключен, а %s — не эта машина: пароль ушёл бы открытым текстом; "+
			"включите ssl или подключитесь с --allow-plaintext", addr)
	}

	return &Client{
		base:  scheme + "://" + addr,
		http:  &http.Client{Transport: transport},
		plain: !settings.SSL,
	}, nil
}

// Plain сообщает, что соединение не шифруется.
func (c *Client) Plain() bool { return c.plain }

// Login входит в роль. Если нужны логин или пароль, возвращает
// *AuthNeeded — тогда их надо спросить и войти снова.
func (c *Client) Login(ctx context.Context, role, login, password string) (*State, error) {
	var state State
	if err := c.call(ctx, http.MethodPost, "/v1/login", LoginRequest{Role: role, Login: login, Password: password}, &state); err != nil {
		return nil, err
	}
	c.token = state.Token
	state.Token = ""
	return &state, nil
}

// Exec выполняет команду на сервере.
func (c *Client) Exec(ctx context.Context, args []string, stdin string) (*Result, error) {
	var result Result
	if err := c.call(ctx, http.MethodPost, "/v1/exec", ExecRequest{Args: args, Stdin: stdin}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SetRole переключает роль сессии.
func (c *Client) SetRole(ctx context.Context, role, login, password string) (*State, error) {
	var state State
	err := c.call(ctx, http.MethodPost, "/v1/role", RoleRequest{Action: "set", Role: role, Login: login, Password: password}, &state)
	return &state, err
}

// BackRole возвращает к прошлой роли сессии.
func (c *Client) BackRole(ctx context.Context, role string) (*State, error) {
	var state State
	err := c.call(ctx, http.MethodPost, "/v1/role", RoleRequest{Action: "back", Role: role}, &state)
	return &state, err
}

// WhoAmI возвращает роль сессии и её историю.
func (c *Client) WhoAmI(ctx context.Context) (*State, error) {
	var state State
	err := c.call(ctx, http.MethodGet, "/v1/whoami", nil, &state)
	return &state, err
}

// Logout завершает сессию.
func (c *Client) Logout(ctx context.Context) error {
	if c.token == "" {
		return nil
	}
	err := c.call(ctx, http.MethodPost, "/v1/logout", nil, nil)
	c.token = ""
	return err
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return explainDialError(c.base, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, execBody*3))
	if err != nil {
		return fmt.Errorf("не удалось прочитать ответ сервера: %w", err)
	}

	if resp.StatusCode >= 300 {
		var problem Problem
		if json.Unmarshal(data, &problem) != nil || problem.Error == "" {
			return &statusError{code: resp.StatusCode, text: resp.Status}
		}
		if resp.StatusCode == http.StatusUnauthorized && (problem.NeedLogin || problem.NeedPassword) {
			return &AuthNeeded{Login: problem.NeedLogin, Password: problem.NeedPassword, Message: problem.Error}
		}
		if resp.StatusCode == http.StatusUnauthorized && c.token != "" {
			c.token = ""
			return &SessionExpired{Message: problem.Error}
		}
		if resp.StatusCode == http.StatusConflict && problem.Primary != "" {
			return &NotPrimary{Message: problem.Error, Primary: problem.Primary}
		}
		return errors.New(problem.Error)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("ответ сервера не разбирается: %w", err)
	}
	return nil
}

// explainDialError объясняет частые причины отказа соединения: сервер без
// TLS при ssl: true и сертификат, которому клиент не доверяет.
func explainDialError(base string, err error) error {
	text := err.Error()
	switch {
	case strings.Contains(text, "server gave HTTP response to HTTPS client"):
		return fmt.Errorf("%s отвечает без шифрования, а у клиента ssl включён: %w", base, err)
	case strings.Contains(text, "certificate signed by unknown authority"),
		strings.Contains(text, "certificate is not valid"),
		strings.Contains(text, "x509:"):
		return fmt.Errorf("сертификат сервера %s не прошёл проверку: укажите его в tls.ca "+
			"(самоподписанный сертификат сервера — это и есть его CA): %w", base, err)
	}
	return fmt.Errorf("нет связи с %s: %w", base, err)
}

// ErrNotPaired — сервер не в паре: на /v1/ha/status он не отвечает.
var ErrNotPaired = errors.New("сервер не в паре primary/standby")

// NodeStatus — что узел говорит о себе. Входа не нужно.
func NodeStatus(ctx context.Context, addr string, settings Settings, allowPlaintext bool) (*HAStatus, error) {
	c, err := Dial(addr, settings, allowPlaintext)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var st HAStatus
	if err := c.call(ctx, http.MethodGet, "/v1/ha/status", nil, &st); err != nil {
		var status *statusError
		if errors.As(err, &status) && status.code == http.StatusNotFound {
			return nil, ErrNotPaired
		}
		return nil, err
	}
	return &st, nil
}

// statusError — сервер ответил кодом без описания.
type statusError struct {
	code int
	text string
}

func (e *statusError) Error() string { return "сервер ответил " + e.text }

// SessionExpired — сессия на сервере кончилась: команда не выполнялась,
// нужно войти снова.
type SessionExpired struct{ Message string }

func (e *SessionExpired) Error() string { return e.Message }
