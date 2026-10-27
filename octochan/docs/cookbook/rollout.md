# Раскатка на группу серверов

**Задача.** Каждому серверу группы — свой конфиг из общего шаблона с
собственным лимитом соединений. Сбой на одном сервере не должен
останавливать остальные; нужен отчёт, согласование и откат.

## Инвентарь

Серверы и их лимиты — в инвентаре, а не в тексте сценария: один и тот же
сценарий раскатывает и всю группу, и её часть.

```yaml
# ~/.octochan/inventory.yaml
groups:
  db: {max_connections: 200}         # по умолчанию для группы
hosts:
  db1: {groups: [db]}
  db2: {groups: [db], max_connections: 300}
  db3: {groups: [db, db_new], max_connections: 150}
```

```
$ ochan inventory
Источник local: /home/ops/.octochan/inventory.yaml

+ db1              db                       max_connections=200
+ db2              db                       max_connections=300
+ db3              db, db_new               max_connections=150

Хостов: 3, новых: 3 (+)
```

## Сценарий

```
# rollout.ochan — раскатка лимита соединений на группу серверов
отчёт = file("rollout.log")

parallel for сервер in inventory("group=db") {
    try {
        c = config("base.conf")
        put(c, "max_connections", str(сервер.max_connections))
        save(c, "out/" + сервер.host + ".conf")
        print(отчёт, сервер.host, "готов, max_connections =", сервер.max_connections)
    } catch ошибка {
        print(отчёт, сервер.host, "ПРОПУЩЕН:", ошибка)
    }
}
print("готово, отчёт — rollout.log")
```

- `inventory("group=db")` — хосты группы словарями: `host`, `groups` и
  переменные; переменная хоста важнее переменной группы. Источники
  инвентаря читает ochan до запуска, и в песочнице сценарий видит тот же
  список.
- `parallel for` идёт по серверам одновременно (не больше
  `defaults.max_parallel_tasks`), в порядке инвентаря.
- `try`/`catch` превращает сбой одного сервера в строку отчёта, а не в
  остановку всей раскатки.
- `print(отчёт, …)` дописывает строку в файл. Строки итераций ложатся в
  порядке элементов, а не завершения, — план совпадёт с выполнением.

## 1. Проверка и план

```
$ ochan check rollout.ochan
rollout.ochan: ошибок не найдено

$ ochan plan rollout.ochan
готово, отчёт — rollout.log

План: rollout.ochan -> —

Изменений: 10

  /srv/configs/out/db1.conf
    добавить   max_connections = 200
    добавить   shared_buffers = 4GB
    добавить   work_mem = 16MB

  /srv/configs/out/db2.conf
    добавить   max_connections = 300
    …
  /srv/configs/rollout.log
    записать   файл — новый файл, 114 байт
```

Только новые серверы — `--limit`: сценарий тот же, `inventory()` видит
только подходящие хосты.

```
$ ochan plan rollout.ochan --limit group=db_new
…
Изменений: 4

  /srv/configs/out/db3.conf
    добавить   max_connections = 150
    …
```

В режиме плана ничего не записано — ни конфиги, ни отчёт; каталог `out/`
должен существовать уже сейчас, иначе план откажет, как отказало бы и
выполнение.

## 2. Заявка, решение, выполнение

```
$ ochan request rollout.ochan
Заявка e1ad62eded создана (ждёт решения)
…
$ ochan approve e1ad --comment "окно с 02:00"
$ ochan apply --request e1ad
Заявка e1ad62eded, сценарий rollout.ochan
готово, отчёт — rollout.log
Заявка e1ad62eded выполнена, записано файлов: 4
Откат: ochan rollback df3033a89b

$ cat rollout.log
db1 готов, max_connections = 200
db2 готов, max_connections = 300
db3 готов, max_connections = 150
```

Перед выполнением план строится заново. Поправили `base.conf` или
инвентарь после согласования — выполнение отменится: записалось бы не то,
что согласовали. Заявка помнит `--limit`, с которым её оформили.

## 3. Откат

```
$ ochan history df30
…
Файлы до операции (к этому вернёт откат):
  /srv/configs/out/db1.conf — не было, откат удалит
  /srv/configs/out/db2.conf — не было, откат удалит
  /srv/configs/out/db3.conf — не было, откат удалит
  /srv/configs/rollout.log — не было, откат удалит

$ ochan rollback df30
удалён /srv/configs/out/db1.conf: его создала операция
…
Операция df3033a89b откатена, файлов: 4 (откат записан как 58c33d2838)
```

Если какой-то файл поправили уже после раскатки, откат откажет целиком и
назовёт его — чужая правка не сотрётся молча. `--force` откатит всё равно.

## Что дальше

Сгенерированные конфиги забирает система доставки — Ansible, git, rsync.
Если её шаги выразить модулем команд или исполнителем, ochan проведёт
через заявку и их.

## Смотрите также

- [Язык алиасов](../guide/language.md) — `parallel for`, `try`, потоки.
- [Инвентарь и --limit](../guide/scenarios.md#инвентарь-и---limit).
- [План, заявки и откат](../guide/requests.md).
