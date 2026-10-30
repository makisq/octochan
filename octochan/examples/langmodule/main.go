// Модуль языка для ochan — пример: немецкий.
//
// Модуль языка — обычный модуль ochan (gRPC через go-plugin), у которого
// нет команд, зато есть словарь: русский текст справки или сообщения →
// перевод. Словарь лежит рядом в JSON и вшивается в модуль; шаблон для
// него выгружает ochan lang template de.
//
// Сборка и установка:
//
//	go build -o lang-de.hcplugin ./examples/langmodule
//	ochan '!!install' lang-de.hcplugin
//
// Затем в config.yaml — lang: de, и со следующего запуска ochan говорит
// по-немецки. Чего нет в словаре, остаётся по-русски.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"octochan/pluginhost"

	"github.com/spf13/cobra"
)

//go:embed de.json
var dictionary []byte

type language struct{ entries map[string]string }

func (language) GetCommands() []*cobra.Command { return nil }
func (language) Init(map[string]any) error     { return nil }
func (language) Name() string                  { return "lang-de" }
func (language) Version() string               { return "0.1.0" }

func (l language) Translations() []pluginhost.Language {
	return []pluginhost.Language{{Code: "de", Name: "Deutsch", Entries: l.entries}}
}

func main() {
	var entries map[string]string
	if err := json.Unmarshal(dictionary, &entries); err != nil {
		fmt.Fprintf(os.Stderr, "lang-de: словарь не разбирается: %v\n", err)
		os.Exit(1)
	}
	pluginhost.ServeCommandModule(language{entries: entries})
}
