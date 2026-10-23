package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/c-bata/go-prompt"
	"github.com/spf13/cobra"
)

func TestResetFlagsRestoresDefaults(t *testing.T) {
	// В оболочке одна и та же команда выполняется много раз. Флаг из
	// прошлой строки не должен действовать на следующую: после
	// `rollback --force` обычный `rollback` обязан снова спрашивать.
	var (
		force bool
		limit int
		name  string
		tags  []string
	)

	root := &cobra.Command{Use: "root"}
	root.PersistentFlags().StringVar(&name, "config", "по-умолчанию", "")
	sub := &cobra.Command{Use: "sub", RunE: func(*cobra.Command, []string) error { return nil }}
	sub.Flags().BoolVar(&force, "force", false, "")
	sub.Flags().IntVar(&limit, "limit", 20, "")
	sub.Flags().StringSliceVar(&tags, "tag", []string{"a"}, "")
	root.AddCommand(sub)

	root.SetArgs([]string{"sub", "--force", "--limit=5", "--config=x.yaml", "--tag=b", "--tag=c"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !force || limit != 5 || name != "x.yaml" || len(tags) != 2 {
		t.Fatalf("флаги не разобрались: force=%v limit=%d config=%q tag=%v", force, limit, name, tags)
	}

	resetFlags(root)

	if force || limit != 20 || name != "по-умолчанию" {
		t.Errorf("флаги не сброшены: force=%v limit=%d config=%q", force, limit, name)
	}
	if len(tags) != 1 || tags[0] != "a" {
		t.Errorf("флаг-список не сброшен к значению по умолчанию: %v", tags)
	}
	if sub.Flags().Lookup("force").Changed {
		t.Error("флаг остался помеченным как заданный")
	}
}

func TestResetFlagsClearsHelp(t *testing.T) {
	// Найдено прогоном оболочки: после `validate --help` следующий
	// `validate файл` снова печатал справку вместо проверки.
	ran := false
	root := &cobra.Command{Use: "root"}
	sub := &cobra.Command{Use: "sub", RunE: func(*cobra.Command, []string) error { ran = true; return nil }}
	root.AddCommand(sub)
	root.SetOut(&discard{})

	root.SetArgs([]string{"sub", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("--help выполнил команду вместо справки")
	}

	resetFlags(root)
	root.SetArgs([]string{"sub"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("после --help команда так и не выполнилась: флаг прилип")
	}
}

func TestSetContextReplacesStaleContext(t *testing.T) {
	// cobra запоминает контекст подкоманды при первом запуске. Без замены
	// отменённый однажды контекст доставался бы всем следующим командам.
	var seen context.Context
	root := &cobra.Command{Use: "root"}
	sub := &cobra.Command{Use: "sub", RunE: func(c *cobra.Command, _ []string) error {
		seen = c.Context()
		return nil
	}}
	root.AddCommand(sub)

	first, cancel := context.WithCancel(context.Background())
	root.SetArgs([]string{"sub"})
	if err := root.ExecuteContext(first); err != nil {
		t.Fatal(err)
	}
	cancel()

	second := context.Background()
	setContext(root, second)
	root.SetArgs([]string{"sub"})
	if err := root.ExecuteContext(second); err != nil {
		t.Fatal(err)
	}
	if seen.Err() != nil {
		t.Error("команда получила отменённый контекст прошлого запуска")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// fakeParser — ввод go-prompt, который только считает вызовы.
type fakeParser struct {
	tearDowns int
	err       error
}

func (f *fakeParser) Setup() error                { return nil }
func (f *fakeParser) TearDown() error             { f.tearDowns++; return f.err }
func (f *fakeParser) GetWinSize() *prompt.WinSize { return &prompt.WinSize{Row: 24, Col: 80} }
func (f *fakeParser) Read() ([]byte, error)       { return nil, nil }

func TestRestoringParserDelegatesTearDown(t *testing.T) {
	inner := &fakeParser{err: errors.New("ошибка go-prompt")}
	parser := &restoringParser{ConsoleParser: inner}

	// terminal == nil: восстанавливать нечего, но и падать нельзя.
	if err := parser.TearDown(); err == nil || inner.tearDowns != 1 {
		t.Errorf("TearDown не передан внутреннему парсеру или потерял ошибку: %v, вызовов %d",
			err, inner.tearDowns)
	}
}

func TestDefaultSlice(t *testing.T) {
	cases := map[string]int{"[]": 0, "[a]": 1, "[a,b,c]": 3}
	for value, want := range cases {
		if got := defaultSlice(value); len(got) != want {
			t.Errorf("defaultSlice(%q) = %v, ожидалось элементов %d", value, got, want)
		}
	}
}
