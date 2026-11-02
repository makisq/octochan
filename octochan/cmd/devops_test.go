package cmd

import (
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// text grep, fields и tail: шаблон, поля, хвост большого файла; шаблон
// grep — не путь для области роли; из сценария без файла и с -f — отказ.
func TestTextCommands(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("app.log"), "2026-11-01 INFO start\n2026-11-01 ERROR conn refused db7\n2026-11-01 WARN slow /var/lib\n"+
		"2026-11-01 error timeout db8\n")
	e.write(e.path("old.log"), "2026-10-31 ERROR disk full\n")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"grep", "ERROR", "app.log"}, "2026-11-01 ERROR conn refused db7\n"},
		{[]string{"grep", "-i", "-n", "error", "app.log"}, "2:2026-11-01 ERROR conn refused db7\n4:2026-11-01 error timeout db8\n"},
		{[]string{"grep", "-c", "-v", "ERROR", "app.log"}, "3\n"},
		{[]string{"grep", "-F", "/var/lib", "app.log"}, "2026-11-01 WARN slow /var/lib\n"},
		{[]string{"grep", "ERROR", "*.log"}, "app.log:2026-11-01 ERROR conn refused db7\nold.log:2026-10-31 ERROR disk full\n"},
		{[]string{"grep", "-m", "1", "-i", "error", "app.log"}, "2026-11-01 ERROR conn refused db7\n"},
		{[]string{"fields", "-f", "2,4-", "--match", "(?i)error", "app.log"}, "ERROR refused db7\nerror db8\n"},
		{[]string{"fields", "-d", " ", "-s", ",", "-f", "1-2", "old.log"}, "2026-10-31,ERROR\n"},
		{[]string{"tail", "-n", "2", "app.log"}, "2026-11-01 WARN slow /var/lib\n2026-11-01 error timeout db8\n"},
	} {
		if out := e.must(append([]string{"text"}, c.args...)...); out != c.want {
			t.Errorf("text %s:\n%q\nожидалось\n%q", strings.Join(c.args, " "), out, c.want)
		}
	}
	e.fails("номера полей с 1", "text", "fields", "-f", "0", "app.log")
	e.fails("шаблон", "text", "grep", "(", "app.log")

	// Хвост большого файла читается с конца кусками.
	var big strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&big, "строка %d\n", i)
	}
	e.write(e.path("big.log"), big.String())
	if out := e.must("text", "tail", "-n", "3", "big.log"); out != "строка 19998\nстрока 19999\nстрока 20000\n" {
		t.Fatalf("хвост большого файла: %q", out)
	}

	// Из сценария: без файла — не stdin, -f — отказ, остальное — вывод.
	e.write(e.path("s.ochan"), `print(ochan("text grep -c ERROR app.log"))`+"\n")
	if out := e.must("run", e.path("s.ochan")); out != "1\n" {
		t.Fatalf("из сценария: %q", out)
	}
	for src, want := range map[string]string{
		`ochan("text tail -f app.log")`: "text tail -f из сценария нельзя",
		`ochan("text grep ERROR")`:      "укажите файл",
	} {
		e.write(e.path("bad.ochan"), src+"\n")
		if _, err := e.ochan("run", e.path("bad.ochan")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", src, err)
		}
	}

	// Роль с областью: шаблон «/var/lib» — не путь, а файл вне области — да.
	inside := filepath.Join(e.work, "team")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(inside, "a.log"), "WARN slow /var/lib\n")
	e.must("role", "create", "dan", "--allow", "read")
	e.must("role", "scope", "dan", inside)
	t.Setenv(EnvRole, "dan")
	if err := os.Chdir(inside); err != nil {
		t.Fatal(err)
	}
	if out := e.must("text", "grep", "/var/lib", "a.log"); out != "WARN slow /var/lib\n" {
		t.Fatalf("шаблон с / у роли с областью: %q", out)
	}
	e.fails("вне области роли dan", "text", "grep", "ERROR", e.path("app.log"))
}

// Сравнение одно: конфиги — по смыслу, остальное — построчно, как diff -u.
func TestDiffLines(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("a.log"), "2026-11-01 ERROR conn refused\n2026-11-01 INFO ok\n2026-11-01 INFO done\n")
	e.write(e.path("b.log"), "2026-11-01 ERROR conn refused\n2026-11-01 WARN slow\n2026-11-01 INFO done\n")
	out := e.must("diff", "a.log", "b.log")
	want := "--- a.log\n+++ b.log\n@@ -1,3 +1,3 @@\n 2026-11-01 ERROR conn refused\n-2026-11-01 INFO ok\n+2026-11-01 WARN slow\n" +
		" 2026-11-01 INFO done\n"
	if !strings.Contains(out, "a.log и b.log — не конфиг: сравниваю построчно") || !strings.HasSuffix(out, want) {
		t.Fatalf("журналы:\n%s", out)
	}
	if out := e.must("diff", "a.log", "a.log"); !strings.Contains(out, "Источники идентичны") {
		t.Fatalf("одинаковые:\n%s", out)
	}

	// Конфиги — по смыслу, как раньше; --lines — построчно и их.
	e.write(e.path("test.conf"), "max_connections = 200\nwork_mem = 4096kB\n")
	if out := e.must("diff", "--fast", "prod.conf", "test.conf"); !strings.Contains(out, "max_connections: 100 -> 200") ||
		strings.Contains(out, "work_mem") {
		t.Fatalf("конфиги по смыслу:\n%s", out)
	}
	if out := e.must("diff", "--lines", "-U", "0", "prod.conf", "test.conf"); !strings.Contains(out,
		"@@ -1,2 +1,2 @@\n-max_connections = 100\n-work_mem = 4MB\n+max_connections = 200\n+work_mem = 4096kB\n") {
		t.Fatalf("конфиги построчно:\n%s", out)
	}
	e.fails("--lines и --config вместе нельзя", "diff", "--lines", "--config", "a.log", "b.log")
}

// Построчные правки: вставка в начало, удаление в конце, два куска.
func TestUnifiedHunks(t *testing.T) {
	a := strings.Split("a b c d e f g h i j", " ")
	b := strings.Split("x a b c d e f g h j", " ")
	edits, err := diffLines(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a\n+++ b\n@@ -1 +1,2 @@\n+x\n a\n@@ -8,3 +9,2 @@\n h\n-i\n j\n"
	if got := unified("a", "b", edits, 1); got != want {
		t.Fatalf("куски:\n%s\nожидалось\n%s", got, want)
	}
	// Без контекста вставка в начало — «после строки 0».
	if got := unified("a", "b", edits, 0); !strings.HasPrefix(got, "--- a\n+++ b\n@@ -0,0 +1 @@\n+x\n@@ -9 +9,0 @@\n-i\n") {
		t.Fatalf("без контекста:\n%s", got)
	}
	if !configLike("[db]\nmax_connections = 100\n# c\ninclude 'x.conf'\n") || configLike("2026 ERROR x\nline\n") {
		t.Fatal("configLike")
	}
}

// net port, dns, http и tls; право net выдают явно.
func TestNetCommands(t *testing.T) {
	e := newScriptEnv(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	open := listener.Addr().String()
	gone, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := gone.Addr().String()
	gone.Close()
	if out := e.must("net", "port", open); !strings.Contains(out, open) || !strings.Contains(out, "открыт") {
		t.Fatalf("открытый порт:\n%s", out)
	}
	if out, err := e.ochan("net", "port", open, closed, "--timeout", "1s"); err == nil ||
		!strings.Contains(err.Error(), "не отвечает 1 из 2: "+closed) || !strings.Contains(out, "закрыт") {
		t.Fatalf("закрытый порт: %v\n%s", err, out)
	}
	e.fails("ожидалось хост:порт", "net", "port", "db7")
	if out := e.must("net", "dns", "localhost"); !strings.Contains(out, "127.0.0.1") && !strings.Contains(out, "::1") {
		t.Fatalf("dns localhost:\n%s", out)
	}
	e.fails("A, AAAA, CNAME", "net", "dns", "localhost", "--type", "BOGUS")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	if out := e.must("net", "http", srv.URL+"/health", "--body"); !strings.HasPrefix(out, "200 OK") ||
		!strings.Contains(out, `{"status":"ok"}`) {
		t.Fatalf("http:\n%s", out)
	}
	e.fails("код 503", "net", "http", srv.URL+"/down")
	e.fails("код 200, ожидался 201", "net", "http", srv.URL, "--expect", "201")

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer tlsSrv.Close()
	e.write(e.path("ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw})))
	e.fails("certificate", "net", "http", tlsSrv.URL)
	if out := e.must("net", "http", tlsSrv.URL, "--ca", e.path("ca.pem")); !strings.Contains(out, "сертификат до") {
		t.Fatalf("https со своим CA:\n%s", out)
	}
	addr := strings.TrimPrefix(tlsSrv.URL, "https://")
	if out := e.must("net", "tls", addr, "--ca", e.path("ca.pem")); !strings.Contains(out, "выдал:") ||
		!strings.Contains(out, "имена:") || !strings.Contains(out, "TLS:") {
		t.Fatalf("tls:\n%s", out)
	}
	if out, err := e.ochan("net", "tls", addr); err == nil || !strings.Contains(err.Error(), "не прошёл проверку") ||
		!strings.Contains(out, "кому:") {
		t.Fatalf("непроверенный сертификат: %v\n%s", err, out)
	}
	e.fails("меньше --warn", "net", "tls", addr, "--ca", e.path("ca.pem"), "--warn", "100000d")

	// Из сценария: читающая — выполняется и в плане.
	e.write(e.path("p.ochan"), fmt.Sprintf(`print(ochan("net port %s"))`, open)+"\n")
	if out := e.must("plan", "--script", e.path("p.ochan")); !strings.Contains(out, "открыт") {
		t.Fatalf("net в плане:\n%s", out)
	}

	// Право net — отдельное: read его не даёт; llm получает его от master.
	e.must("role", "create", "reader", "--allow", "read")
	t.Setenv(EnvRole, "reader")
	e.fails("нужно право net", "net", "port", open)
	t.Setenv(EnvRole, "")
	if out := e.must("role", "show", "llm"); strings.Contains(out, "net") {
		t.Fatalf("у llm после установки — net:\n%s", out)
	}
	e.must("role", "grant", "llm", "net")
}
