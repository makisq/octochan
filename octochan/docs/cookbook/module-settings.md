# Настройки своего модуля

**Задача.** У модуля есть то, что пользователь должен уметь поменять:
адрес сервера, порт, путь, регион API. Пользователь не должен ни знать
названий этих ключей, ни писать раздел конфига руками — как в PostgreSQL,
где расширение само приносит свои параметры в `postgresql.conf`.

Модуль объявляет настройки, `!!install` пишет их в `config.yaml`, а модуль
получает значения при каждой загрузке. Пример — `examples/classmodule`
(модуль `pgstat`) и `examples/runnermodule` (исполнитель с настройкой).

## 1. Объявить настройки

Модуль реализует `Settings() []pluginhost.Setting` рядом с обычными
методами `pluginhost.CommandModule`, а значения забирает в `Init`:

```go
// settings — значения настроек, которые ochan передал в Init.
var settings = map[string]string{}

func (module) Init(values map[string]any) error {
    for k, v := range values {
        settings[k] = fmt.Sprint(v)
    }
    return nil
}

// Settings — что настраивается: ochan пишет это в config.yaml при установке.
func (module) Settings() []pluginhost.Setting {
    return []pluginhost.Setting{
        {Key: "host", Default: "127.0.0.1", Description: "сервер PostgreSQL, статистику которого показывает pgstat"},
        {Key: "port", Default: "5432", Description: "порт PostgreSQL"},
        {Key: "cluster", Description: "имя кластера в отчётах pgstat", Required: true},
    }
}
```

| Поле | Что задаёт |
|---|---|
| `Key` | имя в разделе модуля: `modules.<модуль>.<Key>`. Буквы, цифры, `_` и `-`, первой — буква; иначе модуль не загрузится |
| `Default` | значение по умолчанию текстом, как в YAML: `30s`, `5432`, `[a, b]`; пусто — умолчания нет |
| `Description` | одна строка «что это» — она встанет пояснением в `config.yaml` |
| `Required` | без значения модуль не работает: ochan предупреждает при установке и при каждом запуске. Вместе с `Default` умолчание уже и есть значение: строка остаётся под `#`, предупреждения нет |

Команда читает значения в каждом вызове — `pluginhost.Settings(cmd.Context())`,
а с ochan старше 0.3.1 (он их не шлёт) — то, что пришло в `Init`:

```go
// setting — значение настройки в команде: как сейчас в config.yaml
// (pluginhost.Settings), а с ochan старше 0.3.1 — то, что пришло в Init.
func setting(cmd *cobra.Command, key string) string {
    if values := pluginhost.Settings(cmd.Context()); values != nil {
        return values[key]
    }
    return settings[key]
}

cmd.Printf("pgstat: %s:%s, кластер %s, токен: %s\n", setting(cmd, "host"), setting(cmd, "port"), cluster, token)
```

Токен модуля — `pluginhost.Token(cmd.Context())`; пользователь задаёт его
`ochan auth --executor <модуль>` или переменной `OCTOCHAN_TOKEN_<МОДУЛЬ>`,
как токен исполнителя.

## 2. Установить

```
$ go build -o pgstat.hcplugin ./examples/classmodule
$ ochan '!!install' pgstat.hcplugin
Модуль pgstat.hcplugin установлен и загружен
  команд: 3 — ochan inspect pgstat.hcplugin
  настройки записаны в ~/.octochan/config.yaml (раздел modules.pgstat):
    modules.pgstat.cluster = "" — имя кластера в отчётах pgstat (обязательно)
    modules.pgstat.host = 127.0.0.1 — сервер PostgreSQL, статистику которого показывает pgstat
    modules.pgstat.port = 5432 — порт PostgreSQL
внимание: модуль pgstat: не задано обязательное — modules.pgstat.cluster в ~/.octochan/config.yaml — задайте и выполните !!reload
```

В конце `config.yaml` появился раздел — в том же виде, что весь шаблон
конфига: пояснение над настройкой, умолчание под `#`. Обязательное без
умолчания — без `#` и пустым, чтобы его было видно:

```yaml
# Настройки модулей — их пишет !!install: modules.<модуль>.<настройка>
modules:
  # pgstat 0.1.0
  pgstat:
    # сервер PostgreSQL, статистику которого показывает pgstat
    # host: 127.0.0.1
    # порт PostgreSQL
    # port: 5432
    # имя кластера в отчётах pgstat (обязательно)
    cluster: ""
```

Модуль уже работает — на умолчаниях:

```
$ ochan pgstat where
внимание: модуль pgstat: не задано обязательное — modules.pgstat.cluster в ~/.octochan/config.yaml
pgstat: 127.0.0.1:5432, кластер не задан, токен: нет
```

Откуда какое значение — `ochan config show`:

```
$ ochan config show modules
Конфиг: ~/.octochan/config.yaml
modules.pgstat.cluster = ""  [config.yaml; обязательно, не задано]
modules.pgstat.host    = 127.0.0.1  [умолчание модуля pgstat]
modules.pgstat.port    = 5432  [умолчание модуля pgstat]
```

Что будет без `cluster` — заранее, ещё до установки, показывает `ochan
inspect`:

```
$ ochan inspect pgstat.hcplugin
…
  Без modules.pgstat.cluster ochan при установке, при каждом запуске и после !!reload пишет в stderr:
    внимание: модуль pgstat: не задано обязательное — modules.pgstat.cluster в ~/.octochan/config.yaml
  Выключить — defaults.warn_missing_settings: false. Модуль загружается и сам решает, работать ли:
  ошибка из его Init — отказ загрузиться с её текстом.
```

Токен модуля:

```
$ ochan auth --executor pgstat секрет
Токен модуля pgstat сохранён: ~/.octochan/tokens/pgstat — его команды получают токен в каждом вызове
$ ochan pgstat where
pgstat: 127.0.0.1:5432, кластер не задан, токен: есть
```

## 3. Задать значения

Пользователь снимает `#` с нужной строки и пишет своё:

```yaml
  pgstat:
    # сервер PostgreSQL, статистику которого показывает pgstat
    host: db1.example.internal
    …
    cluster: main
```

Значения модуль получает при загрузке, поэтому в оболочке — `!!reload`, без
выхода; отдельные команды `ochan …` видят их сразу:

```
ochan> !!reload
Перечитано: конфиг ~/.octochan/config.yaml
  модули: pgstat — команд 3
  алиасов: 0
  переменные и алиасы оболочки сохранены; сбросить — !!reset
ochan> pgstat where
pgstat: db1.example.internal:5432, кластер main, токен: есть
```

## 4. Новая версия — новая настройка

Добавили в 0.2.0 настройку `refresh` (`Default: "30s"`) и поставили модуль
заново. ochan дописывает только новое; то, что пользователь задал, и уже
записанные строки не трогаются, версия в заголовке блока — новая:

```
$ ochan '!!install' pgstat.hcplugin
Модуль pgstat.hcplugin установлен и загружен
  команд: 3 — ochan inspect pgstat.hcplugin
  настройки записаны в ~/.octochan/config.yaml (раздел modules.pgstat):
    modules.pgstat.cluster = main — имя кластера в отчётах pgstat (обязательно)
    modules.pgstat.host = db1.example.internal — сервер PostgreSQL, статистику которого показывает pgstat
    modules.pgstat.port = 5432 — порт PostgreSQL
    modules.pgstat.refresh = 30s — как часто обновлять статистику
```

```yaml
modules:
  # pgstat 0.2.0
  pgstat:
    # сервер PostgreSQL, статистику которого показывает pgstat
    host: db1.example.internal
    # порт PostgreSQL
    # port: 5432
    # имя кластера в отчётах pgstat (обязательно)
    cluster: main
    # как часто обновлять статистику
    # refresh: 30s
```

Что модуль настраивает и что сейчас задано — `ochan inspect` (и до
установки) и `ochan modules`:

```
$ ochan inspect ~/.octochan/modules/pgstat.hcplugin

=== Модуль ===
Файл:      ~/.octochan/modules/pgstat.hcplugin
Имя:       pgstat
Версия:    0.2.0
Размер:    20762069 байт
SHA-256:   bdfe1cd4c670602144a48a28efc1e036b2bb7ceee93c29838ef7a750c4b4722f
Установлен: да, этот файл (~/.octochan/modules/pgstat.hcplugin)

Команды (5):
  pgstat reset             Сбросить статистику (класс execution) — класс apply
  pgstat show              Показать статистику (класс read — от pgstat) — класс read
  pgstat where             Показать, куда смотрит pgstat (настройки modules.pgstat) — класс read
  …

Настройки (config.yaml, раздел modules.pgstat; значения — в Init модуля и в каждом вызове команды):
  modules.pgstat.cluster = main — имя кластера в отчётах pgstat (обязательно)
  modules.pgstat.host = db1.example.internal — сервер PostgreSQL, статистику которого показывает pgstat
  modules.pgstat.port = 5432 — порт PostgreSQL
  modules.pgstat.refresh = 30s — как часто обновлять статистику
$ ochan modules
Модули:
  pgstat.hcplugin              pgstat v0.2.0 — команд: 3; настроек: 4 (modules.pgstat)
```

`ochan inspect --json` — то же для программ: `name`, `version`, `size`,
`sha256`, `installed_path`, `installed_same`, `commands`, `runners`
(`kind`, `executor`, `services`, `ops`, `token_command`, `token_env`),
`settings` (`key`, `default`, `description`, `required`).

## 5. Исполнитель с настройками

Модуль-исполнитель объявляет настройки так же, а читает их в каждом вызове
исполнителя — `runnerhost.Settings(ctx)`, рядом с токеном
`runnerhost.Token(ctx)`:

```go
func (module) Settings() []pluginhost.Setting {
    return []pluginhost.Setting{{Key: "region", Default: "eu-1", Description: "регион API демонстрационного исполнителя"}}
}

func (demo) Status(ctx context.Context, ref string) (*scenario.Status, error) {
    region := runnerhost.Settings(ctx)["region"]
    …
}
```

```
$ ochan '!!install' demo-runner.hcplugin
Модуль demo-runner.hcplugin установлен и загружен
  исполнитель Demo: сервисы demo; токен — ochan auth --executor demo
  настройки записаны в ~/.octochan/config.yaml (раздел modules.demo-runner):
    modules.demo-runner.region = eu-1 — регион API демонстрационного исполнителя
$ ochan status --executor demo demo-1

Статус demo-1 (demo)
----------------------------
Состояние: done
Токен: нет
Регион: eu-1
```

Второй модуль встаёт в тот же раздел `modules:`:

```yaml
modules:
  # pgstat 0.2.0
  pgstat:
    …
  # demo-runner 0.2.0
  # demo-runner:
  #   # регион API демонстрационного исполнителя
  #   region: eu-1
```

Исполнитель получает настройки в таком порядке, сильнее — ниже:
`defaults.*` (общие умолчания исполнителей) → `modules.<модуль>` →
`executors.<имя исполнителя>`. Что из этого дошло до исполнителя и откуда —
`ochan config show --executor`:

```
$ ochan config show --executor demo
Исполнитель demo получает в каждом вызове (слои: defaults.* → modules.<модуль> → executors.demo; сильнее — позже):
  http_timeout       = 30s  [defaults.http_timeout — умолчание]
  max_parallel_tasks = 5  [defaults.max_parallel_tasks — умолчание]
  api_key            = …  [executors.demo.api_key]
  region             = us-2  [executors.demo.region]
  токен: …  [~/.octochan/tokens/demo]
```

Для модуля команд — то же, только его раздел:

```
$ ochan config show --executor pgstat
Команды модуля pgstat получают в каждом вызове (modules.pgstat):
  host = 127.0.0.1  [modules.pgstat.host — умолчание]
  port = 5432  [modules.pgstat.port — умолчание]
  токен: не задан — ochan auth --executor pgstat или OCTOCHAN_TOKEN_PGSTAT
```

## Подводные камни

- **Секретов в настройках нет.** `config.yaml` лежит на диске и попадает в
  резервные копии. Токен — `ochan auth --executor <имя>`, в коде —
  `runnerhost.Token(ctx)` у исполнителя и `pluginhost.Token(cmd.Context())`
  у команды. `ochan config show` значения с именем вроде `token`,
  `password`, `secret`, `cookie`, `key` скрывает, но это страховка, а не
  место для секрета.
- **Значения — строки.** `Init` получает `map[string]any`, но значения в
  нём — текст: `5432` — строка `"5432"`, список — YAML одной строкой
  (`[a, b]`). Разбирайте сами.
- **Значение по умолчанию держите и в коде.** ochan, собранный до
  `Settings`, у модуля их не спрашивает, а `Init` не вызывает вовсе: модуль
  должен работать и без них. И наоборот — модуль, собранный до `Settings`,
  на новом ochan просто остаётся без настроек.
- **Отказаться загружаться** модуль может сам: ошибка из `Init` — и ochan
  его не загрузит, с текстом ошибки. Обязательная настройка без значения
  сама по себе загрузку не останавливает — решает модуль.
- **Предупреждение о незаданном обязательном** выключается в
  `config.yaml`: `defaults.warn_missing_settings: false`. Список настроек
  при `!!install` по-прежнему печатается, с пометкой «обязательно».
- **`Required` вместе с `Default`** — умолчание считается значением:
  строка ставится под `#`, предупреждения нет, модуль получает умолчание.
  Чтобы пользователь обязательно вписал своё, `Default` не задавайте.
- **Настройки в `Init` — на момент загрузки.** Значение сейчас — в
  команде, `pluginhost.Settings(cmd.Context())`; в оболочке правка
  `config.yaml` видна после `!!reload`, отдельной команде `ochan …` — сразу.
- **Вывод — через `cmd`, но и `fmt.Println` дойдёт.** Модуль, собранный с
  ochan 0.3.1, отдаёт хосту всё, что команда напечатала, вместе с ответом —
  и мимо `cmd`. Модуль, собранный раньше, печатает мимо `cmd` в свой
  stdout, и такая строка может опоздать к `>|`: пересоберите модуль.
- **`!!install --remove` раздел не удаляет:** значения пригодятся, если
  модуль поставят снова. Убрать — руками.
- **Пояснения пишите на языке пользователей модуля.** ochan переводит на
  язык `lang` только свои пояснения шаблона; текст `Description` остаётся
  как его написал автор.

## Смотрите также

- [Модули и пакеты: настройки модуля](../guide/modules.md#настройки-модуля-settings)
- [Свой модуль команд](command-module.md)
- [Настройки: modules](../reference/config.md#modules--настройки-модулей)
- [modules/pluginhost/command.proto](../../modules/pluginhost/command.proto) — `SettingSpec`, `GetSettings`.
