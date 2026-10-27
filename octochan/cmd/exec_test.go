package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/sandbox"
)

// Запуск программ из сценария: только в песочнице ядра, только
// перечисленных, заявка выполняется той же программой, что видел план.
func TestAllowExec(t *testing.T) {
	if missing := sandbox.Probe(sandbox.Policy{}).ExecReady(); len(missing) > 0 {
		t.Skipf("ядро не даёт запуска программ в песочнице: %s", strings.Join(missing, "; "))
	}
	data, err := os.ReadFile("/usr/bin/uname")
	if err != nil {
		t.Skip("нет /usr/bin/uname")
	}
	e := newScriptEnv(t)
	tool := filepath.Join(t.TempDir(), "myuname")
	if err := os.WriteFile(tool, data, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(e.path("os.ochan"), `write("os.txt", exec("myuname", "-s"))`+"\n")

	e.fails("действуют только вместе с --sandbox", "run", "--allow-exec", tool, "os.ochan")
	e.fails("оболочки, интерпретаторы", "run", "--sandbox", "--allow-exec", "sh", "os.ochan")
	e.fails("exec myuname: запуск программ — только в песочнице ядра", "run", "os.ochan")

	out := e.must("plan", "--sandbox", "--allow-exec", tool, "os.ochan")
	if !strings.Contains(out, "запуск     myuname -s — в плане не запускается; не откатывается") {
		t.Fatalf("план:\n%s", out)
	}
	if _, err := os.Stat(e.path("os.txt")); !os.IsNotExist(err) {
		t.Fatal("план записал файл")
	}
	e.must("run", "--sandbox", "--allow-exec", tool, "os.ochan")
	if got := e.read("os.txt"); !strings.HasPrefix(got, "Linux") {
		t.Fatalf("вывод программы: %q", got)
	}

	id := requestIDPattern.FindStringSubmatch(e.must("request", "--sandbox", "--allow-exec", tool, "os.ochan"))[1]
	if card := e.must("requests", id); !strings.Contains(card, "запуск myuname ("+tool+")") {
		t.Fatalf("карточка без программы:\n%s", card)
	}
	e.must("approve", id)
	// Программу подменили после согласования — запуска нет.
	if err := os.WriteFile(tool, append(append([]byte(nil), data...), 0), 0o755); err != nil {
		t.Fatal(err)
	}
	e.fails("программа myuname ("+tool+") изменилась после согласования", "apply", "--request", id)
	if err := os.WriteFile(tool, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if out := e.must("apply", "--request", id); !strings.Contains(out, "записано файлов: 1") {
		t.Fatalf("apply:\n%s", out)
	}
}
