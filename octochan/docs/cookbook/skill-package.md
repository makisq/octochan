# Свой пакет алиасов как скилл

**Задача.** Собрать типовые операции над конфигами PostgreSQL в пакет с
версией: люди вызывают его из сценариев, а формальный промпт ссылается на
него как на скилл — так модель пишет «как» только из проверенных операций.

## Пакет

```
~/.octochan/modules/pgconf/
  pgconf.ochan
  package.yaml
```

```
# pgconf.ochan — правка параметров конфигурации PostgreSQL

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
name: pgconf
version: 1.2.0
description: правка параметров конфигурации PostgreSQL
```

```
$ ochan aliases pgconf
Пакет pgconf 1.2.0 — правка параметров конфигурации PostgreSQL
  pgconf.set_param(c, name, value)             задать параметр конфига
  pgconf.store(c, path)                        записать конфиг в файл

$ ochan check
библиотека алиасов: ошибок не найдено
```

Комментарий над `alias` — его описание в `ochan aliases`. Типы параметров
проверяются при вызове: ошибка всплывает на входе в алиас, а не в его
середине. Готовый пакет — в [examples/fp/pgconf](../../examples/fp/pgconf).

## Вызов из сценария

```
c = config("prod.conf")
pgconf.set_param(c, "max_connections", "200")
pgconf.store(c, "prod.conf")
```

Заявка на такой сценарий запоминает версию пакета и суммы его файлов. Если
алиас поправили после согласования, выполнение отменится: выполнилось бы
не то, что согласовали.

## Скилл для формального промпта

```
import @pgconf v1
  provides: Config, set_param, store
  requires_class: Config
```

- `v1` подходит под 1.2.0 — версия сверяется по началу номера; `v1.3` уже
  не подойдёт, и это `⊥undef`: та же операция в другой версии может делать
  другое.
- Операции в `provides:` должны быть в пакете: проверка не верит
  объявлению на слово.
- На алиасы пакета ссылаются и жёсткие правила:
  `hard forbid store(except = {prod_conf})` — `store` пишет только в
  `prod.conf`, и это проверяется по плану.

```
$ ochan fp check examples/fp/connections.fp
examples/fp/connections.fp: P ∈ ℒ, слова однозначны, Req(P) ⊆ K, жёсткие правила совместны
Цель γ:  Goal
K:       connection_limit, memory_budget, prod_limit, set_param, store
Req(P):  set_param, store
Скилл:   @pgconf 1.2.0
```

## Новая версия

Поменяли поведение алиаса — поднимите `version`. Промпты с `v1` продолжат
работать, пока версия начинается с 1; заявки, согласованные со старым
текстом пакета, не выполнятся — их оформляют заново.

## Смотрите также

- [Модули и пакеты](../guide/modules.md)
- [Нейросеть пишет сценарий по формальному промпту](ai-script.md)
