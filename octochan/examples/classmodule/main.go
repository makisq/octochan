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
//   - pgraw — без класса: как раньше, только по имени или с all.
//
// И общие строки для f @ (…): node call <плейбук> изображает узел с API.
// Ответ на плейбук сервиса node_login несёт string_i, а плейбук сервиса
// node_tune без верного string_i получает 500. Аннотация shared
// «node_tune:string_i» говорит ядру, что string_i у node_tune — общая
// строка: ochan сам поставит ? и подставит её из ответа node_login:
//
//	ochan shell
//	> node.call @ ("tune.yaml" | "login.yaml")
//
// И настройки: модуль объявляет их (Settings), а !!install сам пишет их в
// config.yaml — раздел modules.pgstat, с пояснениями и значениями по
// умолчанию. Значения модуль получает в Init при каждой загрузке:
//
//	modules:
//	  pgstat:
//	    host: db1.example.internal
//	    cluster: main
//
// // Сборка и установка:
//
//	go build -o pgstat.hcplugin ./examples/classmodule
//	ochan '!!install' pgstat.hcplugin
package main

import (
	"fmt"
	"os"
	"strings"

	"ochan/modules/pluginhost"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type module struct{}

// settings — значения настроек, которые ochan передал в Init.
var settings = map[string]string{}

func (module) Init(values map[string]any) error {
	for k, v := range values {
		settings[k] = fmt.Sprint(v)
	}
	return nil
}

// setting — значение настройки в команде: как сейчас в config.yaml
// (pluginhost.Settings), а с ochan старше 0.3.1 — то, что пришло в Init.
func setting(cmd *cobra.Command, key string) string {
	if values := pluginhost.Settings(cmd.Context()); values != nil {
		return values[key]
	}
	return settings[key]
}

func (module) Name() string    { return "pgstat" }
func (module) Version() string { return "0.1.0" }

// Settings — что настраивается: ochan пишет это в config.yaml при установке.
func (module) Settings() []pluginhost.Setting {
	return []pluginhost.Setting{
		{Key: "host", Default: "127.0.0.1", Description: "сервер PostgreSQL, статистику которого показывает pgstat"},
		{Key: "port", Default: "5432", Description: "порт PostgreSQL"},
		{Key: "cluster", Description: "имя кластера в отчётах pgstat", Required: true},
	}
}

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
	root.AddCommand(&cobra.Command{
		Use:   "where",
		Short: "Показать, куда смотрит pgstat (настройки modules.pgstat)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cluster := setting(cmd, "cluster")
			if cluster == "" {
				cluster = "не задан"
			}
			token := "нет"
			if pluginhost.Token(cmd.Context()) != "" {
				token = "есть"
			}
			cmd.Printf("pgstat: %s:%s, кластер %s, токен: %s\n", setting(cmd, "host"), setting(cmd, "port"), cluster, token)
			return nil
		},
	})
	node := &cobra.Command{Use: "node", Short: "Узел с API — пример общих строк для f @ (…)"}
	node.AddCommand(&cobra.Command{
		Use:   "call <плейбук>",
		Short: "Отправить плейбук узлу и напечатать ответ",
		Args:  cobra.ExactArgs(1),
		Annotations: map[string]string{
			"class":  "execution",
			"shared": "node_tune:string_i",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var playbook struct {
				Service    string            `yaml:"service"`
				Parameters map[string]string `yaml:"parameters"`
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			// «ключ: ?» — общая строка, которую ждут; YAML без кавычек её не читает.
			data = []byte(strings.ReplaceAll(string(data), ": ?\n", ": \"?\"\n"))
			if err := yaml.Unmarshal(data, &playbook); err != nil {
				return err
			}
			switch playbook.Service {
			case "node_login":
				cmd.Println("HTTP 200")
				cmd.Println("string_i: k-42-session")
			case "node_tune":
				if playbook.Parameters["string_i"] != "k-42-session" {
					return fmt.Errorf("HTTP 500: нет верного string_i (получено %q)", playbook.Parameters["string_i"])
				}
				cmd.Println("HTTP 200: параметры применены")
			default:
				return fmt.Errorf("HTTP 404: сервис %q", playbook.Service)
			}
			return nil
		},
	})
	return []*cobra.Command{root, node, {
		Use:   "pgraw",
		Short: "Команда без класса: только по имени или с all",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Печать мимо cmd — в stdout процесса модуля: она тоже уходит
			// хосту вместе с ответом, в >| и в ochan().
			fmt.Println("pgraw: выполнено")
			return nil
		},
	}}
}

func main() { pluginhost.ServeCommandModule(module{}) }
