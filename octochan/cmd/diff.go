package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"ochan/config"
	"ochan/workspace"

	"github.com/spf13/cobra"
)

var (
	fastMode        bool
	noSnapshot      bool
	includeDefaults bool
	diffByLines     bool
	diffByMeaning   bool
	diffContext     int
)

var diffCmd = &cobra.Command{
	Use:   "diff <источник1> <источник2>",
	Short: "Сравнить две конфигурации или два файла",
	Long: "Сравнение у ochan одно. Конфиги и живые источники — по смыслу: 8GB и\n" +
		"8192MB равны, порядок параметров неважен. Всё остальное — журналы,\n" +
		"скрипты, любой текст — построчно, в формате diff -u: разбор конфига\n" +
		"пропустил бы строки журнала, и два разных журнала вышли бы\n" +
		"одинаковыми. Конфигом считается текст, где почти каждая строка —\n" +
		"[секция] или «имя = значение»; --lines и --config выбирают явно.\n\n" +
		"Источником может быть файл, \"-\" для stdin или живой сервер,\n" +
		"например postgres://user@host:5432/postgres.\n\n" +
		"Для живого PostgreSQL читается pg_settings, а не postgresql.conf:\n" +
		"поверх файла действуют ALTER SYSTEM, include и параметры командной\n" +
		"строки, поэтому файл не отражает того, как сервер работает на самом деле.",
	Example: "ochan diff prod.conf test.conf\n" +
		"ochan diff эталон.conf postgres://dba@db1:5432/postgres\n" +
		"cat prod.conf | ochan diff - test.conf\n" +
		"ochan diff app.log app.log.1              # не конфиг — построчно\n" +
		"ochan diff --lines -U 0 nginx.conf nginx.conf.bak",
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}

		opts := config.SourceOptions{IncludeDefaults: includeDefaults}
		ctx := cmd.Context()

		rightRef, err := argOrStdin(args, 1)
		if err != nil {
			return fmt.Errorf("для сравнения нужен второй источник: %w", err)
		}
		if args[0] == "-" && rightRef == "-" {
			return fmt.Errorf("нельзя использовать stdin для обеих сторон")
		}
		if done, err := diffLinesIfText(cmd, args[0], rightRef); done || err != nil {
			return err
		}

		left, err := loadConfigArg(ctx, args[0], opts)
		if err != nil {
			return err
		}
		right, err := loadConfigArg(ctx, rightRef, opts)
		if err != nil {
			return err
		}

		// Замечания источника важнее самих расхождений: сервер может
		// выглядеть настроенным, но работать по-старому, пока не перезапущен.
		for _, note := range append(append([]string{}, left.Notes...), right.Notes...) {
			cmd.Printf("внимание: %s\n", note)
		}

		diff := config.CompareConfigs(left.Data, right.Data, left.Name, right.Name)
		if diff.Len() == 0 {
			cmd.Println("Источники идентичны — различий не найдено.")
			return nil
		}

		cmd.Printf("Найдены различия (%d):\n", diff.Len())
		for _, entry := range diff.Entries {
			leftVal, rightVal := entry.Left, entry.Right
			if !entry.LeftExists {
				leftVal = "—"
			}
			if !entry.RightExists {
				rightVal = "—"
			}
			cmd.Printf("  %s: %s -> %s [%s]\n", entry.Param(), leftVal, rightVal, entry.Status)
		}

		// Отчёты и снапшоты пишутся в ~/.octochan, а не рядом с бинарником:
		// после установки в /usr/local/bin туда просто нет прав на запись.
		if fastMode {
			return nil
		}

		reportNum, err := config.GetNextReportNumber(fm.ReportsDir())
		if err != nil {
			return err
		}
		reportPath := filepath.Join(fm.ReportsDir(), fmt.Sprintf("diff_report_%d.txt", reportNum))
		if err := config.SaveDiffToFile(diff, reportPath); err != nil {
			return err
		}
		cmd.Printf("\nОтчёт сохранён: %s\n", reportPath)

		if noSnapshot {
			return nil
		}

		// Сравнение ничего не меняет, поэтому запись необратима по природе:
		// откатывать нечего. Но знать, что и когда сравнивали, нужно.
		entry := &workspace.Entry{
			Kind:    workspace.EntryDiff,
			Command: "diff",
			Source:  left.Name,
			Target:  right.Name,
			Summary: fmt.Sprintf("расхождений: %d", diff.Len()),
			Changes: config.PlanFromDiff(diff, right.Name).Changes,
		}
		record(cmd, entry)
		cmd.Printf("Записано в историю: %s\n", entry.ID)
		return nil
	},
}

// diffLinesIfText сравнивает построчно, если это не конфиги или так
// попросили, и сообщает, что сравнение сделано.
func diffLinesIfText(cmd *cobra.Command, leftRef, rightRef string) (bool, error) {
	switch {
	case diffByLines && diffByMeaning:
		return true, errors.New("--lines и --config вместе нельзя: сравнение или построчно, или по смыслу")
	case diffContext < 0:
		return true, errors.New("-U — число строк, не меньше 0")
	case diffByMeaning || (!diffByLines && (strings.Contains(leftRef, "://") || strings.Contains(rightRef, "://"))):
		return false, nil
	}
	left, err := readSource(leftRef)
	if err != nil {
		return true, err
	}
	right, err := readSource(rightRef)
	if err != nil {
		return true, err
	}
	if !diffByLines {
		var text []string
		for _, s := range []source{left, right} {
			if !configLike(s.Content) {
				text = append(text, s.Name)
			}
		}
		if len(text) == 0 {
			return false, nil
		}
		cmd.PrintErrf("%s — не конфиг: сравниваю построчно (по смыслу — --config)\n", strings.Join(text, " и "))
	}
	edits, err := diffLines(splitLines(left.Content), splitLines(right.Content))
	if err != nil {
		return true, err
	}
	if out := unified(left.Name, right.Name, edits, diffContext); out != "" {
		cmd.Print(out)
	} else {
		cmd.Println("Источники идентичны — различий не найдено.")
	}
	return true, nil
}

func init() {
	diffCmd.Flags().BoolVarP(&diffByLines, "lines", "l", false, "сравнить построчно, как diff -u, даже конфиги")
	diffCmd.Flags().BoolVar(&diffByMeaning, "config", false, "сравнить по смыслу, как конфиги, даже если текст на них не похож")
	diffCmd.Flags().IntVarP(&diffContext, "unified", "U", 3, "строк контекста вокруг изменения при построчном сравнении")
	diffCmd.Flags().BoolVarP(&fastMode, "fast", "f", false,
		"только вывод в консоль, без отчёта и снапшота")
	diffCmd.Flags().BoolVar(&noSnapshot, "no-snapshot", false,
		"не создавать снапшот")
	diffCmd.Flags().BoolVar(&includeDefaults, "all", false,
		"для живых источников включать параметры со значением по умолчанию")
}
