package remote

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"octochan/rbac"
)

func init() {
	rbac.HashCost = rbac.Cost{Time: 1, Memory: 1024, Threads: 1}
}

// fakeClock — часы, которые двигает тест.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t        *testing.T
	store    *rbac.Store
	storeMu  sync.Mutex
	server   *Server
	clock    *fakeClock
	url      string
	settings Settings
}

// newHarness запускает сервер с TLS на 127.0.0.1 и хранилищем в памяти.
// Роли: master (пароль m), slave (read), dan (логин dan, пароль d; read, apply).
func newHarness(t *testing.T) *harness {
	t.Helper()

	store := rbac.Default()
	if _, err := store.Create("slave", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("dan", []string{"read", "apply"}, nil); err != nil {
		t.Fatal(err)
	}
	set := func(name string, login, password string) {
		if login != "" {
			if err := store.SetLogin(name, &login); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.SetPassword(name, &password); err != nil {
			t.Fatal(err)
		}
	}
	set(rbac.Master, "", "m")
	set("dan", "dan", "d")

	dir := t.TempDir()
	cert, key := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if _, err := GenerateCert(cert, key, []string{"127.0.0.1", "localhost"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, store: store, clock: &fakeClock{now: time.Unix(1_800_000_000, 0)},
		settings: Settings{SSL: true, MinVersion: "1.3", Cert: cert, Key: key, CA: cert}}
	h.server = &Server{
		LoadRoles: func() (*rbac.Store, error) {
			h.storeMu.Lock()
			defer h.storeMu.Unlock()
			return h.store, nil
		},
		Authorize: func(role *rbac.Role, args []string) error {
			if !role.Allows(args[0]) {
				return errors.New("роль " + role.Name + " не может выполнять " + args[0])
			}
			return nil
		},
		Exec: func(_ context.Context, role string, args []string, stdin string) Result {
			return Result{Stdout: role + ": " + strings.Join(args, " ") + stdin, Code: 0}
		},
		Logger: log.New(io.Discard, "", 0),
	}
	h.server.now = h.clock.Now

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- h.server.Serve(ctx, "127.0.0.1:0", h.settings, false, func(url string) { ready <- url })
	}()
	select {
	case url := <-ready:
		h.url = strings.TrimPrefix(url, "https://")
	case err := <-done:
		t.Fatalf("сервер не запустился: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return h
}

func (h *harness) client() *Client {
	h.t.Helper()
	c, err := Dial(h.url, h.settings, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) login(role, login, password string) *Client {
	h.t.Helper()
	c := h.client()
	if _, err := c.Login(context.Background(), role, login, password); err != nil {
		h.t.Fatalf("вход в %s: %v", role, err)
	}
	return c
}

func mustFail(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ожидалась ошибка с %q, получено: %v", want, err)
	}
}

func TestLoginAndExec(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	c := h.login("slave", "", "")
	result, err := c.Exec(ctx, []string{"diff", "a.conf", "b.conf"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "slave: diff a.conf b.conf" {
		t.Fatalf("команда выполнена не в роли сессии: %q", result.Stdout)
	}

	_, err = c.Exec(ctx, []string{"apply", "x"}, "")
	mustFail(t, err, "роль slave не может выполнять apply")

	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = c.Exec(ctx, []string{"diff"}, "")
	mustFail(t, err, "нужен вход")
}

// Сервер говорит, что спросить, но не говорит, что именно не совпало.
func TestLoginAsksForCredentials(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := h.client()

	_, err := c.Login(ctx, "dan", "", "")
	var need *AuthNeeded
	if !errors.As(err, &need) || !need.Login || !need.Password {
		t.Fatalf("ожидался вопрос о логине и пароле, получено %v", err)
	}
	_, err = c.Login(ctx, "dan", "dan", "wrong")
	mustFail(t, err, "неверная роль, логин или пароль")
	_, err = c.Login(ctx, "dan", "other", "d")
	mustFail(t, err, "неверная роль, логин или пароль")

	// Несуществующая роль отвечает так же, как роль с паролем.
	_, err = c.Login(ctx, "ghost", "", "")
	if !errors.As(err, &need) || !need.Password {
		t.Fatalf("несуществующая роль выдаёт себя: %v", err)
	}

	if _, err := c.Login(ctx, "DAN", "dan", "d"); err != nil {
		t.Fatal(err)
	}
	state, err := c.WhoAmI(ctx)
	if err != nil || state.Role != "dan" {
		t.Fatalf("роль сессии: %+v, %v", state, err)
	}
}

func TestLockoutAfterFailures(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := h.client()

	for i := 0; i < 5; i++ {
		_, err := c.Login(ctx, rbac.Master, "", "wrong")
		mustFail(t, err, "неверная роль")
	}
	// Заблокирован даже верный пароль: иначе перебор просто продолжился бы.
	_, err := c.Login(ctx, rbac.Master, "", "m")
	mustFail(t, err, "слишком много неудачных входов")

	// Блокировка — на пару «адрес, роль»: другая роль доступна.
	h.login("slave", "", "")

	h.clock.Advance(61 * time.Second)
	if _, err := c.Login(ctx, rbac.Master, "", "m"); err != nil {
		t.Fatalf("после блокировки вход не восстановился: %v", err)
	}
}

// set role и back role — внутри сессии: своя роль и своя история, общее
// хранилище сервера они не трогают.
func TestSessionRoles(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := h.login(rbac.Master, "", "m")

	_, err := c.SetRole(ctx, "dan", "", "")
	var need *AuthNeeded
	if !errors.As(err, &need) || !need.Login {
		t.Fatalf("set role в роль с паролем без входа: %v", err)
	}
	_, err = c.SetRole(ctx, "dan", "dan", "x")
	mustFail(t, err, "неверная роль")
	if _, err := c.SetRole(ctx, "dan", "dan", "d"); err != nil {
		t.Fatal(err)
	}

	var roles []string
	for i := 0; i < 3; i++ {
		state, err := c.BackRole(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		roles = append(roles, state.Role)
	}
	if strings.Join(roles, ",") != "master,dan,master" {
		t.Fatalf("back role ходит не по кольцу: %v", roles)
	}

	state, err := c.BackRole(ctx, "dan")
	if err != nil || state.Role != "dan" {
		t.Fatalf("back role dan: %+v, %v", state, err)
	}
	result, err := c.Exec(ctx, []string{"apply", "x"}, "")
	if err != nil || !strings.HasPrefix(result.Stdout, "dan:") {
		t.Fatalf("команда не в роли сессии: %+v, %v", result, err)
	}

	// Хранилище сервера не тронуто: текущая роль там прежняя.
	if h.store.Current != rbac.Master {
		t.Fatalf("сессия изменила текущую роль сервера: %s", h.store.Current)
	}

	// Две сессии независимы.
	other := h.login("slave", "", "")
	if state, _ := other.WhoAmI(ctx); state.Role != "slave" {
		t.Fatalf("сессии перепутались: %+v", state)
	}
}

// Смена пароля выгоняет вошедших и закрывает back role без входа.
func TestCredentialChangeEndsAccess(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	c := h.login("dan", "dan", "d")
	if _, err := c.SetRole(ctx, "slave", "", ""); err != nil {
		t.Fatal(err)
	}

	h.storeMu.Lock()
	password := "new"
	if err := h.store.SetPassword("dan", &password); err != nil {
		t.Fatal(err)
	}
	h.storeMu.Unlock()

	_, err := c.BackRole(ctx, "dan")
	mustFail(t, err, "логин или пароль роли dan сменился")

	// Кто сидел в роли dan, тот из неё выгнан.
	d := h.login("dan", "dan", "new")
	h.storeMu.Lock()
	again := "again"
	if err := h.store.SetPassword("dan", &again); err != nil {
		t.Fatal(err)
	}
	h.storeMu.Unlock()
	_, err = d.Exec(ctx, []string{"diff"}, "")
	mustFail(t, err, "сменился: войдите заново")

	// Удалённая роль перестаёт работать сразу.
	s := h.login("slave", "", "")
	h.storeMu.Lock()
	if err := h.store.Delete("slave"); err != nil {
		t.Fatal(err)
	}
	h.storeMu.Unlock()
	_, err = s.Exec(ctx, []string{"diff"}, "")
	mustFail(t, err, "роли slave на сервере больше нет")
}

func TestSessionTimeout(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := h.login("slave", "", "")

	h.clock.Advance(29 * time.Minute)
	if _, err := c.WhoAmI(ctx); err != nil {
		t.Fatalf("сессия истекла раньше срока: %v", err)
	}
	h.clock.Advance(31 * time.Minute)
	_, err := c.WhoAmI(ctx)
	mustFail(t, err, "сессии нет или она истекла")
}

func TestPlaintextIsLocalOnly(t *testing.T) {
	s := &Server{Logger: log.New(io.Discard, "", 0)}
	err := s.Serve(context.Background(), "0.0.0.0:0", Settings{SSL: false}, false, nil)
	mustFail(t, err, "ssl выключен")

	_, err = Dial("10.0.0.1:7443", Settings{SSL: false}, false)
	mustFail(t, err, "пароль ушёл бы открытым текстом")
	if _, err := Dial("127.0.0.1:7443", Settings{SSL: false}, false); err != nil {
		t.Fatalf("без шифрования к этой машине можно: %v", err)
	}
	if _, err := Dial("10.0.0.1:7443", Settings{SSL: false}, true); err != nil {
		t.Fatalf("--allow-plaintext снимает запрет: %v", err)
	}
}

// Сертификату, которого клиент не знает, он не верит: проверку не
// отключить.
func TestUntrustedCertificate(t *testing.T) {
	h := newHarness(t)
	settings := h.settings
	settings.CA = ""
	c, err := Dial(h.url, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Login(context.Background(), "slave", "", "")
	mustFail(t, err, "укажите его в tls.ca")
}

func TestSettingsValidation(t *testing.T) {
	_, err := ClientTLS(Settings{SSL: true, MinVersion: "1.0"}, "x")
	mustFail(t, err, "допустимо 1.2 или 1.3")
	_, err = ServerTLS(Settings{SSL: true})
	mustFail(t, err, "ochan tls init")
	for _, v := range []string{"", "1.2", "TLS1.3", "1.3"} {
		if _, err := ClientTLS(Settings{SSL: true, MinVersion: v}, "x"); err != nil {
			t.Errorf("версия %q: %v", v, err)
		}
	}
}

// Параллельные входы и команды — для детектора гонок.
func TestConcurrentSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := h.client()
			if _, err := c.Login(ctx, "dan", "dan", "d"); err != nil {
				errs <- err
				return
			}
			if _, err := c.Exec(ctx, []string{"diff"}, ""); err != nil {
				errs <- err
			}
			if _, err := c.SetRole(ctx, "slave", "", ""); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
