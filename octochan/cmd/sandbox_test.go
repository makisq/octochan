package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"octochan/sandbox"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Сценарий в песочнице выполняется процессом песочницы: вывод, ошибки,
// результат алиаса и действия доходят до родителя.
func TestSandboxedRunInChildProcess(t *testing.T) {
	e := newScriptEnv(t)
	if err := os.MkdirAll(filepath.Join(e.home, ".octochan", "modules", "util"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(e.home, ".octochan", "modules", "util", "util.ochan"),
		"# удвоить\nalias double(x: number) {\n    return x * 2\n}\n")
	e.write(e.path("streams.ochan"), "print(\"в stdout\")\nprint(stderr(), \"в stderr\")\n")
	e.write(e.path("broken.ochan"), "x = 1\nprint(x)\nfail(\"сломано\")\n")
	t.Setenv("OCHAN_TEST_TOKEN", "секрет")
	e.write(e.path("env.ochan"), "print(env(\"OCHAN_TEST_TOKEN\"))\n")

	if out := e.must("run", "--sandbox", "util.double", "21"); strings.TrimSpace(out) != "42" {
		t.Fatalf("алиас пакета в песочнице: %q", out)
	}
	if out := e.must("run", "util.double", "21"); strings.TrimSpace(out) != "42" {
		t.Fatalf("алиас пакета без песочницы: %q", out)
	}

	out := e.must("run", "--sandbox", "streams.ochan")
	if !strings.Contains(out, "в stdout") || !strings.Contains(out, "в stderr") {
		t.Fatalf("потоки сценария не дошли:\n%s", out)
	}
	e.fails("сломано", "run", "--sandbox", "broken.ochan")
	e.fails("переменная окружения OCHAN_TEST_TOKEN недоступна", "run", "--sandbox", "env.ochan")
	if out := e.must("run", "--sandbox", "--allow-env", "OCHAN_TEST_TOKEN", "env.ochan"); !strings.Contains(out, "секрет") {
		t.Fatalf("разрешённая переменная не дошла до процесса песочницы:\n%s", out)
	}

	// План и выполнение идут одним путём: заявка, построенная в песочнице,
	// выполняется и откатывается.
	created := e.must("request", "--sandbox", "tune.ochan")
	id := requestIDPattern.FindStringSubmatch(created)[1]
	e.must("approve", id)
	applied := e.must("apply", "--request", id)
	if got := e.read("prod.conf"); !strings.Contains(got, "max_connections = 200") {
		t.Fatalf("сценарий не выполнен: %q\n%s", got, applied)
	}
	entry := rollbackHint.FindStringSubmatch(applied)
	if entry == nil {
		t.Fatalf("нет отката:\n%s", applied)
	}
	e.must("rollback", entry[1])
	if got := e.read("prod.conf"); !strings.Contains(got, "max_connections = 100") {
		t.Fatalf("откат не вернул файл: %q", got)
	}
}

func TestSandboxStatusCommand(t *testing.T) {
	e := newScriptEnv(t)
	out := e.must("sandbox")
	report := sandbox.Probe(sandbox.Policy{})
	if report.Landlock > 0 && !strings.Contains(out, "Landlock ABI") {
		t.Fatalf("ядро умеет Landlock, а отчёт молчит:\n%s", out)
	}
	if !strings.Contains(out, "Отдельный процесс:") {
		t.Fatalf("отчёт:\n%s", out)
	}
}

func TestSandboxStrictRefusesIncompleteIsolation(t *testing.T) {
	e := newScriptEnv(t)
	saved := probeSandbox
	probeSandbox = func(p sandbox.Policy) sandbox.Report { return sandbox.Report{OS: "linux", Network: p.Network} }
	viper.Set("sandbox.strict", true)
	t.Cleanup(func() {
		probeSandbox = saved
		viper.Set("sandbox.strict", false)
	})

	e.fails("sandbox.strict требует полной", "run", "--sandbox", "tune.ochan")
	e.fails("нет Landlock", "plan", "--sandbox", "tune.ochan")
	if got := e.read("prod.conf"); !strings.Contains(got, "max_connections = 100") {
		t.Fatalf("сценарий выполнился в неполной песочнице: %q", got)
	}
	// Без --sandbox строгость не мешает.
	e.must("plan", "tune.ochan")
}

func TestIsolationWarningOncePerProcess(t *testing.T) {
	warnedIsolation = sync.Once{}
	t.Cleanup(func() { warnedIsolation = sync.Once{} })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&out)
	warnIsolation(cmd, sandbox.Report{OS: "linux", Landlock: 7, Seccomp: true})
	if out.Len() != 0 {
		t.Fatalf("полная изоляция, а есть предупреждение: %q", out.String())
	}
	report := sandbox.Report{OS: "darwin"}
	warnIsolation(cmd, report)
	warnIsolation(cmd, report)
	if got := strings.Count(out.String(), "песочница ОС неполная"); got != 1 {
		t.Fatalf("предупреждений %d, ожидалось одно:\n%s", got, out.String())
	}
}

func TestSandboxChildIgnoresOrdinaryArgs(t *testing.T) {
	// Без переменной окружения аргумент процесса песочницы — обычная
	// неизвестная команда, а не вход в песочницу.
	e := newScriptEnv(t)
	if os.Getenv("OCTOCHAN_SANDBOX_STAGE") != "" {
		t.Skip("переменная песочницы задана снаружи")
	}
	e.fails("unknown command", "__ochan-sandbox", "2", "{}")
}
