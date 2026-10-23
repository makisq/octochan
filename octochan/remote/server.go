package remote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"octochan/audit"
	"octochan/rbac"
)

// Протокол — JSON поверх HTTP(S):
//
//	POST /v1/login   {role, login, password}  → {token, role, history}
//	POST /v1/exec    {args, stdin}            → {stdout, stderr, code}
//	POST /v1/role    {action, role, login, password} → {role, history}
//	GET  /v1/whoami                            → {role, history}
//	POST /v1/logout
//
// Всё, кроме входа, — с заголовком Authorization: Bearer <токен>.

// LoginRequest — вход в роль.
type LoginRequest struct {
	Role     string `json:"role"`
	Login    string `json:"login,omitempty"`
	Password string `json:"password,omitempty"`
}

// ExecRequest — команда ochan на сервере.
type ExecRequest struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin,omitempty"`
}

// RoleRequest — set role или back role внутри сессии.
type RoleRequest struct {
	Action   string `json:"action"` // set или back
	Role     string `json:"role,omitempty"`
	Login    string `json:"login,omitempty"`
	Password string `json:"password,omitempty"`
}

// Result — итог команды.
type Result struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int    `json:"code"`
}

// State — роль сессии и её история.
type State struct {
	Token   string   `json:"token,omitempty"`
	Role    string   `json:"role"`
	History []string `json:"history"`
}

// Problem — отказ сервера.
type Problem struct {
	Error string `json:"error"`
	// NeedLogin и NeedPassword подсказывают клиенту, что спросить.
	NeedLogin    bool `json:"need_login,omitempty"`
	NeedPassword bool `json:"need_password,omitempty"`
	// RetryAfter — через сколько секунд можно пробовать снова.
	RetryAfter int `json:"retry_after,omitempty"`
}

// Executor выполняет команду в роли role. Адрес клиента — ClientAddr(ctx).
type Executor func(ctx context.Context, role string, args []string, stdin string) Result

type clientKey struct{}

// ClientAddr — адрес клиента, приславшего команду.
func ClientAddr(ctx context.Context) string {
	addr, _ := ctx.Value(clientKey{}).(string)
	return addr
}

// record пишет событие сервера в журнал аудита. Сбой журнала не
// останавливает сервер, но попадает в его лог.
func (s *Server) record(rec audit.Record, refused string) {
	if s.Audit == nil {
		return
	}
	var guard func(audit.Recent) error
	if refused != "" {
		guard = func(audit.Recent) error { return errors.New(refused) }
	}
	if err := s.Audit.Append(rec, guard); err != nil && refused == "" {
		s.Logger.Printf("журнал аудита: %v", err)
	}
}

// Server — общий оркестратор.
type Server struct {
	// LoadRoles читает хранилище ролей на каждый запрос: роль, созданную
	// или изменённую при работающем сервере, видно сразу, а удалённая
	// роль сразу перестаёт работать.
	LoadRoles func() (*rbac.Store, error)
	// Authorize решает, может ли роль выполнить команду.
	Authorize func(role *rbac.Role, args []string) error
	Exec      Executor
	Logger    *log.Logger
	// Audit — журнал аудита: входы, смены ролей и отказы сервера. Команды,
	// которые сервер пропустил, пишет в журнал сам дочерний процесс.
	// nil — без журнала.
	Audit *audit.Journal

	// SessionTimeout — сколько живёт сессия без запросов.
	SessionTimeout time.Duration
	// SessionLifetime — сколько живёт сессия всего.
	SessionLifetime time.Duration
	// MaxSessions — предел числа сессий.
	MaxSessions int
	// MaxFailures — неудачных входов подряд до блокировки; Lockout — на
	// сколько блокируется пара «адрес, роль».
	MaxFailures int
	Lockout     time.Duration

	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	failures map[string]*failure

	// verify ограничивает одновременные проверки пароля: argon2id берёт
	// 64 МиБ на проверку, и сотня параллельных входов съела бы память.
	verifyOnce sync.Once
	verify     chan struct{}
}

type session struct {
	role    string
	history []string
	// creds — отпечатки логина и пароля ролей, в которые сессия входила:
	// back role пускает без входа, только пока они не менялись.
	creds   map[string]string
	created time.Time
	seen    time.Time
	remote  string
}

type failure struct {
	count int
	until time.Time
	last  time.Time
}

const (
	loginBody = 64 << 10
	execBody  = 16 << 20
	maxActive = 4
)

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Server) defaults() {
	s.verifyOnce.Do(func() {
		if s.SessionTimeout <= 0 {
			s.SessionTimeout = 30 * time.Minute
		}
		if s.SessionLifetime <= 0 {
			s.SessionLifetime = 12 * time.Hour
		}
		if s.MaxSessions <= 0 {
			s.MaxSessions = 256
		}
		if s.MaxFailures <= 0 {
			s.MaxFailures = 5
		}
		if s.Lockout <= 0 {
			s.Lockout = time.Minute
		}
		if s.Logger == nil {
			s.Logger = log.New(log.Writer(), "", log.LstdFlags)
		}
		s.sessions = make(map[string]*session)
		s.failures = make(map[string]*failure)
		s.verify = make(chan struct{}, maxActive)
	})
}

// Handler — обработчик запросов сервера.
func (s *Server) Handler() http.Handler {
	s.defaults()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login", s.login)
	mux.HandleFunc("POST /v1/exec", s.authed(s.exec))
	mux.HandleFunc("POST /v1/role", s.authed(s.role))
	mux.HandleFunc("GET /v1/whoami", s.authed(s.whoami))
	mux.HandleFunc("POST /v1/logout", s.authed(s.logout))
	return mux
}

// ---------------------------------------------------------------------------
// Вход
// ---------------------------------------------------------------------------

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if !decode(w, r, loginBody, &req) {
		return
	}
	ip := clientIP(r)

	if wait := s.locked(ip, req.Role); wait > 0 {
		s.Logger.Printf("вход: роль %s с %s заблокирован ещё на %s", req.Role, ip, wait.Round(time.Second))
		s.record(audit.Record{Event: audit.EventLogin, Role: req.Role, Remote: ip}, "вход заблокирован после неудачных попыток")
		reply(w, http.StatusTooManyRequests, Problem{
			Error:      fmt.Sprintf("слишком много неудачных входов; повторите через %s", wait.Round(time.Second)),
			RetryAfter: int(wait.Seconds()) + 1,
		})
		return
	}

	store, err := s.LoadRoles()
	if err != nil {
		s.Logger.Printf("вход: хранилище ролей: %v", err)
		reply(w, http.StatusInternalServerError, Problem{Error: "хранилище ролей сервера недоступно"})
		return
	}

	role, problem := s.authenticate(r.Context(), store, ip, req.Role, req.Login, req.Password)
	if problem != nil {
		// Пустой вход — вопрос «что спросить», а не попытка.
		if req.Login != "" || req.Password != "" {
			s.record(audit.Record{Event: audit.EventLogin, Role: req.Role, Remote: ip}, problem.Error)
		}
		reply(w, http.StatusUnauthorized, *problem)
		return
	}

	token, err := newToken()
	if err != nil {
		reply(w, http.StatusInternalServerError, Problem{Error: err.Error()})
		return
	}

	now := s.clock()
	sess := &session{
		role: role.Name, history: []string{role.Name}, creds: map[string]string{},
		created: now, seen: now, remote: ip,
	}
	sess.creds[strings.ToLower(role.Name)] = credFingerprint(role)

	s.mu.Lock()
	s.prune(now)
	if len(s.sessions) >= s.MaxSessions {
		s.mu.Unlock()
		reply(w, http.StatusServiceUnavailable, Problem{Error: "на сервере слишком много сессий"})
		return
	}
	s.sessions[tokenKey(token)] = sess
	s.mu.Unlock()

	s.Logger.Printf("вход: роль %s с %s", role.Name, ip)
	s.record(audit.Record{Event: audit.EventLogin, Role: role.Name, Remote: ip}, "")
	reply(w, http.StatusOK, State{Token: token, Role: role.Name, History: sess.history})
}

// authenticate проверяет вход в роль. Пустой вход в роль, которой нужен
// пароль, — не попытка, а вопрос «что спросить»: он не считается неудачей.
//
// На несуществующую роль сервер отвечает так же, как на роль с паролем:
// по ответу нельзя перебирать имена ролей.
func (s *Server) authenticate(ctx context.Context, store *rbac.Store, ip, name, login, password string) (*rbac.Role, *Problem) {
	role := store.Find(name)
	probe := login == "" && password == ""

	if role == nil {
		if probe {
			return nil, &Problem{Error: "нужен пароль", NeedPassword: true}
		}
		s.fail(ip, name)
		return nil, &Problem{Error: "неверная роль, логин или пароль", NeedPassword: true}
	}
	if !role.NeedsAuth() {
		return role, nil
	}

	need := &Problem{NeedLogin: role.Login != nil, NeedPassword: role.Password != nil}
	if probe {
		need.Error = "нужны логин или пароль"
		return nil, need
	}

	select {
	case s.verify <- struct{}{}:
	case <-ctx.Done():
		return nil, &Problem{Error: "запрос отменён"}
	}
	ok := role.Verify(login, password)
	<-s.verify

	if !ok {
		s.fail(ip, name)
		s.Logger.Printf("вход: неверный логин или пароль роли %s с %s", role.Name, ip)
		need.Error = "неверная роль, логин или пароль"
		return nil, need
	}
	s.succeed(ip, name)
	return role, nil
}

func failureKey(ip, role string) string { return ip + "|" + strings.ToLower(role) }

func (s *Server) locked(ip, role string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failures[failureKey(ip, role)]
	if f == nil {
		return 0
	}
	if wait := f.until.Sub(s.clock()); wait > 0 {
		return wait
	}
	return 0
}

func (s *Server) fail(ip, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	key := failureKey(ip, role)
	f := s.failures[key]
	if f == nil || now.Sub(f.last) > s.Lockout*10 {
		f = &failure{}
		s.failures[key] = f
	}
	f.count++
	f.last = now
	if f.count >= s.MaxFailures {
		f.until = now.Add(s.Lockout)
		f.count = 0
	}
}

func (s *Server) succeed(ip, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, failureKey(ip, role))
}

// ---------------------------------------------------------------------------
// Сессия
// ---------------------------------------------------------------------------

type authedHandler func(w http.ResponseWriter, r *http.Request, sess *session, store *rbac.Store)

// authed пускает только с действующей сессией, чья роль ещё существует.
func (s *Server) authed(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			reply(w, http.StatusUnauthorized, Problem{Error: "нужен вход: POST /v1/login"})
			return
		}

		now := s.clock()
		s.mu.Lock()
		s.prune(now)
		sess := s.sessions[tokenKey(token)]
		if sess != nil {
			sess.seen = now
		}
		s.mu.Unlock()
		if sess == nil {
			reply(w, http.StatusUnauthorized, Problem{Error: "сессии нет или она истекла: войдите заново"})
			return
		}

		store, err := s.LoadRoles()
		if err != nil {
			s.Logger.Printf("хранилище ролей: %v", err)
			reply(w, http.StatusInternalServerError, Problem{Error: "хранилище ролей сервера недоступно"})
			return
		}
		if store.Find(sess.role) == nil {
			s.drop(token)
			reply(w, http.StatusUnauthorized, Problem{Error: fmt.Sprintf("роли %s на сервере больше нет: войдите заново", sess.role)})
			return
		}
		// Смена пароля роли выводит из неё и тех, кто уже вошёл: иначе
		// сменой пароля нельзя было бы выгнать того, кто его узнал.
		if sess.creds[strings.ToLower(sess.role)] != credFingerprint(store.Find(sess.role)) {
			s.drop(token)
			reply(w, http.StatusUnauthorized, Problem{Error: fmt.Sprintf("логин или пароль роли %s сменился: войдите заново", sess.role)})
			return
		}
		next(w, r, sess, store)
	}
}

// prune удаляет истёкшие сессии и старые неудачи. Вызывается под mu.
func (s *Server) prune(now time.Time) {
	for key, sess := range s.sessions {
		if now.Sub(sess.seen) > s.SessionTimeout || now.Sub(sess.created) > s.SessionLifetime {
			delete(s.sessions, key)
		}
	}
	for key, f := range s.failures {
		if now.After(f.until) && now.Sub(f.last) > s.Lockout*10 {
			delete(s.failures, key)
		}
	}
}

func (s *Server) drop(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, tokenKey(token))
}

func (s *Server) state(sess *session) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return State{Role: sess.role, History: append([]string(nil), sess.history...)}
}

func (s *Server) whoami(w http.ResponseWriter, _ *http.Request, sess *session, _ *rbac.Store) {
	reply(w, http.StatusOK, s.state(sess))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess *session, _ *rbac.Store) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.drop(token)
	s.Logger.Printf("выход: роль %s с %s", sess.role, sess.remote)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Команды и роли
// ---------------------------------------------------------------------------

func (s *Server) exec(w http.ResponseWriter, r *http.Request, sess *session, store *rbac.Store) {
	var req ExecRequest
	if !decode(w, r, execBody, &req) {
		return
	}
	if len(req.Args) == 0 {
		reply(w, http.StatusBadRequest, Problem{Error: "пустая команда"})
		return
	}

	roleName := s.state(sess).Role
	role := store.Find(roleName)
	if err := s.Authorize(role, req.Args); err != nil {
		s.Logger.Printf("отказ: %s@%s: %s: %v", role.Name, sess.remote, strings.Join(req.Args, " "), err)
		s.record(audit.Record{Event: audit.EventCommand, Role: role.Name, Remote: sess.remote, Args: req.Args}, err.Error())
		reply(w, http.StatusForbidden, Problem{Error: err.Error()})
		return
	}

	started := s.clock()
	result := s.Exec(context.WithValue(r.Context(), clientKey{}, sess.remote), role.Name, req.Args, req.Stdin)
	s.Logger.Printf("%s@%s: %s → код %d за %s", role.Name, sess.remote, strings.Join(req.Args, " "),
		result.Code, s.clock().Sub(started).Round(time.Millisecond))
	reply(w, http.StatusOK, result)
}

// role — set role и back role внутри сессии. У каждой сессии своя роль
// и своя история; хранилище ролей сервера они не трогают.
func (s *Server) role(w http.ResponseWriter, r *http.Request, sess *session, store *rbac.Store) {
	var req RoleRequest
	if !decode(w, r, loginBody, &req) {
		return
	}

	current := s.state(sess)
	view := &rbac.Store{Roles: store.Roles, Current: current.Role, History: current.History}

	var target *rbac.Role
	switch req.Action {
	case "set":
		ip := clientIP(r)
		if wait := s.locked(ip, req.Role); wait > 0 {
			reply(w, http.StatusTooManyRequests, Problem{
				Error:      fmt.Sprintf("слишком много неудачных входов; повторите через %s", wait.Round(time.Second)),
				RetryAfter: int(wait.Seconds()) + 1,
			})
			return
		}
		role, problem := s.authenticate(r.Context(), store, ip, req.Role, req.Login, req.Password)
		if problem != nil {
			if req.Login != "" || req.Password != "" {
				s.record(audit.Record{Event: audit.EventRole, Role: current.Role, Remote: ip,
					Command: "set.role", Args: []string{req.Role}}, problem.Error)
			}
			reply(w, http.StatusUnauthorized, *problem)
			return
		}
		if strings.EqualFold(role.Name, current.Role) {
			reply(w, http.StatusBadRequest, Problem{Error: fmt.Sprintf("роль %s уже текущая", role.Name)})
			return
		}
		target = role

	case "back":
		role, err := view.BackTarget(req.Role)
		if err != nil {
			reply(w, http.StatusBadRequest, Problem{Error: err.Error()})
			return
		}
		s.mu.Lock()
		known := sess.creds[strings.ToLower(role.Name)]
		s.mu.Unlock()
		if known != credFingerprint(role) {
			reply(w, http.StatusUnauthorized, Problem{
				Error:     fmt.Sprintf("логин или пароль роли %s сменился: войдите через set role %s", role.Name, role.Name),
				NeedLogin: role.Login != nil, NeedPassword: role.Password != nil,
			})
			return
		}
		target = role

	default:
		reply(w, http.StatusBadRequest, Problem{Error: "action — set или back"})
		return
	}

	if err := view.Switch(target.Name); err != nil {
		reply(w, http.StatusBadRequest, Problem{Error: err.Error()})
		return
	}
	s.mu.Lock()
	sess.role = view.Current
	sess.history = view.History
	sess.creds[strings.ToLower(target.Name)] = credFingerprint(target)
	s.mu.Unlock()

	s.Logger.Printf("роль: %s@%s → %s", current.Role, sess.remote, target.Name)
	s.record(audit.Record{Event: audit.EventRole, Role: current.Role, Remote: sess.remote,
		Command: req.Action + ".role", Args: []string{target.Name}}, "")
	reply(w, http.StatusOK, s.state(sess))
}

// credFingerprint — отпечаток логина и пароля роли: меняется вместе с ними.
func credFingerprint(role *rbac.Role) string {
	h := sha256.New()
	for _, secret := range []*rbac.Secret{role.Login, role.Password} {
		if secret == nil {
			h.Write([]byte{0})
			continue
		}
		h.Write([]byte{1})
		h.Write(secret.Salt)
		h.Write(secret.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Мелочи
// ---------------------------------------------------------------------------

func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("не удалось создать токен сессии: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// tokenKey — под каким ключом сессия лежит в памяти: хеш, а не сам токен.
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			reply(w, http.StatusRequestEntityTooLarge, Problem{Error: fmt.Sprintf("запрос больше %d байт", limit)})
			return false
		}
		reply(w, http.StatusBadRequest, Problem{Error: "некорректный запрос: " + err.Error()})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve запускает сервер на addr и работает до отмены ctx.
//
// Без шифрования сервер слушает только эту машину: иначе пароли ролей
// шли бы по сети открытым текстом. allowPlaintext снимает запрет.
func (s *Server) Serve(ctx context.Context, addr string, settings Settings, allowPlaintext bool, ready func(string)) error {
	if !settings.SSL && !isLoopback(addr) && !allowPlaintext {
		return fmt.Errorf("ssl выключен, а %s доступен из сети: пароли шли бы открытым текстом; "+
			"включите ssl или запустите с --allow-plaintext", addr)
	}

	httpServer := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          s.Logger,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("не удалось слушать %s: %w", addr, err)
	}
	scheme := "http"
	if settings.SSL {
		config, err := ServerTLS(settings)
		if err != nil {
			listener.Close()
			return err
		}
		httpServer.TLSConfig = config
		scheme = "https"
	}
	if ready != nil {
		ready(scheme + "://" + listener.Addr().String())
	}

	done := make(chan error, 1)
	go func() {
		if settings.SSL {
			done <- httpServer.ServeTLS(listener, "", "")
		} else {
			done <- httpServer.Serve(listener)
		}
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		return nil
	}
}
