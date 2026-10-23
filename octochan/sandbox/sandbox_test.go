package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMain делает тестовый бинарь ребёнком песочницы: Start запускает
// сам исполняемый файл, а в тестах это он.
func TestMain(m *testing.M) {
	if Enter() {
		os.Exit(child())
	}
	os.Exit(m.Run())
}

// probe — попытка ребёнка что-то сделать; итог — "ok" или текст ошибки.
type probeReq struct {
	Name string
	Op   string
	Path string
	Addr string
}

func child() int {
	var probes []probeReq
	if err := json.NewDecoder(os.Stdin).Decode(&probes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	results := map[string]string{}
	for _, p := range probes {
		results[p.Name] = outcome(run(p))
	}
	_ = json.NewEncoder(os.Stdout).Encode(results)
	return 0
}

func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return err.Error()
}

func run(p probeReq) error {
	switch p.Op {
	case "read":
		_, err := os.ReadFile(p.Path)
		return err
	case "write":
		return os.WriteFile(p.Path, []byte("x"), 0o600)
	case "symlink":
		return os.Symlink("/etc/passwd", p.Path)
	case "exec":
		return syscall.Exec(p.Path, []string{p.Path}, nil)
	case "kill":
		return syscall.Kill(1, 0)
	case "unshare":
		return syscall.Unshare(syscall.CLONE_NEWUSER)
	case "socket":
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err == nil {
			syscall.Close(fd)
		}
		return err
	case "dial":
		conn, err := net.Dial("tcp", p.Addr)
		if err == nil {
			conn.Close()
		}
		return err
	case "listen":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			ln.Close()
		}
		return err
	case "caps":
		header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		var data [2]unix.CapUserData
		if err := unix.Capget(&header, &data[0]); err != nil {
			return err
		}
		if data[0].Effective|data[1].Effective|data[0].Permitted|data[1].Permitted != 0 {
			return fmt.Errorf("возможности ядра остались: %x %x", data[0].Effective, data[1].Effective)
		}
		return nil
	case "env":
		if v := os.Getenv(p.Path); v != "" {
			return errors.New("видна: " + v)
		}
		return nil
	}
	return errors.New("неизвестная проба " + p.Op)
}

func spawn(t *testing.T, policy Policy, probes []probeReq) (map[string]string, Report) {
	t.Helper()
	input, _ := json.Marshal(probes)
	var stdout, stderr bytes.Buffer
	c := &Command{Policy: policy, Stdin: bytes.NewReader(input), Stdout: &stdout, Stderr: &stderr}
	proc, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("ребёнок: %v\n%s", err, stderr.String())
	}
	results := map[string]string{}
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("ответ ребёнка %q: %v\n%s", stdout.String(), err, stderr.String())
	}
	return results, proc.Report
}

func requireIsolation(t *testing.T) Report {
	t.Helper()
	report := Probe(Policy{})
	if runtime.GOOS != "linux" || report.Landlock == 0 || !report.Seccomp {
		t.Skipf("ядро не даёт полной изоляции: %s", strings.Join(report.Gaps(), "; "))
	}
	return report
}

func TestKernelEnforcesPolicy(t *testing.T) {
	report := requireIsolation(t)
	root, outside, library := t.TempDir(), t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	os.WriteFile(secret, []byte("пароль"), 0o600)
	book := filepath.Join(library, "book")
	os.WriteFile(book, []byte("книга"), 0o600)
	t.Setenv("OCHAN_TEST_SECRET", "токен")

	results, got := spawn(t, Policy{Write: []string{root}, Read: []string{library}}, []probeReq{
		{Name: "запись внутри", Op: "write", Path: filepath.Join(root, "out")},
		{Name: "чтение записанного", Op: "read", Path: filepath.Join(root, "out")},
		{Name: "чтение библиотеки", Op: "read", Path: book},
		{Name: "запись в библиотеку", Op: "write", Path: filepath.Join(library, "new")},
		{Name: "чтение снаружи", Op: "read", Path: secret},
		{Name: "запись снаружи", Op: "write", Path: filepath.Join(outside, "new")},
		{Name: "passwd", Op: "read", Path: "/etc/passwd"},
		{Name: "proc", Op: "read", Path: "/proc/self/environ"},
		{Name: "ссылка", Op: "symlink", Path: filepath.Join(root, "link")},
		{Name: "exec", Op: "exec", Path: "/bin/true"},
		{Name: "kill", Op: "kill"},
		{Name: "unshare", Op: "unshare"},
		{Name: "socket", Op: "socket"},
		{Name: "окружение", Op: "env", Path: "OCHAN_TEST_SECRET"},
		{Name: "привилегии", Op: "caps"},
	})

	want := map[string]string{
		"запись внутри":       "ok",
		"чтение записанного":  "ok",
		"чтение библиотеки":   "ok",
		"запись в библиотеку": "permission denied",
		"чтение снаружи":      "permission denied",
		"запись снаружи":      "permission denied",
		"passwd":              "permission denied",
		"proc":                "permission denied",
		"ссылка":              "permission denied",
		"exec":                "operation not permitted",
		"kill":                "operation not permitted",
		"unshare":             "operation not permitted",
		"socket":              "operation not permitted",
		"окружение":           "ok",
		"привилегии":          "ok",
	}
	for name, expected := range want {
		if results[name] != expected {
			t.Errorf("%s: %q, ожидалось %q", name, results[name], expected)
		}
	}
	if got.Landlock != report.Landlock || !got.Seccomp || !got.NetworkClosed() {
		t.Errorf("отчёт: %+v", got)
	}
	if !got.Full() {
		t.Errorf("изоляция неполная: %v", got.Gaps())
	}
}

func TestNetworkClosedUnlessAllowed(t *testing.T) {
	report := requireIsolation(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	root := t.TempDir()
	probes := []probeReq{
		{Name: "dial", Op: "dial", Addr: ln.Addr().String()},
		{Name: "listen", Op: "listen"},
	}

	closed, _ := spawn(t, Policy{Write: []string{root}}, probes)
	if closed["dial"] == "ok" || closed["listen"] == "ok" {
		t.Errorf("без сети соединение прошло: %v", closed)
	}

	open, got := spawn(t, Policy{Write: []string{root}, Network: true}, probes)
	if open["dial"] != "ok" {
		t.Errorf("с сетью соединение не прошло: %v", open)
	}
	if report.Landlock >= 4 && open["listen"] == "ok" {
		t.Errorf("слушать порт сценарию нельзя и с сетью: %v", open)
	}
	if got.NetNS || got.NetworkClosed() {
		t.Errorf("с разрешённой сетью отчёт говорит, что она закрыта: %+v", got)
	}
}

func TestEnterIgnoresOrdinaryRun(t *testing.T) {
	saved := os.Args
	defer func() { os.Args = saved }()
	// Без переменной окружения аргумент — просто аргумент.
	os.Args = []string{"ochan", childArg, "2", "{}"}
	if Enter() {
		t.Fatal("Enter принял обычный запуск за ребёнка песочницы")
	}
}

func TestGapsOnOtherSystems(t *testing.T) {
	gaps := Report{OS: "darwin"}.Gaps()
	if len(gaps) != 1 || !strings.Contains(gaps[0], "только песочница языка") {
		t.Errorf("darwin: %v", gaps)
	}
	if !(Report{OS: "linux", Landlock: 7, Seccomp: true}).Full() {
		t.Error("Linux с Landlock и seccomp — полная изоляция")
	}
	gaps = Report{OS: "linux", Seccomp: true}.Gaps()
	if len(gaps) != 1 || !strings.Contains(gaps[0], "Landlock") {
		t.Errorf("без Landlock: %v", gaps)
	}
}
