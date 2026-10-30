package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"octochan/i18n"
	"octochan/logs"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// У каждого текста справки есть английский перевод, и перевод строки
// вызова не трогает имя команды.
func TestEnglishCoversHelp(t *testing.T) {
	for _, text := range helpTexts(rootCmd) {
		translated, ok := i18n.Lookup("en", text)
		if !ok {
			t.Errorf("нет английского перевода справки:\n%s", text)
			continue
		}
		if hasCyrillic(translated) {
			t.Errorf("в английском переводе кириллица:\n%s", translated)
		}
	}

	walkCommands(rootCmd, func(c *cobra.Command) {
		use := commandSource(c, sourceUse, c.Use)
		if translated, ok := i18n.Lookup("en", use); ok && firstWord(translated) != firstWord(use) {
			t.Errorf("перевод %q меняет имя команды: %q", use, translated)
		}
	}, func(*pflag.Flag) {})
}

// Каждое сообщение, которое код переводит через i18n, есть в английском
// словаре: иначе lang: eng молча оставил бы его по-русски.
func TestEnglishCoversMessages(t *testing.T) {
	// Весь модуль octochan, а не только cmd: сообщения переводят и пакеты.
	var files []string
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "mcp" || d.Name() == ".git") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "i18n" ||
				(sel.Sel.Name != "T" && sel.Sel.Name != "Sprintf" && sel.Sel.Name != "Errorf") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, _ := strconv.Unquote(lit.Value)
			if _, ok := i18n.Lookup("en", text); !ok {
				t.Errorf("%s: нет английского перевода сообщения %q", path, text)
			}
			return true
		})
	}
}

// useLanguage включает язык на время теста и возвращает русский: дерево
// команд общее для всех тестов пакета.
func useLanguage(t *testing.T, code string) {
	t.Helper()
	viper.Set("lang", code)
	applyLanguage()
	t.Cleanup(func() {
		viper.Set("lang", i18n.Source)
		if err := i18n.Use(i18n.Source); err != nil {
			t.Fatal(err)
		}
		localizeCommands(rootCmd)
	})
}

func TestLangSwitchesHelp(t *testing.T) {
	e := newScriptEnv(t)
	useLanguage(t, "eng")

	help := e.must("plan", "--help")
	for _, want := range []string{"With one argument, plans a scenario",
		"alias-language script (implied for a .ochan file)", "plan <scenario> | plan <base> <changes>"} {
		if !strings.Contains(help, want) {
			t.Fatalf("в справке нет %q:\n%s", want, help)
		}
	}
	if hasCyrillic(help) {
		t.Fatalf("в английской справке кириллица:\n%s", help)
	}
	if out := e.must("lang"); !strings.Contains(out, "* en   English") {
		t.Fatalf("текущий язык не отмечен:\n%s", out)
	}
	// Имя команды не переведено: она по-прежнему вызывается.
	if out := e.must("help"); !strings.Contains(out, "Available commands:") ||
		!strings.Contains(out, "Show what would happen, without doing anything") {
		t.Fatalf("help:\n%s", out)
	}

	// Отказ в правах — тоже по-английски.
	t.Setenv(EnvRole, "llm")
	e.fails("role llm may not run apply: permission apply is required", "apply", "--request", "x")
}

func TestLangTemplate(t *testing.T) {
	e := newScriptEnv(t)
	useLanguage(t, "eng")
	// Шаблон — с русскими ключами, даже когда справка уже по-английски.
	out := e.must("lang", "template", "de")
	if !strings.Contains(out, `"Согласовать заявку": ""`) || !strings.Contains(out, `"Доступные команды:": ""`) {
		t.Fatalf("шаблон:\n%.400s", out)
	}
	if out := e.must("lang", "template", "en"); !strings.Contains(out, `"Согласовать заявку": "Approve a request"`) {
		t.Fatalf("шаблон en:\n%.400s", out)
	}
}

func TestShellHistoryPersists(t *testing.T) {
	newScriptEnv(t)
	commandHistory = nil
	t.Cleanup(func() { commandHistory = nil })

	rememberCommand("diff a.conf postgres://dba:секрет@db1/postgres", "diff a.conf postgres://dba:секрет@db1/postgres")
	rememberCommand(" role create x", "role create x") // с пробела — не запоминается
	rememberCommand("plan x.ochan", "plan x.ochan")
	rememberCommand("plan x.ochan", "plan x.ochan") // повтор — один раз
	rememberCommand("!!history", "!!history")

	commandHistory = nil
	loadShellHistory()
	want := []string{"diff a.conf postgres://dba:***@db1/postgres", "plan x.ochan"}
	if strings.Join(commandHistory, "|") != strings.Join(want, "|") {
		t.Fatalf("история после перезапуска: %q", commandHistory)
	}

	fm, _ := mustFileManager()
	info, err := os.Stat(fm.StatePath())
	if err != nil {
		t.Fatalf("файл состояния: %v", err)
	}
	// Права 0600 есть только в Unix: на Windows доступ задаётся списками
	// ACL, а Mode().Perm() обычного файла там всегда 0666.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("файл состояния доступен не только владельцу: %v", info.Mode().Perm())
	}
	viper.Set("shell.history_size", 1)
	t.Cleanup(func() { viper.Set("shell.history_size", 1000) })
	rememberCommand("stats a.conf", "stats a.conf")
	state, err := workspace.LoadState(fm.StatePath())
	if err != nil || len(state.History) != 1 || state.History[0] != "stats a.conf" {
		t.Fatalf("предел истории: %+v %v", state, err)
	}
}

func TestRedactSecrets(t *testing.T) {
	cases := map[string]string{
		"diff postgres://u:p@h/db x":            "diff postgres://u:***@h/db x",
		`diff "host=db password=s3cr3t" a.conf`: `diff "host=db password=***" a.conf`,
		"diff postgres://u@h/db x":              "diff postgres://u@h/db x",
	}
	for in, want := range cases {
		if got := redactSecrets(in); got != want {
			t.Errorf("%q → %q, ожидалось %q", in, got, want)
		}
	}
}

func TestLogsRank(t *testing.T) {
	e := newScriptEnv(t)
	e.write(e.path("pg.log"), "10:00 LOG:  ready\n10:01 ERROR:  relation \"a\" does not exist\n"+
		"10:02 ERROR:  relation \"b\" does not exist\n10:03 FATAL:  password authentication failed\n")

	out := e.must("logs", "rank", "pg.log", "--min", "error")
	if strings.Contains(out, "ready") || !strings.Contains(out, "fatal    10:03 FATAL:") || strings.Count(out, "error    ") != 2 {
		t.Fatalf("rank:\n%s", out)
	}

	viper.Set("logs.rules", []map[string]any{{"level": "fatal", "match": "authentication failed"},
		{"level": "error", "match": `ERROR:\s+(?P<msg>relation)`}})
	t.Cleanup(func() { viper.Set("logs.rules", logs.DefaultRules) })
	out = e.must("logs", "rank", "pg.log", "--summary", "--min", "error")
	for _, want := range []string{"fatal      1", "error      2", "error          2  relation  (впервые в строке 2)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в сводке нет %q:\n%s", want, out)
		}
	}

	e.fails(`уровня "срочно" нет`, "logs", "rank", "pg.log", "--min", "срочно")
	viper.Set("logs.rules", []map[string]any{{"level": "error", "match": "("}})
	e.fails("logs.rules[1]", "logs", "rank", "pg.log")
}

func TestLogsFiles(t *testing.T) {
	e := newScriptEnv(t)
	dir, err := journalDir()
	if err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(dir, logFileName), "x\n")
	e.write(filepath.Join(dir, "2026-10-01_08-00-00_"+logFileName), "old\n")
	e.write(filepath.Join(dir, "чужой_"+logFileName), "не архив\n")
	out := e.must("logs", "files")
	if !strings.Contains(out, "2026-10-01_08-00-00_"+logFileName) || strings.Contains(out, "чужой") {
		t.Fatalf("files:\n%s", out)
	}
}
