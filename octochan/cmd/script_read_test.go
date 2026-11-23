package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Сценарий читает из рабочего каталога то, что открыто paths.script_read
// (по умолчанию logs, reports, modules), и ничего в нём не пишет; секреты
// не открываются никаким значением.
func TestScriptReadsOpenedDirs(t *testing.T) {
	e := newScriptEnv(t)
	fm, err := mustFileManager()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{fm.LogsDir(), filepath.Join(fm.BaseDir(), "tokens"), fm.HistoryDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	logFile := filepath.Join(fm.LogsDir(), "octochan.log")
	e.write(logFile, "строка журнала\n")
	token := filepath.Join(fm.BaseDir(), "tokens", "demo")
	e.write(token, "секрет\n")
	history := filepath.Join(fm.HistoryDir(), "x.json")
	e.write(history, "{}\n")
	script := func(src string) string {
		path := e.path("s.ochan")
		e.write(path, src)
		return path
	}

	if out := e.must("run", script(`print(read("`+filepath.ToSlash(logFile)+`"))`)); !strings.Contains(out, "строка журнала") {
		t.Fatalf("журнал не прочитан:\n%s", out)
	}
	e.fails("сценарий его читает, но не пишет", "run", script(`write("`+filepath.ToSlash(logFile)+`", "x")`))
	e.fails("сценариям он закрыт", "run", script(`print(read("`+filepath.ToSlash(token)+`"))`))
	e.fails("сценариям он закрыт", "run", script(`print(read("`+filepath.ToSlash(os.Getenv("OCTOCHAN_CONFIG"))+`"))`))
	e.fails("сценариям он закрыт", "run", script(`print(read("`+filepath.ToSlash(history)+`"))`))

	// history — только явно; tokens не открывается и так.
	setting(t, scriptReadKey, []string{"logs", "history", "tokens"})
	if out := e.must("run", script(`print(read("`+filepath.ToSlash(history)+`"))`)); !strings.Contains(out, "{}") {
		t.Fatalf("history не прочитана:\n%s", out)
	}
	e.fails("сценариям он закрыт", "run", script(`print(read("`+filepath.ToSlash(token)+`"))`))
	if err := checkScriptRead(); err == nil || !strings.Contains(err.Error(), "tokens не открывается сценариям") {
		t.Fatalf("checkScriptRead: %v", err)
	}
}
