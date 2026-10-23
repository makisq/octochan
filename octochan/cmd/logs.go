package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"octochan/i18n"
	"octochan/logs"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	logsTail    int
	logsMin     string
	logsSummary bool
	logsTop     int
)

// journalDir — каталог журнала ochan.
func journalDir() (string, error) {
	if dir := viper.GetString("defaults.log_path"); dir != "" {
		return dir, nil
	}
	fm, err := mustFileManager()
	if err != nil {
		return "", err
	}
	return fm.LogsDir(), nil
}

var logsCmd = &cobra.Command{
	Use:   "logs [--tail N]",
	Short: "Показать журнал работы",
	Long: "Без подкоманды показывает журнал ochan. Журнал, достигший logs.max_size,\n" +
		"уходит в архив с датой в имени (шаблон — logs.prefix), запись идёт в новый\n" +
		"файл; архивов хранится logs.keep. Список файлов журнала — ochan logs files.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := journalDir()
		if err != nil {
			return err
		}
		// Имя файла берётся из той же константы, что и у писателя.
		logFile := filepath.Join(dir, logFileName)

		content, err := os.ReadFile(logFile)
		if err != nil {
			if os.IsNotExist(err) {
				cmd.Println(i18n.T("Журнал ещё не создан — записей нет"))
				return nil
			}
			return i18n.Errorf("не удалось прочитать журнал: %w", err)
		}

		lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
		if logsTail > 0 && logsTail < len(lines) {
			lines = lines[len(lines)-logsTail:]
			cmd.Print(i18n.Sprintf("Последние %d строк журнала (%s):\n", logsTail, logFile))
		}

		for _, line := range lines {
			cmd.Println(line)
		}
		return nil
	},
}

// ranker собирает правила уровней из конфига.
func ranker() (*logs.Ranker, error) {
	var rules []logs.Rule
	if err := viper.UnmarshalKey("logs.rules", &rules); err != nil {
		return nil, i18n.Errorf("logs.rules: %w", err)
	}
	return logs.NewRanker(logs.Settings{
		Levels:  viper.GetStringSlice("logs.levels"),
		Rules:   rules,
		Default: viper.GetString("logs.default_level"),
		Min:     viper.GetString("logs.min_level"),
	})
}

var logsRankCmd = &cobra.Command{
	Use:   "rank <журнал | ->",
	Short: "Ранжировать строки журнала по уровням важности",
	Long: "Уровень строки задают правила logs.rules — регулярные выражения по порядку:\n" +
		"первое совпадение решает. Уровни — logs.levels, от наименее важного, как\n" +
		"log_min_messages у PostgreSQL: --min warning оставляет warning и всё важнее.\n" +
		"Строка без совпадений получает logs.default_level.\n\n" +
		"С --summary — сводка: число строк по уровням и самые частые сообщения.\n" +
		"Группа (?P<msg>…) в правиле выделяет суть сообщения; без неё одинаковыми\n" +
		"считаются строки, которые различаются только числами.",
	Example: "ochan logs rank /var/log/postgresql/postgresql.log --min error\n" +
		"journalctl -u nginx | ochan logs rank - --summary --top 5",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := ranker()
		if err != nil {
			return err
		}
		min := r.Min()
		if logsMin != "" {
			min = logsMin
		}
		minRank, err := r.Level(min)
		if err != nil {
			return err
		}

		in, name, err := openLog(args[0])
		if err != nil {
			return err
		}
		defer in.Close()

		if logsSummary {
			return printSummary(cmd, r, in, name, minRank)
		}
		width := 0
		for _, level := range r.Levels() {
			width = max(width, len(level))
		}
		return r.Scan(in, func(e logs.Entry) error {
			if e.Rank >= minRank {
				cmd.Printf("%-*s  %s\n", width, e.Level, e.Text)
			}
			return nil
		})
	},
}

// openLog открывает журнал или stdin.
func openLog(path string) (io.ReadCloser, string, error) {
	if path == "-" {
		return io.NopCloser(os.Stdin), "stdin", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", i18n.Errorf("не удалось открыть %s: %w", path, err)
	}
	if info, err := f.Stat(); err == nil && !info.Mode().IsRegular() {
		f.Close()
		return nil, "", i18n.Errorf("%s — не обычный файл", path)
	}
	return f, path, nil
}

func printSummary(cmd *cobra.Command, r *logs.Ranker, in io.Reader, name string, minRank int) error {
	s, err := r.Summarize(in, minRank)
	if err != nil {
		return err
	}
	levels := r.Levels()
	cmd.Print(i18n.Sprintf("Журнал: %s\n\nСтрок по уровням:\n", name))
	for i := len(levels) - 1; i >= 0; i-- {
		cmd.Printf("  %-10s %d\n", levels[i], s.Counts[i])
	}

	top := s.Groups
	if logsTop > 0 && len(top) > logsTop {
		top = top[:logsTop]
	}
	if len(top) == 0 {
		cmd.Print(i18n.Sprintf("\nСообщений уровня %s и выше нет\n", levels[minRank]))
		return nil
	}
	cmd.Print(i18n.Sprintf("\nЧастые сообщения уровня %s и выше:\n", levels[minRank]))
	for _, g := range top {
		cmd.Print(i18n.Sprintf("  %-10s %5d  %s  (впервые в строке %d)\n", g.Level, g.Count, g.Message, g.First))
	}
	return nil
}

var logsFilesCmd = &cobra.Command{
	Use:   "files",
	Short: "Показать файлы журнала ochan: текущий и архивы по дате",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := journalDir()
		if err != nil {
			return err
		}
		files, err := logs.Archives(dir, logFileName, viper.GetString("logs.prefix"))
		if err != nil {
			return err
		}
		if len(files) == 0 {
			cmd.Print(i18n.Sprintf("Журнала ещё нет. Каталог: %s\n", dir))
			return nil
		}
		for _, f := range files {
			cmd.Printf("%s  %10d  %s\n", f.Modified.Local().Format("2006-01-02 15:04:05"), f.Size, f.Path)
		}
		return nil
	},
}

func init() {
	logsCmd.Flags().IntVar(&logsTail, "tail", 0, "показать последние N строк (0 — все)")
	logsRankCmd.Flags().StringVar(&logsMin, "min", "", "показывать строки этого уровня и важнее (по умолчанию logs.min_level)")
	logsRankCmd.Flags().BoolVar(&logsSummary, "summary", false, "сводка: строки по уровням и частые сообщения")
	logsRankCmd.Flags().IntVar(&logsTop, "top", 10, "сколько частых сообщений показать в сводке (0 — все)")
	logsCmd.AddCommand(logsRankCmd, logsFilesCmd)
}
