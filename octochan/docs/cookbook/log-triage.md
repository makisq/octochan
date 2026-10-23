# Разбор журнала PostgreSQL

**Задача.** В журнале за ночь тысячи строк. Нужно быстро увидеть, что
важное, сколько его и что повторяется.

## Важное — одной командой

```
$ ochan logs rank /var/log/postgresql/postgresql.log --min error
error    2026-10-23 10:01:00 ERROR:  relation "users" does not exist
error    2026-10-23 10:01:03 ERROR:  relation "orders" does not exist
fatal    2026-10-23 10:02:00 FATAL:  password authentication failed for user "app"
```

Уровень — в начале строки, поэтому дальше работает обычный конвейер:
`| grep ^fatal`, `| wc -l`. Журнал можно подать и на stdin:

```
journalctl -u postgresql --since today | ochan logs rank - --min warning
```

## Сводка

```
$ ochan logs rank postgresql.log --summary --min error --top 5
Журнал: postgresql.log

Строк по уровням:
  panic      0
  fatal      1
  error      2
  warning    0
  notice     0
  info       1
  debug      0

Частые сообщения уровня error и выше:
  fatal          1  password authentication failed for user "app"  (впервые в строке 4)
  error          1  relation "users" does not exist  (впервые в строке 2)
  error          1  relation "orders" does not exist  (впервые в строке 3)
```

Важнее — выше, при равной важности — чаще. Время, номер процесса и числа
в сводку не попадают, поэтому одно и то же сообщение с разными значениями
— одна строка с числом повторов.

## Свои правила

Пусть неудачные входы разных пользователей считаются одной проблемой
уровня `fatal`, а отсутствующие таблицы — отдельными, по имени таблицы:

```yaml
# ~/.octochan/config.yaml
logs:
  rules:
    - level: fatal
      match: 'FATAL:\s+(?P<msg>password authentication failed for user)'
    - level: error
      match: 'ERROR:\s+(?P<msg>relation "[^"]+" does not exist)'
    - level: error
      match: 'ERROR:\s+(?P<msg>.*)'
    - level: warning
      match: 'WARNING:\s+(?P<msg>.*)'
    - level: info
      match: '\bLOG\b'
```

```
$ ochan logs rank postgresql.log --summary --min warning
…
Частые сообщения уровня warning и выше:
  fatal          2  password authentication failed for user  (впервые в строке 5)
  error          2  relation "users" does not exist  (впервые в строке 2)
  error          1  relation "orders" does not exist  (впервые в строке 3)
  warning        1  could not open statistics file  (впервые в строке 7)
```

- Правила проверяются по порядку, первое совпадение решает: частное
  правило — выше общего.
- Группа `(?P<msg>…)` — суть сообщения для сводки; что в неё не попало
  (имя пользователя), группы не дробит. Числа в ней заменяются на `N`.
- Строка без совпадений получает `logs.default_level`.
- Ошибка в выражении называет правило по номеру: `logs.rules[2]: …`.

Свои уровни — тоже можно, например `levels: [low, mid, high]`; тогда
`min_level` и `default_level` должны быть из этого списка.

## Модели — только чтение

Через сервер MCP модель ранжирует журналы инструментом `rank_logs`:
файл внутри разрешённых каталогов, `min`, `summary`, `top`. Журнал самого
ochan (`ochan logs`) ей не виден: в нём действия оператора.

## Смотрите также

- [Журналы](../guide/logs.md) — журнал ochan, архивы, правила по умолчанию.
