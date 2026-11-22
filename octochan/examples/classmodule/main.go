// Модуль с классами команд — пример: команды объявляют, читают они или
// выполняют.
//
// Класс — аннотация class на команде cobra. Она уходит в ochan по gRPC
// вместе с командой, и право на группу класса открывает команду сразу, без
// правки ядра:
//
//   - pgstat show — класс read: её выполняет любая роль с правом read, в том
//     числе llm;
//   - pgstat reset — класс execution (то же, что apply): только роль с apply;
//   - pgstat raw — без класса: как раньше, только по имени или с all.
//
// Сборка и установка:
//
//	go build -o pgstat.hcplugin ./examples/classmodule
//	ochan '!!install' pgstat.hcplugin
package main

import (
	"ochan/pluginhost"

	"github.com/spf13/cobra"
)

type module struct{}

func (module) Init(map[string]any) error { return nil }
func (module) Name() string              { return "pgstat" }
func (module) Version() string           { return "0.1.0" }

func (module) GetCommands() []*cobra.Command {
	root := &cobra.Command{
		Use:         "pgstat",
		Short:       "Статистика PostgreSQL — пример модуля с классами команд",
		Annotations: map[string]string{"class": "read"},
	}
	root.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Показать статистику (класс read — от pgstat)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println("pg_stat: соединений 42, попаданий в кеш 99.1%")
			return nil
		},
	}, &cobra.Command{
		Use:         "reset",
		Short:       "Сбросить статистику (класс execution)",
		Annotations: map[string]string{"class": "execution"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println("pg_stat: статистика сброшена")
			return nil
		},
	})
	return []*cobra.Command{root, {
		Use:   "pgraw",
		Short: "Команда без класса: только по имени или с all",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println("pgraw: выполнено")
			return nil
		},
	}}
}

func main() { pluginhost.ServeCommandModule(module{}) }
