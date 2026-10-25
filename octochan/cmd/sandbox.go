package cmd

import (
	"os"

	"octochan/i18n"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var sandboxCmd = &cobra.Command{
	Use:   "sandbox",
	Short: "Показать, какую изоляцию даёт песочница на этой системе",
	Long: "Сценарий с --sandbox выполняется отдельным процессом. На Linux его\n" +
		"ограничивает ядро: Landlock пускает только в разрешённые каталоги,\n" +
		"seccomp запрещает запуск программ, отладку чужих процессов, монтирование\n" +
		"и сокеты, своё сетевое пространство оставляет без сети. На других ОС\n" +
		"файлы и сеть держит только песочница языка.\n\n" +
		"С sandbox.strict: true в config.yaml сценарий не выполняется, если\n" +
		"изоляция неполная.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// Пробный запуск, а не только опрос ядра: пространства имён
		// выясняются при запуске, и так видно, что песочница ставится.
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		result, err := isolated(cmd, isolatedSpec{Mode: "probe", Roots: []string{cwd}}, cmd.OutOrStdout())
		if err != nil {
			return err
		}
		report := result.report
		for _, line := range report.Lines() {
			cmd.Println(line)
		}
		if gaps := report.Gaps(); len(gaps) > 0 {
			cmd.Println()
			cmd.Println(i18n.T("Изоляция неполная:"))
			for _, gap := range gaps {
				cmd.Println("  " + gap)
			}
		}
		if viper.GetBool("sandbox.strict") {
			cmd.Println(i18n.T("\nsandbox.strict: true — при неполной изоляции сценарий не выполняется"))
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(sandboxCmd)
}
