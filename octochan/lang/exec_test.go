package lang

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octochan/config"
)

func TestLaunchersAreRefused(t *testing.T) {
	for _, name := range []string{"sh", "bash", "python3", "python3.11", "/usr/bin/perl5.36", "busybox", "gawk",
		"ld-linux-x86-64.so.2", "node"} {
		if !isLauncher(name) {
			t.Errorf("%s не считается оболочкой или интерпретатором", name)
		}
	}
	for _, name := range []string{"kubectl", "terraform", "pythonista", "shellcheck", "true", "nice"} {
		if isLauncher(name) {
			t.Errorf("%s считается оболочкой или интерпретатором", name)
		}
	}
}

func TestResolveExecutable(t *testing.T) {
	if _, err := os.Stat("/usr/bin/true"); err != nil {
		t.Skip("нет /usr/bin/true")
	}
	root := t.TempDir()
	p, err := ResolveExecutable("true", []string{root})
	if err != nil || p.Name != "true" || !filepath.IsAbs(p.Path) || len(p.Sum) != 64 {
		t.Fatalf("true: %+v, %v", p, err)
	}
	for name, want := range map[string]string{
		"sh":                "оболочки, интерпретаторы",
		"no-such-program-x": "не найдена",
		"":                  "пустое имя",
	} {
		if _, err := ResolveExecutable(name, []string{root}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, ожидалось %q", name, err, want)
		}
	}
	// Программа в каталоге песочницы — сценарий может её переписать.
	data, _ := os.ReadFile("/usr/bin/true")
	inside := filepath.Join(root, "tool")
	if err := os.WriteFile(inside, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveExecutable(inside, []string{root}); err == nil || !strings.Contains(err.Error(), "в каталоге песочницы") {
		t.Fatalf("программа внутри песочницы: %v", err)
	}
}

// exec() — только в песочнице и только разрешённое; в плане программа не
// запускается, но план о ней говорит.
func TestExecBuiltinPlan(t *testing.T) {
	run := func(src string, box *Sandbox, plan bool) (*Interpreter, error) {
		file, err := Parse("t.ochan", src)
		if err != nil {
			t.Fatal(err)
		}
		in := NewInterpreter(context.Background(), NewRegistry(), io.Discard)
		in.SetSandbox(box)
		if plan {
			in.SetPlanMode()
		}
		return in, in.RunFile(file)
	}
	if _, err := run(`exec("kubectl", "get", "pods")`, nil, true); err == nil || !strings.Contains(err.Error(), "только в песочнице ядра") {
		t.Fatalf("без песочницы: %v", err)
	}
	box, err := NewSandbox([]string{t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(`exec("kubectl")`, box, true); err == nil || !strings.Contains(err.Error(), "только в песочнице ядра") {
		t.Fatalf("песочница без --allow-exec: %v", err)
	}
	box.SetExec([]Executable{{Name: "kubectl", Path: "/nonexistent/kubectl"}})
	if _, err := run(`exec("terraform")`, box, true); err == nil || !strings.Contains(err.Error(), "разрешены (--allow-exec): kubectl") {
		t.Fatalf("не разрешённая: %v", err)
	}
	in, err := run(`x = exec("kubectl", "apply", "-f", "web.yaml")
if x != "" { fail("в плане программа запустилась") }`, box, true)
	if err != nil {
		t.Fatal(err)
	}
	effects := in.Effects()
	if len(effects) != 1 || effects[0].Kind != EffectExec || effects[0].Path != "kubectl" || effects[0].Writes() {
		t.Fatalf("действия: %+v", effects)
	}
	plan := BuildPlan("t.ochan", effects)
	if plan.Len() != 1 || plan.Changes[0].Kind != config.ChangeExec || plan.Changes[0].Name != "kubectl apply -f web.yaml" {
		t.Fatalf("план: %+v", plan.Changes)
	}
	other := effects[0]
	other.Args = []string{"delete", "-f", "web.yaml"}
	if other.Fingerprint() == effects[0].Fingerprint() {
		t.Fatal("отпечаток не зависит от аргументов")
	}
}
