// Команда dwd — пример внешнего модуля octochan.
//
// Модулю достаточно реализовать pluginhost.CommandModule: вся обвязка go-plugin,
// gRPC-сервер и конвертация команд живут в core. Раньше каждый модуль
// копировал к себе ~130 строк этой обвязки, включая собственную (и другую
// по поведению) конвертацию флагов.
package main

import (
	"fmt"

	"ochan/pluginhost"

	"github.com/spf13/cobra"
)

// TestModule — демонстрационный набор команд.
type TestModule struct{}

func (t *TestModule) Name() string    { return "test-module" }
func (t *TestModule) Version() string { return "1.3.0" }

func (t *TestModule) Init(_ map[string]any) error { return nil }

func (t *TestModule) GetCommands() []*cobra.Command {
	testCmd := &cobra.Command{
		Use:   "test",
		Short: "Тестовая команда из модуля",
		RunE: func(cmd *cobra.Command, _ []string) error {
			debug, err := cmd.Flags().GetBool("debug")
			if err != nil {
				return err
			}
			if debug {
				cmd.Println("Debug mode is ON")
			}
			cmd.Println("Test command executed successfully!")
			return nil
		},
	}
	testCmd.Flags().BoolP("debug", "d", false, "Включить отладочный вывод")

	generateCmd := &cobra.Command{
		Use:   "generate",
		Short: "Сгенерировать тестовые данные",
		RunE: func(cmd *cobra.Command, _ []string) error {
			count, err := cmd.Flags().GetInt("count")
			if err != nil {
				return err
			}
			if count < 0 {
				return fmt.Errorf("count не может быть отрицательным: %d", count)
			}

			cmd.Printf("Generating %d test items...\n", count)
			for i := 1; i <= count; i++ {
				cmd.Printf("Item %d\n", i)
			}
			return nil
		},
	}
	generateCmd.Flags().IntP("count", "n", 3, "Сколько элементов сгенерировать")

	testCmd.AddCommand(generateCmd)

	return []*cobra.Command{testCmd}
}

func main() {
	pluginhost.ServeCommandModule(&TestModule{})
}
