package cmd

import (
	"fmt"

	"octochan/lang"

	"github.com/spf13/cobra"
)

var checkName string

var checkCmd = &cobra.Command{
	Use:   "check [файл.ochan | -]...",
	Short: "Проверить сценарий языка алиасов, ничего не выполняя",
	Long: "Обходит все ветки и все алиасы и находит то, что видно без запуска:\n" +
		"неизвестные имена и функции, неверное число аргументов, break и return\n" +
		"не на своём месте, явно неверные типы. Файлы не читаются, источники\n" +
		"не опрашиваются: проверяется только текст.\n\n" +
		"Без аргументов проверяет библиотеку — все установленные алиасы.",
	Example: "ochan check сценарий.ochan\n" +
		"cat сценарий.ochan | ochan check -\n" +
		"ochan check",
	RunE: func(cmd *cobra.Command, args []string) error {
		registry, loadErrs := aliases()

		var problems []string

		if len(args) == 0 {
			// Сломанный файл библиотеки при проверке — это находка, а не
			// предупреждение: ради неё команду и запускают.
			for _, err := range loadErrs {
				problems = append(problems, err.Error())
			}
			for _, problem := range lang.CheckRegistry(registry) {
				problems = append(problems, problem.String())
			}
			return reportCheck(cmd, "библиотека алиасов", problems)
		}

		reportAliasErrors(cmd, loadErrs)

		for _, path := range args {
			src, err := readSource(path)
			if err != nil {
				return err
			}
			if path == "-" && checkName != "" {
				src.Name = checkName
			}

			// Синтаксическая ошибка останавливает разбор файла, но не
			// проверку остальных файлов.
			file, err := lang.Parse(src.Name, src.Content)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}

			// Каждый файл — отдельный сценарий и проверяется так, как будет
			// выполняться: со своими алиасами поверх библиотеки, а не с
			// алиасами соседнего файла.
			own := registry.Clone()
			registered := true
			for _, decl := range file.Aliases {
				if err := own.Add("", decl); err != nil {
					problems = append(problems, err.Error())
					registered = false
				}
			}
			if !registered {
				continue
			}

			for _, problem := range lang.Check(file, own) {
				problems = append(problems, problem.String())
			}
		}

		name := args[0]
		switch {
		case len(args) > 1:
			name = fmt.Sprintf("файлов: %d", len(args))
		case name == "-" && checkName != "":
			name = checkName
		case name == "-":
			name = "stdin"
		}
		return reportCheck(cmd, name, problems)
	},
}

func reportCheck(cmd *cobra.Command, name string, problems []string) error {
	if len(problems) == 0 {
		cmd.Printf("✅ %s: ошибок не найдено\n", name)
		return nil
	}
	for _, problem := range problems {
		cmd.Println(problem)
	}
	return fmt.Errorf("%s: найдено ошибок: %d", name, len(problems))
}

func init() {
	checkCmd.Flags().StringVar(&checkName, "name", "", "имя сценария, поданного на stdin")
}
