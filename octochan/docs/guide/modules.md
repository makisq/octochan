# Модули и пакеты

ochan расширяется двумя видами модулей:

| | Что это | Что даёт |
|---|---|---|
| пакет алиасов | каталог или файл `.ochan` | сценарии поверх встроенных функций; с версией — скилл для формального промпта |
| модуль gRPC | плагин `.hcplugin` (go-plugin + gRPC) | что объявит: команды ochan, язык интерфейса, исполнителей сценариев |

Пакет алиасов безопаснее плагина: это текст, который видно глазами, и он
умеет ровно то, что дают встроенные функции. Плагин — исполняемый файл:
ставится только явно и только с проверкой контрольной суммы.

## Установка

Всё ставится одной командой — в `~/.octochan/modules`:

```
ochan '!install' ssh-runner.hcplugin   # модуль: команды, языки, исполнители
ochan '!install' ./pgconf              # пакет алиасов; .hcplugin из каталога — вместе с ним
ochan '!install' util.ochan            # однофайловый пакет
ochan '!install' --remove pgconf       # удалить пакет (или модуль — по имени файла)
ochan modules                          # что установлено и что каждый модуль принёс
```

```
Модули:
  demo-runner.hcplugin         demo-runner v0.1.0 — исполнитель Demo (demo)
  lang-de.hcplugin             lang-de v0.1.0 — языки: de
Пакеты алиасов:
  pgconf                       v1.2.0 — алиасов: 2
```

- Пакет проверяется до копирования: пакет с ошибкой не ставится и
  установленный не портит. Копируются только `*.ochan` и `package.yaml`.
- Имя пакета — его префикс в сценариях (`pgconf.store`), поэтому в нём
  только буквы, цифры и `_`.
- Скилл и его исполнитель лежат в одном каталоге и ставятся вместе:
  алиасы — пакетом, `.hcplugin` рядом — модулем.

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

## Модуль-исполнитель

Исполнитель — способ выполнять сценарии: по SSH, через Ansible, через API
своей системы. Модуль, реализующий `pluginhost.RunnerProvider`, приносит
исполнителей, и ядро выбирает их по полю `service` сценария, как
встроенных: `plan`, `apply`, `status`, `--wait` работают с ними так же.

```go
type ssh struct{}

func (ssh) Kind() string { return "SSH" }
func (ssh) Plan(ctx context.Context, s *scenario.Scenario) (*config.Plan, error) { … }
func (ssh) Apply(ctx context.Context, s *scenario.Scenario) (*scenario.Result, error) { … }
// по желанию: Wait(ctx, ref) и Status(ctx, ref)

func (Module) Runners() []pluginhost.RunnerSpec {
    return []pluginhost.RunnerSpec{runnerhost.Expose(ssh{}, "ssh-restart", "ssh-tune")}
}
```

- По проводу идёт JSON — сценарий, план, ссылки, статус; формат описан в
  [runnerhost](../../runnerhost/runnerhost.go). Модулю на другом языке
  достаточно реализовать тот же gRPC-сервис и тот же JSON.
- Свой API исполнитель выбирает сам. Токен ему задаёт оператор —
  `ochan auth --executor ssh` (файл `~/.octochan/tokens/ssh`, права 0600)
  или `OCTOCHAN_TOKEN_SSH`; ядро передаёт его при каждом вызове, в коде
  исполнителя он — `runnerhost.Token(ctx)`.
- Ошибка `apply` идёт вместе с тем, что исполнитель успел создать: следы
  во внешней системе не теряются.
- Сервис, который уже выполняет встроенный исполнитель или модуль раньше по
  порядку, модуль не перехватывает — ochan предупредит.

Пример — [examples/runnermodule](../../examples/runnermodule).

## Источники

Глубже, чем модуль, — Go-пакет внутри ochan: **источник** регистрирует
схему (`config.RegisterSourceScheme`) и отдаёт конфиг по ссылке вида
`схема://…` — так сделан `pgsource`. Встроенные исполнители (`rlm`,
`module`) регистрируются так же, пакетом.
