package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var logsTail int

var logsCmd = &cobra.Command{
	Use:   "logs [--tail N]",
	Short: "Показать журнал работы",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		logPath := viper.GetString("defaults.log_path")
		if logPath == "" {
			fm, err := mustFileManager()
			if err != nil {
				return err
			}
			logPath = fm.LogsDir()
		}

		// Имя файла берётся из той же константы, что и у писателя.
		logFile := filepath.Join(logPath, logFileName)

		content, err := os.ReadFile(logFile)
		if err != nil {
			if os.IsNotExist(err) {
				cmd.Println("Журнал ещё не создан — записей нет")
				return nil
			}
			return fmt.Errorf("не удалось прочитать журнал: %w", err)
		}

		lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
		if logsTail > 0 && logsTail < len(lines) {
			lines = lines[len(lines)-logsTail:]
			cmd.Printf("Последние %d строк журнала (%s):\n", logsTail, logFile)
		}

		for _, line := range lines {
			cmd.Println(line)
		}
		return nil
	},
}

func init() {
	logsCmd.Flags().IntVar(&logsTail, "tail", 0, "показать последние N строк (0 — все)")
}
