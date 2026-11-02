package lang

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"ochan/config"
)

// fakeCommands — команды ochan для теста: logs читает, incident меняет.
type fakeCommands struct{ ran []string }

func (f *fakeCommands) Resolve(args []string) ([]string, string, bool, error) {
	argv := args
	if len(args) == 1 {
		argv = strings.Fields(args[0])
	}
	switch argv[0] {
	case "logs":
		return argv, "logs.rank", true, nil
	case "incident":
		return argv, "incident.open", false, nil
	}
	return nil, "", false, errors.New("ochan " + argv[0] + ": такой команды нет")
}

func (f *fakeCommands) Run(_ context.Context, argv []string) (string, error) {
	f.ran = append(f.ran, strings.Join(argv, " "))
	return "вывод " + argv[0], nil
}

// ochan(): читающая команда выполняется и в плане, меняющая в плане —
// действие и пустая строка, при выполнении — действие и вывод; без команд
// и в песочнице — отказ.
func TestOchanBuiltin(t *testing.T) {
	run := func(src string, cmds Commands, plan bool) (*Interpreter, error) {
		file, err := Parse("t.ochan", src)
		if err != nil {
			t.Fatal(err)
		}
		in := NewInterpreter(context.Background(), NewRegistry(), io.Discard)
		if cmds != nil {
			in.SetCommands(cmds)
		}
		if plan {
			in.SetPlanMode()
		}
		return in, in.RunFile(file)
	}
	if _, err := run(`ochan("logs rank")`, nil, false); err == nil || !strings.Contains(err.Error(), "команды ochan здесь недоступны") {
		t.Fatalf("без команд: %v", err)
	}

	cmds := &fakeCommands{}
	src := `log = ochan("logs rank --min warning")
if log != "вывод logs" { fail("читающая не выполнилась: " + log) }
x = ochan("incident", "open", "--summary", "соединения")`
	in, err := run(src+"\nif x != \"\" { fail(\"в плане меняющая выполнилась\") }", cmds, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds.ran) != 1 || cmds.ran[0] != "logs rank --min warning" {
		t.Fatalf("в плане выполнено: %v", cmds.ran)
	}
	effects := in.Effects()
	if len(effects) != 1 || effects[0].Kind != EffectCommand || effects[0].Path != "incident.open" || effects[0].Writes() {
		t.Fatalf("действия плана: %+v", effects)
	}
	plan := BuildPlan("t.ochan", effects)
	if plan.Len() != 1 || plan.Changes[0].Kind != config.ChangeCommand ||
		plan.Changes[0].Name != "ochan incident open --summary соединения" {
		t.Fatalf("план: %+v", plan.Changes)
	}
	other := effects[0]
	other.Args = []string{"incident", "open", "--summary", "диск"}
	if other.Fingerprint() == effects[0].Fingerprint() {
		t.Fatal("отпечаток не зависит от аргументов")
	}

	cmds = &fakeCommands{}
	in, err = run(src+"\nif x != \"вывод incident\" { fail(\"меняющая не выполнилась\") }", cmds, false)
	if err != nil || len(cmds.ran) != 2 || len(in.Effects()) != 1 {
		t.Fatalf("выполнение: %v, выполнено %v, действий %d", err, cmds.ran, len(in.Effects()))
	}
	if _, err := run(`ochan("rm -rf /")`, cmds, false); err == nil || !strings.Contains(err.Error(), "такой команды нет") {
		t.Fatalf("неизвестная команда: %v", err)
	}

	box, err := NewSandbox([]string{t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, _ := Parse("t.ochan", `ochan("logs rank")`)
	in = NewInterpreter(context.Background(), NewRegistry(), io.Discard)
	in.SetCommands(&fakeCommands{})
	in.SetSandbox(box)
	if err := in.RunFile(file); err == nil || !strings.Contains(err.Error(), "в песочнице сценарий их не видит") {
		t.Fatalf("песочница: %v", err)
	}
}

// Сеанс оболочки: переменные одной строки видны следующим, значение
// одиночного выражения возвращается.
func TestSessionRunLine(t *testing.T) {
	s := NewSession()
	line := func(src string) Value {
		t.Helper()
		file, err := Parse("<shell>", src)
		if err != nil {
			t.Fatal(err)
		}
		v, err := NewInterpreter(context.Background(), NewRegistry(), io.Discard).RunLine(s, file)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if _, null := line("n = 20").(NullValue); !null || !s.Defined("n") || s.Defined("m") {
		t.Fatal("присваивание в сеансе")
	}
	line("for i in [1, 2] { n = n + i }")
	if v := line("n * 2"); Display(v) != "46" {
		t.Fatalf("n * 2 = %s", Display(v))
	}
}
