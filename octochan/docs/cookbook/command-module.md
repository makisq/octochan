# Свой модуль команд

**Задача.** Добавить в ochan свою команду — например, обёртку над внутренней
системой, — не трогая код самого ochan.

## Модуль

Модуль — отдельная программа на Go, реализующая
`pluginhost.CommandModule`. Обвязка go-plugin, gRPC и перенос команд cobra
живут в `pluginhost`; модулю достаточно отдать команды. Пример —
`examples/testmodule` в репозитории:

```go
package main

import (
    "fmt"

    "ochan/modules/pluginhost"

    "github.com/spf13/cobra"
)

type TestModule struct{}

func (t *TestModule) Name() string                { return "test-module" }
func (t *TestModule) Version() string             { return "1.3.0" }
func (t *TestModule) Init(_ map[string]any) error { return nil } // сюда приходят значения настроек

func (t *TestModule) GetCommands() []*cobra.Command {
    testCmd := &cobra.Command{
        Use:   "test",
        Short: "Тестовая команда из модуля",
        RunE: func(cmd *cobra.Command, _ []string) error {
            debug, _ := cmd.Flags().GetBool("debug")
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
            count, _ := cmd.Flags().GetInt("count")
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

func main() { pluginhost.ServeCommandModule(&TestModule{}) }
```

Модуль импортирует `ochan/modules/pluginhost`, поэтому собирается внутри модуля
`ochan` (как `examples/testmodule/`) или с `replace ochan => …` на него в своём `go.mod`.
До 0.3.0 модуль назывался `octochan`: в старом модуле поправьте импорты
`octochan/…` на `ochan/…`.

## Сборка и установка

```
$ go build -o test-module.hcplugin ./examples/testmodule
$ ochan '!!install' test-module.hcplugin
Модуль test-module.hcplugin установлен и загружен
  команд: 1 — ochan inspect test-module.hcplugin
$ ochan modules
Модули:
  test-module.hcplugin         test-module v1.3.0 — команд: 1
$ ochan test generate -n 2
Generating 2 test items...
Item 1
Item 2
```

При установке модуль копируется в `~/.octochan/modules` и фиксируется его
SHA-256. При каждом запуске ochan загружает только модули с совпадающей
суммой: файл, подменённый после установки, выполнен не будет. После
пересборки модуль ставится заново — так обновляется сумма.

## Права

Команда модуля проходит проверку прав, как своя. Право выдаётся по имени
команды и открывает её подкоманды:

```
$ ochan role create bot --allow read
$ OCTOCHAN_ROLE=bot ochan test
ошибка: роль bot не может выполнять test: нужно право test или all
$ ochan role grant bot test
Роль bot: read, test
$ OCTOCHAN_ROLE=bot ochan test generate -n 1
Generating 1 test items...
Item 1
```

## Что ещё умеет модуль

- **Флаги** любых типов cobra переносятся как есть; передаются только явно
  заданные — значения по умолчанию модуль знает сам.
- **Ошибка** `RunE` — ненулевой код возврата ochan с текстом ошибки модуля.
- **Время** команды ограничено `defaults.module_command_timeout` (10m).
- **Язык**: реализовав ещё и `Translations()`, модуль приносит язык
  интерфейса — см. [«ochan на своём языке»](translate.md).
- **Оболочка** перезагружает изменившиеся модули сама, без перезапуска;
  `!!reload` — загрузить все модули заново и перечитать `config.yaml`.
- **Настройки**: реализовав `Settings() []pluginhost.Setting`, модуль
  объявляет, что в нём настраивается, — `!!install` сам пишет это в
  `config.yaml` (раздел `modules.<модуль>`), а значения приходят в `Init`.
  Пошагово — рецепт [«Настройки своего модуля»](module-settings.md).
- **Права**: аннотация `class` у команды (`read`, `execution`) открывает её
  правом группы, без `role grant` по имени — см.
  [«Класс команды»](../guide/modules.md#класс-команды-read-или-execution).

## Смотрите также

- [Модули и пакеты](../guide/modules.md)
- [Настройки своего модуля](module-settings.md)
- [modules/pluginhost/command.proto](../../modules/pluginhost/command.proto) — контракт модуля.
