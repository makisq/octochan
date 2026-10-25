package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/rbac"
	"octochan/remote"
	"octochan/sandbox"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// TestMain позволяет тестовому бинарю быть ochan: сервер и песочница
// запускают дочерним процессом самого себя, и в тестах это тестовый бинарь.
func TestMain(m *testing.M) {
	if sandbox.Enter() {
		os.Exit(SandboxChild())
	}
	if os.Getenv("OCHAN_TEST_AS_OCHAN") == "1" {
		os.Exit(Execute())
	}
	os.Exit(m.Run())
}

type serverEnv struct {
	*scriptEnv
	addr string
	log  *bytes.Buffer
}

// startServer поднимает сервер с TLS на 127.0.0.1. Роли: master с паролем
// m, slave (read, plan), dan с логином dan и паролем d (read, plan, apply).
func startServer(t *testing.T) *serverEnv {
	t.Helper()
	e := newScriptEnv(t)
	t.Setenv("OCHAN_TEST_AS_OCHAN", "1")

	typed(t, "m\n")
	e.must("role", "update", "master", "--npsw")
	e.must("role", "create", "slave", "--allow", "read,plan")
	typed(t, "d\n")
	e.must("role", "create", "dan", "--allow", "read,plan,apply", "--nlogin", "dan", "--npsw")

	out := e.must("tls", "init")
	if !strings.Contains(out, "Отпечаток:") {
		t.Fatalf("tls init:\n%s", out)
	}
	e.fails("уже есть", "tls", "init")

	viper.Set("tls.ca", viper.GetString("tls.cert"))
	t.Cleanup(func() { viper.Set("tls.ca", "") })

	if err := mutateRoles(func(s *rbac.Store) error { return s.RotateServerKey() }); err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	srv := newRemoteServer(e.work, log.New(logs, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, "127.0.0.1:0", tlsSettings(), false, func(url string) { ready <- url })
	}()
	var addr string
	select {
	case url := <-ready:
		addr = strings.TrimPrefix(url, "https://")
	case err := <-done:
		t.Fatalf("сервер не запустился: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &serverEnv{scriptEnv: e, addr: addr, log: logs}
}

func (s *serverEnv) client(role, login, password string) *remote.Client {
	s.t.Helper()
	c, err := remote.Dial(s.addr, tlsSettings(), false)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := c.Login(context.Background(), role, login, password); err != nil {
		s.t.Fatalf("вход в %s: %v", role, err)
	}
	return c
}

func (s *serverEnv) exec(c *remote.Client, args ...string) *remote.Result {
	s.t.Helper()
	result, err := c.Exec(context.Background(), args, "")
	if err != nil {
		s.t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
	return result
}

// Команда выполняется на сервере, отдельным процессом ochan, в роли сессии:
// и в роли с паролем, которую окружением иначе не задать.
func TestServeRunsCommandsInSessionRole(t *testing.T) {
	s := startServer(t)
	ctx := context.Background()

	slave := s.client("slave", "", "")
	if result := s.exec(slave, "plan", "tune.ochan"); result.Code != 0 || !strings.Contains(result.Stdout, "изменить   max_connections") {
		t.Fatalf("plan под slave: %+v", result)
	}
	_, err := slave.Exec(ctx, []string{"run", "tune.ochan"}, "")
	if err == nil || !strings.Contains(err.Error(), "роль slave не может выполнять run: нужно право apply") {
		t.Fatalf("run под slave: %v", err)
	}
	if !strings.Contains(s.read("prod.conf"), "max_connections = 100") {
		t.Fatal("отказанная команда всё-таки выполнилась")
	}

	dan := s.client("dan", "dan", "d")
	if result := s.exec(dan, "run", "tune.ochan"); result.Code != 0 {
		t.Fatalf("run под dan: %+v", result)
	}
	if !strings.Contains(s.read("prod.conf"), "max_connections = 200") {
		t.Fatalf("сценарий на сервере не выполнен:\n%s", s.read("prod.conf"))
	}

	// Ошибка команды — код и stderr, а не отказ протокола.
	if result := s.exec(dan, "validate", "нет-такого.conf"); result.Code == 0 || result.Stderr == "" {
		t.Fatalf("ошибка команды потерялась: %+v", result)
	}

	for args, want := range map[string]string{
		"--config /tmp/x.yaml diff a b": "флаг --config на сервере недоступен",
		"shell":                         "интерактивной оболочки на сервере нет",
		"serve":                         "сервер уже запущен",
		"set role master":               "роль сессии переключает клиент",
		"nosuchcommand":                 "неизвестная команда",
	} {
		_, err := dan.Exec(ctx, strings.Fields(args), "")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: ожидался отказ с %q, получено %v", args, want, err)
		}
	}

	if !strings.Contains(s.log.String(), "dan@127.0.0.1: run tune.ochan → код 0") {
		t.Errorf("в журнале сервера нет выполнения:\n%s", s.log.String())
	}
}

func TestServeLogin(t *testing.T) {
	s := startServer(t)
	c, err := remote.Dial(s.addr, tlsSettings(), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err = c.Login(ctx, "master", "", "")
	var need *remote.AuthNeeded
	if !errors.As(err, &need) || !need.Password || need.Login {
		t.Fatalf("master без пароля: %v", err)
	}
	if _, err := c.Login(ctx, "master", "", "wrong"); err == nil {
		t.Fatal("неверный пароль принят")
	}
	if _, err := c.Login(ctx, "master", "", "m"); err != nil {
		t.Fatal(err)
	}
}

// Интерактивная сессия: роль, set role со входом, back role по кольцу,
// смена пароля роли через --npsw, который спрашивает клиент.
func TestConnectSession(t *testing.T) {
	s := startServer(t)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	typed(t, "")

	master := s.client("master", "", "m")
	input := bufio.NewReader(strings.NewReader(strings.Join([]string{
		"role",
		"set role dan", "dan", "d",
		"back role",
		"back role",
		"run tune.ochan",
		"back role master",
		"role update slave --npsw", "slave-pw",
		"diff -",
		"exit",
	}, "\n") + "\n"))
	if err := runSession(cmd, master, s.addr, input); err != nil {
		t.Fatal(err)
	}

	text := out.String()
	for _, want := range []string{
		"Текущая роль: master",
		"Логин: Пароль: Роль: dan",
		"Роль: master\n",
		"Роль: dan\n",
		"ochan@" + s.addr + "[dan]> ",
		"Роль slave обновлена",
		"stdin в интерактивной сессии не передаётся",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("в сессии нет %q:\n%s", want, text)
		}
	}

	// Новый пароль slave — на сервере.
	c, err := remote.Dial(s.addr, tlsSettings(), false)
	if err != nil {
		t.Fatal(err)
	}
	var need *remote.AuthNeeded
	if _, err := c.Login(context.Background(), "slave", "", ""); !errors.As(err, &need) {
		t.Fatalf("у slave нет пароля после role update --npsw: %v", err)
	}
	if _, err := c.Login(context.Background(), "slave", "", "slave-pw"); err != nil {
		t.Fatal(err)
	}
}

// Сертификат нужен, а без ssl к другой машине клиент не подключится.
func TestTLSSettingsFromConfig(t *testing.T) {
	e := newScriptEnv(t)
	t.Cleanup(func() { viper.Set("ssl", true) })

	viper.Set("ssl", false)
	e.fails("пароль ушёл бы открытым текстом", "remote", "10.0.0.1:7443", "diff", "a", "b")

	viper.Set("ssl", true)
	viper.Set("tls.min_version", "1.1")
	t.Cleanup(func() { viper.Set("tls.min_version", "1.2") })
	e.fails("допустимо 1.2 или 1.3", "remote", "127.0.0.1:7443", "diff", "a", "b")

	if _, err := os.Stat(filepath.Join(e.home, ".octochan", "tls")); err == nil {
		t.Fatal("сертификат появился без tls init")
	}
}

// Клиент сервера с правом apply не дотягивается ни до файлов вне каталога
// сервера, ни до хранилища ролей с ключом подписи допусков: иначе он
// подделал бы допуск master.
func TestServerConfinesRemoteRoles(t *testing.T) {
	s := startServer(t)
	dan := s.client("dan", "dan", "d")

	outside := filepath.Join(t.TempDir(), "secret.conf")
	s.write(outside, "password = x\n")
	if r := s.exec(dan, "print", outside); r.Code == 0 || !strings.Contains(r.Stderr, "вне области роли dan") {
		t.Fatalf("файл вне каталога сервера: %+v", r)
	}

	roles := filepath.Join(s.home, ".octochan", "roles.bin")
	s.write(s.path("steal.ochan"), "print(read(\""+filepath.ToSlash(roles)+"\"))\n")
	if r := s.exec(dan, "run", "steal.ochan"); r.Code == 0 || r.Stdout != "" {
		t.Fatalf("сценарий клиента прочитал хранилище ролей: %+v", r)
	}
	if r := s.exec(dan, "run", "--sandbox", "--root", s.home, "steal.ochan"); r.Code == 0 ||
		!strings.Contains(r.Stderr, "вне области роли dan") {
		t.Fatalf("песочница поверх домашнего каталога: %+v", r)
	}
	// Сценарий в каталоге сервера работает: песочница ставится сама.
	if r := s.exec(dan, "run", "tune.ochan"); r.Code != 0 {
		t.Fatalf("сценарий в каталоге сервера: %+v", r)
	}

	// Неудачный вход, отказ сервера и команда клиента — в журнале аудита,
	// с адресом клиента.
	c, err := remote.Dial(s.addr, tlsSettings(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Login(context.Background(), "dan", "dan", "не тот"); err == nil {
		t.Fatal("вход с неверным паролем прошёл")
	}
	_, _ = dan.Exec(context.Background(), []string{"serve"}, "")

	journal := s.must("audit", "-n", "0")
	for _, want := range []string{"login", "неверная роль, логин или пароль", "127.0.0.1", "сервер уже запущен", "run tune.ochan"} {
		if !strings.Contains(journal, want) {
			t.Fatalf("в журнале нет %q:\n%s", want, journal)
		}
	}
	s.must("audit", "verify")
}
