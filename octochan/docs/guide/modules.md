# Модули и пакеты

ochan расширяется тремя способами:

| | Что это | Что даёт |
|---|---|---|
| пакет алиасов | каталог или файл `.ochan` в `~/.octochan/modules` | сценарии поверх встроенных функций; с версией — скилл для формального промпта |
| модуль команд | Go-плагин `.hcplugin` (go-plugin + gRPC) | новые команды ochan |
| модуль языка | такой же плагин со словарём | язык интерфейса |

Пакет алиасов безопаснее плагина: это текст, который видно глазами, и он
умеет ровно то, что дают встроенные функции. Плагин — исполняемый файл:
ставится только явно и только с проверкой контрольной суммы.

## Пакет алиасов

```
~/.octochan/modules/pgconf/
  pgconf.ochan       алиасы пакета
  package.yaml       манифест: версия и описание
```

```
# pgconf.ochan
# задать параметр конфига
alias set_param(c: config, name: string, value: string) {
    put(c, name, value)
    return c
}

# записать конфиг в файл
alias store(c: config, path: string) {
    return save(c, path)
}
```

```yaml
# package.yaml
name: pgconf          # необязательно; если указано — совпадает с каталогом
version: 1.2.0        # 1, 1.2 или 1.2.3
description: правка параметров конфигурации PostgreSQL
```

```
$ ochan aliases pgconf
Пакет pgconf 1.2.0 — правка параметров конфигурации PostgreSQL
  pgconf.set_param(c, name, value)             задать параметр конфига
  pgconf.store(c, path)                        записать конфиг в файл
```

- Алиас пакета вызывается как `pgconf.set_param(…)`; короткое имя — пока
  оно не занято в нескольких пакетах (совпадение — ошибка с требованием
  уточнить, а не молчаливый выбор).
- Неизвестное поле в манифесте — ошибка: опечатка в `version` иначе тихо
  сделала бы пакет безверсионным. Однофайловый пакет (`pgconf.ochan`)
  версии не имеет.
- Версию сверяет `import @pgconf v1` формального промпта, а заявка на
  сценарий записывает версии пакетов, алиасы которых он вызывает, и суммы
  их файлов: поправленный после согласования алиас отменит выполнение.
- `ochan check` без аргументов проверяет всю библиотеку.

Готовый пример — [examples/fp/pgconf](../../examples/fp/pgconf).

## Модуль команд

Модуль — отдельный исполняемый файл, реализующий
`pluginhost.CommandModule`. Обвязка go-plugin и gRPC живёт в
`pluginhost`, модулю достаточно отдать команды cobra:

```go
type Module struct{}

func (Module) Name() string                 { return "hello" }
func (Module) Version() string              { return "1.0.0" }
func (Module) Init(map[string]any) error    { return nil }
func (Module) GetCommands() []*cobra.Command {
    return []*cobra.Command{{
        Use:   "hello",
        Short: "Поздороваться",
        RunE: func(cmd *cobra.Command, args []string) error {
            cmd.Println("привет из модуля")
            return nil
        },
    }}
}

func main() { pluginhost.ServeCommandModule(Module{}) }
```

```
go build -o hello.hcplugin ./hello
ochan '!install' hello.hcplugin
ochan modules
ochan hello
```

- При установке фиксируется SHA-256 модуля. При запуске загружаются только
  модули с совпадающей суммой: файл, подменённый после установки,
  выполнен не будет. Для разработки проверку отключает
  `defaults.allow_unverified_modules: true`.
- Команды модуля проходят проверку прав, как свои: право выдаётся по имени
  команды (`ochan role grant ci hello`).
- Ошибка команды модуля — ненулевой код возврата ochan.
- Предел времени команды модуля — `defaults.module_command_timeout` (10m).
- Оболочка перезагружает изменившиеся модули сама; после пересборки модуль
  нужно переустановить (`!install`), чтобы обновить контрольную сумму.
- `ochan inspect модуль.hcplugin` поднимает модуль отдельно и показывает
  его имя, версию и команды, не меняя набор команд текущей сессии. Это
  тоже запуск исполняемого файла, поэтому и здесь нужна контрольная сумма
  (или `allow_unverified_modules`).

Пример с подкомандами и флагами — [dwd/main.go](../../dwd/main.go).
Пошагово — рецепт [«Свой модуль команд»](../cookbook/command-module.md).

## Модуль языка

Модуль, реализующий ещё и `pluginhost.Translator`, приносит язык:
см. [«Язык интерфейса»](languages.md#другой-язык--модулем) и
[examples/langmodule](../../examples/langmodule).

## Источники и исполнители

Глубже, чем модуль, — Go-пакет внутри ochan:

- **источник** регистрирует схему (`config.RegisterSourceScheme`) и отдаёт
  конфиг по ссылке вида `схема://…` — так сделан `pgsource`;
- **исполнитель** реализует `scenario.Runner` и регистрируется за сервисом
  — так сделан RLM в `rlm` и `module`.

Ядро о них не знает: новая ниша — новый пакет, а не правка ядра.
