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
| `Required` | без значения модуль не работает: ochan предупреждает при установке и при каждом запуске |

Команда читает значение из того, что пришло в `Init`:

```go
cmd.Printf("pgstat: %s:%s, кластер %s\n", settings["host"], settings["port"], cluster)
```

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
pgstat: 127.0.0.1:5432, кластер не задан
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
pgstat: db1.example.internal:5432, кластер main
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

Что модуль настраивает и что сейчас задано — `ochan inspect` и `ochan
modules`:

```
$ ochan inspect ~/.octochan/modules/pgstat.hcplugin

=== Информация о модуле ===
Файл:   ~/.octochan/modules/pgstat.hcplugin
Имя:    pgstat
Версия: 0.2.0
Настройки (config.yaml, раздел modules.pgstat):
  modules.pgstat.cluster = main — имя кластера в отчётах pgstat (обязательно)
  modules.pgstat.host = db1.example.internal — сервер PostgreSQL, статистику которого показывает pgstat
  modules.pgstat.port = 5432 — порт PostgreSQL
  modules.pgstat.refresh = 30s — как часто обновлять статистику
…
$ ochan modules
Модули:
  pgstat.hcplugin              pgstat v0.2.0 — команд: 3; настроек: 4 (modules.pgstat)
```

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
`executors.<имя исполнителя>`.

## Подводные камни

- **Секретов в настройках нет.** `config.yaml` лежит на диске и попадает в
  резервные копии. Токен — `ochan auth --executor <имя>`, в коде —
  `runnerhost.Token(ctx)`.
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
