package cmd

import (
	"fmt"
	"path/filepath"

	"octochan/config"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

var (
	fastMode        bool
	noSnapshot      bool
	includeDefaults bool
)

var diffCmd = &cobra.Command{
	Use:   "diff <источник1> <источник2>",
	Short: "Сравнить две конфигурации",
	Long: "Источником может быть файл, \"-\" для stdin или живой сервер,\n" +
		"например postgres://user@host:5432/postgres.\n\n" +
		"Для живого PostgreSQL читается pg_settings, а не postgresql.conf:\n" +
		"поверх файла действуют ALTER SYSTEM, include и параметры командной\n" +
		"строки, поэтому файл не отражает того, как сервер работает на самом деле.",
	Example: "ochan diff prod.conf test.conf\n" +
		"ochan diff эталон.conf postgres://dba@db1:5432/postgres\n" +
		"cat prod.conf | ochan diff - test.conf",
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}

		opts := config.SourceOptions{IncludeDefaults: includeDefaults}
		ctx := cmd.Context()

		left, err := loadConfigArg(ctx, args[0], opts)
		if err != nil {
			return err
		}

		rightRef, err := argOrStdin(args, 1)
		if err != nil {
			return fmt.Errorf("для сравнения нужен второй источник: %w", err)
		}
		if left.Name == "stdin" && rightRef == "-" {
			return fmt.Errorf("нельзя использовать stdin для обеих сторон")
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

func init() {
	diffCmd.Flags().BoolVarP(&fastMode, "fast", "f", false,
		"только вывод в консоль, без отчёта и снапшота")
	diffCmd.Flags().BoolVar(&noSnapshot, "no-snapshot", false,
		"не создавать снапшот")
	diffCmd.Flags().BoolVar(&includeDefaults, "all", false,
		"для живых источников включать параметры со значением по умолчанию")
}
