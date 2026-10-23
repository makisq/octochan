package cmd

import (
	"os"
	"strings"
	"testing"
)

// Согласованная правка конфига выполняется apply --request: ровно план
// заявки, в тот файл, где её согласовали, с откатом. Если файл поменяли
// после согласования, выполнение отменяется.
func TestConfigRequestLifecycle(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("changes.conf"), "max_connections = 200\nwork_mem = 8MB\nstatement_timeout = 30s\n")

	id := requestIDPattern.FindStringSubmatch(e.must("request", "prod.conf", "changes.conf"))[1]
	e.must("approve", id)

	// Кто-то поправил параметр, который правка меняет.
	original := e.read("prod.conf")
	e.write(e.path("prod.conf"), "max_connections = 150\nwork_mem = 4MB\n")
	e.fails("max_connections: при согласовании 100, сейчас 150", "apply", "--request", id)
	e.write(e.path("prod.conf"), original)

	// Выполнять можно из другого каталога: путь записан в заявке.
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	out := e.must("apply", "--request", id)
	if !strings.Contains(out, "изменено параметров: 3") {
		t.Fatalf("apply:\n%s", out)
	}
	got := e.read("prod.conf")
	for _, want := range []string{"max_connections = 200", "work_mem = 8MB", "statement_timeout = 30s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prod.conf без %q:\n%s", want, got)
		}
	}
	e.fails("не согласована (выполнена)", "apply", "--request", id)

	entry := rollbackHint.FindStringSubmatch(out)[1]
	e.must("rollback", entry)
	if got := e.read("prod.conf"); !strings.Contains(got, "max_connections = 100") || strings.Contains(got, "statement_timeout") {
		t.Fatalf("откат не вернул prod.conf:\n%s", got)
	}
}
