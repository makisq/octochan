package cmd

import (
	"fmt"
	"os"
	"sort"

	"octochan/config"

	"github.com/spf13/cobra"
)

// Команды find/stats/validate/print/tee раньше не объявляли Args и молча
// игнорировали ошибки чтения, поэтому `ochan stats` без аргументов падал
// с index out of range, а `ochan validate несуществующий.conf` рапортовал
// «конфиг валиден».

var findCmd = &cobra.Command{
	Use:     "find <file> <parameter>",
	Short:   "Найти параметр в конфиге",
	Args:    cobra.RangeArgs(1, 2),
	Example: "ochan find postgresql.conf shared_buffers",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, param := args[0], ""
		switch len(args) {
		case 1:
			// В конвейере файл заменяется на stdin: `cat a.conf | ochan find shared_buffers`
			if !stdinIsPipe() {
				return fmt.Errorf("укажите файл и имя параметра")
			}
			path, param = "-", args[0]
		default:
			param = args[1]
		}

		src, err := loadConfigArg(cmd.Context(), path, config.SourceOptions{})
		if err != nil {
			return err
		}
		cfg := src.Data

		found := 0
		for _, section := range sortedSections(cfg) {
			if value, ok := cfg[section][param]; ok {
				if section == "" {
					cmd.Printf("%s = %s\n", param, value)
				} else {
					cmd.Printf("[%s] %s = %s\n", section, param, value)
				}
				found++
			}
		}

		if found == 0 {
			return fmt.Errorf("параметр %q не найден в %s", param, src.Name)
		}
		return nil
	},
}

var statsCmd = &cobra.Command{
	Use:   "stats <file>",
	Short: "Показать статистику конфига",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := argOrStdin(args, 0)
		if err != nil {
			return err
		}
		src, err := loadConfigArg(cmd.Context(), path, config.SourceOptions{})
		if err != nil {
			return err
		}
		cfg := src.Data

		total := 0
		for _, params := range cfg {
			total += len(params)
		}

		for _, note := range src.Notes {
			cmd.Printf("внимание: %s\n", note)
		}
		cmd.Printf("Источник: %s\n", src.Name)
		cmd.Printf("Секций: %d\n", len(cfg))
		cmd.Printf("Параметров: %d\n", total)
		for _, section := range sortedSections(cfg) {
			name := section
			if name == "" {
				name = "(без секции)"
			}
			cmd.Printf("  %-30s %d\n", name, len(cfg[section]))
		}
		return nil
	},
}

var validateCmd = &cobra.Command{
	Use:   "validate <file>",
	Short: "Проверить конфиг на ошибки",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := argOrStdin(args, 0)
		if err != nil {
			return err
		}
		src, err := loadConfigArg(cmd.Context(), path, config.SourceOptions{})
		if err != nil {
			return err
		}
		cfg := src.Data

		total := 0
		for _, params := range cfg {
			total += len(params)
		}
		if total == 0 {
			return fmt.Errorf("%s: не найдено ни одного параметра", src.Name)
		}

		cmd.Printf("Конфиг валиден: %s (%d параметров)\n", src.Name, total)
		return nil
	},
}

var printCmd = &cobra.Command{
	Use:   "print [file]",
	Short: "Вывести содержимое файла или stdin",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := argOrStdin(args, 0)
		if err != nil {
			return fmt.Errorf("укажите файл или используйте конвейер: echo 'hello' | ochan print")
		}
		src, err := readSource(path)
		if err != nil {
			return err
		}
		cmd.Print(src.Content)
		return nil
	},
}

var teeCmd = &cobra.Command{
	Use:   "tee [file]",
	Short: "Прочитать stdin и продублировать в файл",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		data, err := readStdin()
		if err != nil {
			return err
		}
		cmd.Print(string(data))

		if len(args) == 0 {
			return nil
		}
		if err := os.WriteFile(args[0], data, 0o644); err != nil {
			return fmt.Errorf("не удалось записать %s: %w", args[0], err)
		}
		return nil
	},
}

func sortedSections(cfg config.ConfigMap) []string {
	sections := make([]string, 0, len(cfg))
	for section := range cfg {
		sections = append(sections, section)
	}
	sort.Strings(sections)
	return sections
}
