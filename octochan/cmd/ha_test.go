package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"octochan/remote"
	"octochan/workspace"
)

// haTestNode — узел пары: отдельный процесс ochan serve со своим рабочим
// каталогом, как на отдельной машине.
type haTestNode struct {
	t    *testing.T
	name string
	home string
	dir  string
	addr string
	peer string
	cmd  *exec.Cmd
	log  *strings.Builder
	mu   sync.Mutex
}

func (n *haTestNode) base() string { return filepath.Join(n.home, ".octochan") }

func (n *haTestNode) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		for _, drop := range []string{"HOME=", "USERPROFILE=", "OCTOCHAN_"} {
			if strings.HasPrefix(kv, drop) {
				kv = ""
			}
		}
		if kv != "" {
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+n.home, "USERPROFILE="+n.home, "OCHAN_TEST_AS_OCHAN=1",
		"OCTOCHAN_CONFIG="+filepath.Join(n.base(), "config.yaml"))
}

// ochan выполняет команду ochan на этом узле, как оператор в его консоли.
func (n *haTestNode) ochan(args ...string) (string, error) {
	self, _ := os.Executable()
	c := exec.Command(self, args...)
	c.Env, c.Dir = n.env(), n.dir
	out, err := c.CombinedOutput()
	return string(out), err
}

func (n *haTestNode) start() {
	n.t.Helper()
	self, _ := os.Executable()
	n.log = &strings.Builder{}
	n.cmd = exec.Command(self, "serve", "--listen", n.addr, "--dir", n.dir)
	n.cmd.Env, n.cmd.Dir = n.env(), n.dir
	n.cmd.Stdout, n.cmd.Stderr = &lockedWriter{b: n.log, mu: &n.mu}, &lockedWriter{b: n.log, mu: &n.mu}
	if err := n.cmd.Start(); err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(n.stop)
	n.waitFor("узел отвечает", func() bool {
		_, err := remote.NodeStatus(context.Background(), n.addr, remote.Settings{}, false)
		return err == nil
	})
}

func (n *haTestNode) stop() {
	if n.cmd != nil && n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
		_ = n.cmd.Wait()
		n.cmd = nil
	}
}

func (n *haTestNode) status() *remote.HAStatus {
	st, err := remote.NodeStatus(context.Background(), n.addr, remote.Settings{}, false)
	if err != nil {
		return &remote.HAStatus{Error: err.Error()}
	}
	return st
}

func (n *haTestNode) waitFor(what string, ok func() bool) {
	n.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.t.Fatalf("узел %s: не дождались — %s\n%s", n.name, what, n.log)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type lockedWriter struct {
	b  *strings.Builder
	mu *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// Пара серверов: копия на standby, запись только на primary, барьер перед
// выполнением, переключение человеком, отступление бывшего primary.
func TestServerPair(t *testing.T) {
	if testing.Short() {
		t.Skip("два процесса сервера")
	}
	a := &haTestNode{t: t, name: "a", home: t.TempDir(), dir: t.TempDir(), addr: freePort(t)}
	b := &haTestNode{t: t, name: "b", home: t.TempDir(), dir: t.TempDir(), addr: freePort(t)}
	a.peer, b.peer = b.addr, a.addr
	for i, n := range []*haTestNode{a, b} {
		if err := os.MkdirAll(n.base(), 0o700); err != nil {
			t.Fatal(err)
		}
		config := fmt.Sprintf("ssl: false\nha:\n  peer: %s\n  self: %s\n  replicate_every: 1s\n", n.peer, n.addr)
		if err := os.WriteFile(filepath.Join(n.base(), "config.yaml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(n.base(), "ha.token"), []byte(strings.Repeat("7", 64)), 0o600); err != nil {
			t.Fatal(err)
		}
		role := []string{"primary", "standby"}[i]
		if out, err := n.ochan("ha", "init", "--node", n.name, "--role", role); err != nil {
			t.Fatalf("ha init %s: %v\n%s", n.name, err, out)
		}
		if err := os.WriteFile(filepath.Join(n.dir, "prod.conf"), []byte("max_connections = 100\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(n.dir, "changes.conf"), []byte("max_connections = 200\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a.start()
	b.start()

	ctx := context.Background()
	pool, err := remote.DialPool(ctx, []string{b.addr, a.addr}, remote.Settings{}, false)
	if err != nil {
		t.Fatal(err)
	}
	var switched []string
	pool.OnSwitch = func(from, to string) { switched = append(switched, from+"→"+to) }
	if pool.Primary() != a.addr {
		t.Fatalf("primary: %s", pool.Primary())
	}
	if _, err := pool.Login(ctx, "master", "", ""); err != nil {
		t.Fatal(err)
	}
	run := func(readOnly bool, args ...string) string {
		t.Helper()
		res, err := pool.Exec(ctx, args, "", readOnly)
		if err != nil || res.Code != 0 {
			t.Fatalf("%s: %v\n%s%s", strings.Join(args, " "), err, res.Stdout, res.Stderr)
		}
		return res.Stdout + res.Stderr
	}
	id := regexp.MustCompile(`Заявка ([0-9a-f]+) создана`).FindStringSubmatch(run(false, "request", "prod.conf", "changes.conf"))[1]
	b.waitFor("копия заявки", func() bool {
		_, err := os.Stat(filepath.Join(b.base(), "requests", id+".json"))
		return err == nil
	})

	// Чтение — на любой узел; запись на standby — отказ с адресом primary.
	pool.Balance = true
	for i := 0; i < 2; i++ {
		if out := run(true, "requests"); !strings.Contains(out, id) {
			t.Fatalf("чтение %d:\n%s", i, out)
		}
	}
	pool.Balance = false
	direct, _ := remote.Dial(b.addr, remote.Settings{}, false)
	if _, err := direct.Login(ctx, "master", "", ""); err != nil {
		t.Fatal(err)
	}
	var notPrimary *remote.NotPrimary
	if _, err := direct.Exec(ctx, []string{"approve", id}, ""); !errors.As(err, &notPrimary) || notPrimary.Primary != a.addr {
		t.Fatalf("запись на standby: %v", err)
	}

	// Выполнение: пометка доходит до standby раньше действия.
	run(false, "approve", id)
	if out := run(false, "apply", "--request", id); !strings.Contains(out, "выполнена") || strings.Contains(out, "внимание") {
		t.Fatalf("apply:\n%s", out)
	}

	// Заявка, которая «выполнялась» на a, когда его не стало.
	ghost := &workspace.Request{ID: "0badc0de00", State: workspace.RequestRunning, Created: time.Now().UTC(),
		Summary: "выполнялась на a", Source: "x", Runner: &workspace.RunnerMark{Host: "a-host", PID: 1, Node: "a", Epoch: 1}}
	store, _ := workspace.NewRequests(filepath.Join(a.base(), "requests"))
	if err := store.Create(ghost); err != nil {
		t.Fatal(err)
	}
	b.waitFor("копия выполняемой заявки", func() bool {
		_, err := os.Stat(filepath.Join(b.base(), "requests", ghost.ID+".json"))
		return err == nil
	})

	// a пропал. Пока человек не повысил b, к нему подключаются для чтения:
	// он отвечает по копии, а запись отклоняет.
	a.stop()
	standby, err := remote.DialPool(ctx, []string{a.addr, b.addr}, remote.Settings{}, false)
	if err != nil {
		t.Fatalf("подключение к одному standby: %v", err)
	}
	if standby.Primary() != b.addr || !strings.Contains(standby.ReadOnly(), "primary не найден") {
		t.Fatalf("пул без primary: %s, %q", standby.Primary(), standby.ReadOnly())
	}
	if _, err := standby.Login(ctx, "master", "", ""); err != nil {
		t.Fatal(err)
	}
	if res, err := standby.Exec(ctx, []string{"requests"}, "", true); err != nil || !strings.Contains(res.Stdout, id) {
		t.Fatalf("чтение со standby: %v\n%+v", err, res)
	}
	if _, err := standby.Exec(ctx, []string{"approve", id}, "", false); !errors.As(err, &notPrimary) {
		t.Fatalf("запись на standby без primary: %v", err)
	}

	out, err := b.ochan("ha", "promote")
	if err != nil || !strings.Contains(out, "Узел b — primary, эпоха 2") || !strings.Contains(out, "Заявка 0badc0de00 упала") {
		t.Fatalf("promote: %v\n%s", err, out)
	}
	b.waitFor("b стал primary", func() bool { return b.status().Writable() })

	// Клиент переживает переключение сам: запись уходит на b.
	if out := run(false, "request", "prod.conf", "changes.conf"); !strings.Contains(out, "создана") {
		t.Fatalf("запись после переключения:\n%s", out)
	}
	if len(switched) != 1 || switched[0] != a.addr+"→"+b.addr || pool.Primary() != b.addr {
		t.Fatalf("переключение клиента: %v, primary %s", switched, pool.Primary())
	}

	// a вернулся, считая себя primary эпохи 1: увидев эпоху 2, он отступает,
	// а записанное только у него откладывает.
	a.start()
	a.waitFor("a отступил", func() bool { st := a.status(); return st.Role == "standby" && st.Epoch == 2 })
	if st := a.status(); len(st.Diverged) != 1 {
		t.Fatalf("расходящееся состояние: %+v", st)
	}
	if out, _ := a.ochan("ha", "status"); !strings.Contains(out, "Отложено расходящееся состояние: ha/diverged-e1-") {
		t.Fatalf("ha status на a:\n%s", out)
	}
}
