package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
)

// Pool — клиент пары серверов: запись идёт на primary, чтение — на любой
// живой узел (с Balance), а переключение пары клиент переживает сам.
//
// Повтор — только когда команда точно не выполнялась: узел ответил «я
// standby, primary — там» или соединение не установилось. Оборвалось
// посреди команды — исход неизвестен, и пул говорит это, а не повторяет:
// повтор записи мог бы выполнить её дважды.
type Pool struct {
	addrs    []string
	settings Settings
	plain    bool
	// Balance — команды чтения по очереди на все узлы на связи.
	Balance bool
	// OnSwitch — сообщить, что primary сменился.
	OnSwitch func(from, to string)

	mu      sync.Mutex
	clients map[string]*Client
	primary string
	role    string
	creds   map[string][2]string
	next    int
}

// DialPool готовит пул и находит primary. Один адрес без пары — просто
// сервер.
func DialPool(ctx context.Context, addrs []string, settings Settings, allowPlaintext bool) (*Pool, error) {
	if len(addrs) == 0 {
		return nil, errors.New("не указан адрес сервера")
	}
	p := &Pool{addrs: addrs, settings: settings, plain: allowPlaintext,
		clients: map[string]*Client{}, creds: map[string][2]string{}}
	for _, addr := range addrs {
		c, err := Dial(addr, settings, allowPlaintext)
		if err != nil {
			return nil, err
		}
		p.clients[addr] = c
	}
	primary, err := p.findPrimary(ctx, "")
	if err != nil {
		return nil, err
	}
	p.primary = primary
	return p, nil
}

// Primary — адрес primary.
func (p *Pool) Primary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.primary
}

// Plain сообщает, что соединение не шифруется.
func (p *Pool) Plain() bool { return !p.settings.SSL }

// findPrimary опрашивает узлы. Узел без пары — сервер сам по себе: годится,
// если он один. Из узлов пары — primary без конфликта с наибольшей эпохой.
func (p *Pool) findPrimary(ctx context.Context, skip string) (string, error) {
	var (
		best      string
		bestEpoch int64 = -1
		single    []string
		reasons   []string
	)
	for _, addr := range p.addrs {
		if addr == skip {
			continue
		}
		st, err := NodeStatus(ctx, addr, p.settings, p.plain)
		switch {
		case errors.Is(err, ErrNotPaired):
			single = append(single, addr)
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("%s: %v", addr, err))
		case st.Conflict:
			reasons = append(reasons, fmt.Sprintf("%s: конфликт двух primary — запись закрыта", addr))
		case st.Role == RolePrimary && st.Epoch > bestEpoch:
			best, bestEpoch = addr, st.Epoch
		case st.Role != RolePrimary:
			reasons = append(reasons, fmt.Sprintf("%s: standby (эпоха %d)", addr, st.Epoch))
		}
	}
	switch {
	case best != "":
		return best, nil
	case len(single) == 1 && len(p.addrs) == 1:
		return single[0], nil
	case len(single) > 0:
		return "", fmt.Errorf("%s — серверы не в паре: разные серверы одним списком не подключаются", strings.Join(single, ", "))
	}
	return "", fmt.Errorf("primary не найден: %s", strings.Join(reasons, "; "))
}

// Login входит в роль на primary. Логин и пароль пул помнит, пока жив: ими
// он входит на другой узел — для чтения или после переключения.
func (p *Pool) Login(ctx context.Context, role, login, password string) (*State, error) {
	c := p.client(p.Primary())
	state, err := c.Login(ctx, role, login, password)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.role = state.Role
	p.creds[strings.ToLower(state.Role)] = [2]string{login, password}
	p.mu.Unlock()
	return state, nil
}

func (p *Pool) client(addr string) *Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clients[addr]
}

// session — клиент узла, вошедший в текущую роль.
func (p *Pool) session(ctx context.Context, addr string) (*Client, error) {
	c := p.client(addr)
	if c.token != "" {
		return c, nil
	}
	p.mu.Lock()
	role, creds := p.role, p.creds[strings.ToLower(p.role)]
	p.mu.Unlock()
	if _, err := c.Login(ctx, role, creds[0], creds[1]); err != nil {
		return nil, fmt.Errorf("%s: вход в роль %s: %w", addr, role, err)
	}
	return c, nil
}

// Exec выполняет команду. readOnly — команда только читает: её можно на
// любой узел.
func (p *Pool) Exec(ctx context.Context, args []string, stdin string, readOnly bool) (*Result, error) {
	addr := p.Primary()
	if readOnly && p.Balance && len(p.addrs) > 1 {
		p.mu.Lock()
		addr = p.addrs[p.next%len(p.addrs)]
		p.next++
		p.mu.Unlock()
	}
	result, err := p.execOn(ctx, addr, args, stdin)
	if err == nil {
		return result, nil
	}
	retry, why := p.retryable(err)
	if !retry {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && !readOnly {
			return nil, fmt.Errorf("связь с %s оборвалась посреди команды: исход неизвестен — посмотрите "+
				"ochan requests и ochan history, прежде чем повторять: %w", addr, err)
		}
		return nil, err
	}
	var expired *SessionExpired
	if errors.As(err, &expired) {
		// Сессия кончилась — войти снова на тот же узел.
		return p.execOn(ctx, addr, args, stdin)
	}
	if readOnly && addr != p.Primary() {
		// Узел для чтения не ответил — прочитать на primary.
		return p.execOn(ctx, p.Primary(), args, stdin)
	}
	primary, ferr := p.findPrimary(ctx, addr)
	if ferr != nil {
		return nil, fmt.Errorf("%s: %s; другой primary не найден: %w", addr, why, ferr)
	}
	p.mu.Lock()
	old := p.primary
	p.primary = primary
	p.mu.Unlock()
	if p.OnSwitch != nil && old != primary {
		p.OnSwitch(old, primary)
	}
	return p.execOn(ctx, primary, args, stdin)
}

func (p *Pool) execOn(ctx context.Context, addr string, args []string, stdin string) (*Result, error) {
	c, err := p.session(ctx, addr)
	if err != nil {
		return nil, err
	}
	return c.Exec(ctx, args, stdin)
}

// retryable — команда точно не выполнялась: узел — standby или до него
// не достучаться.
func (p *Pool) retryable(err error) (bool, string) {
	var notPrimary *NotPrimary
	if errors.As(err, &notPrimary) {
		return true, notPrimary.Error()
	}
	var expired *SessionExpired
	if errors.As(err, &expired) {
		return true, expired.Error()
	}
	var urlErr *url.Error
	var opErr *net.OpError
	if errors.As(err, &urlErr) && errors.As(err, &opErr) && opErr.Op == "dial" {
		return true, "узел недоступен"
	}
	return false, ""
}

// SetRole переключает роль на primary; сессии других узлов входят в новую
// роль при следующем обращении.
func (p *Pool) SetRole(ctx context.Context, role, login, password string) (*State, error) {
	c, err := p.session(ctx, p.Primary())
	if err != nil {
		return nil, err
	}
	state, err := c.SetRole(ctx, role, login, password)
	if err != nil {
		return state, err
	}
	p.switched(state.Role, &login, &password)
	return state, nil
}

// BackRole возвращает к прошлой роли на primary.
func (p *Pool) BackRole(ctx context.Context, role string) (*State, error) {
	c, err := p.session(ctx, p.Primary())
	if err != nil {
		return nil, err
	}
	state, err := c.BackRole(ctx, role)
	if err != nil {
		return state, err
	}
	p.switched(state.Role, nil, nil)
	return state, nil
}

func (p *Pool) switched(role string, login, password *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.role = role
	if login != nil {
		p.creds[strings.ToLower(role)] = [2]string{*login, *password}
	}
	for addr, c := range p.clients {
		if addr != p.primary && c.token != "" {
			go c.Logout(context.Background())
		}
	}
}

// WhoAmI — роль сессии на primary.
func (p *Pool) WhoAmI(ctx context.Context) (*State, error) {
	c, err := p.session(ctx, p.Primary())
	if err != nil {
		return nil, err
	}
	return c.WhoAmI(ctx)
}

// Logout завершает сессии на всех узлах.
func (p *Pool) Logout(ctx context.Context) error {
	var first error
	for _, addr := range p.addrs {
		if err := p.client(addr).Logout(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// NotPrimary — узел ответил, что он standby: запись — на primary.
type NotPrimary struct {
	Message string
	Primary string
}

func (e *NotPrimary) Error() string { return e.Message }
