# Журналы

## Журнал ochan

`ochan logs` показывает журнал работы ochan (`--tail N` — последние N
строк, `--verbose` у любой команды дублирует журнал в stderr). Журнал,
который не помещает очередную запись в `logs.max_size`, уходит в архив с
датой в имени, и запись идёт в новый файл:

```yaml
logs:
  max_size: 100MB              # 512KB, 1GB; число без единицы — мегабайты
  prefix: "%Y-%m-%d_%H-%M-%S_" # префикс архива: %Y %m %d %H %M %S и %%
  keep: 10                     # сколько архивов хранить; 0 — все
```

```
$ ochan logs files
2026-10-23 12:00:03       8121  /home/dba/.octochan/logs/octochan.log
2026-10-23 11:58:41  104857511  /home/dba/.octochan/logs/2026-10-23_11-58-41_octochan.log
```

- Пишется всегда один файл — `octochan.log`; архивы — с префиксом.
- Архив узнаётся по шаблону префикса, поэтому чужие файлы в каталоге
  журнала `keep` не тронет, даже если журнал лежит в общем каталоге.
- Два переноса в одну секунду (или префикс без времени) друг друга не
  затирают: второй архив получает номер — `2026-10-23_1.octochan.log`.
- Если журнал перенёс другой процесс ochan, запись идёт в новый файл, а не
  в архив.
- Каталог журнала — `defaults.log_path` (по умолчанию `~/.octochan/logs`).

## Уровни важности

`ochan logs rank` ранжирует строки любого журнала — PostgreSQL, pgbouncer,
journalctl, nginx — по уровням важности. Уровень задают правила из
конфига: регулярные выражения по порядку, первое совпадение решает, как в
pg_hba.conf. Порядок уровней — от наименее важного, как
`log_min_messages` у PostgreSQL:

```yaml
logs:
  levels: [debug, info, notice, warning, error, fatal, panic]
  min_level: warning           # по умолчанию для --min
  default_level: info          # строка без совпадений
  rules:
    - level: fatal
      match: 'authentication failed|out of memory'
    - level: error
      match: 'ERROR:\s+(?P<msg>.*)'
```

```
ochan logs rank /var/log/postgresql/postgresql.log --min error
journalctl -u pgbouncer | ochan logs rank - --summary --top 5
```

```
$ ochan logs rank pg.log --summary --min error
Журнал: pg.log

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

- Без `--summary` печатаются строки уровня `--min` и выше, с уровнем в
  начале — удобно передавать дальше по конвейеру.
- Группа `(?P<msg>…)` выделяет суть сообщения для сводки; числа в ней
  заменяются на `N`, поэтому одинаковые сообщения с разными значениями
  попадают в одну группу. Без группы суть — вся строка.
- Правила по умолчанию узнают метки PANIC, FATAL (и CRITICAL, EMERG,
  ALERT), ERROR, WARNING, NOTICE, INFO и LOG, DEBUG и TRACE, а сутью
  считают текст после метки — время и номер процесса в сводку не попадают.
- Выражения — RE2 из стандартной библиотеки Go: время разбора линейно, и
  ни правило из конфига, ни строка чужого журнала не подвесят разбор.
- Ошибка в правиле называет его номер: `logs.rules[2]: выражение "(": …`.

Через сервер MCP модель ранжирует журналы инструментом `rank_logs` —
только файлы внутри разрешённых каталогов; журнал самого ochan ей не виден.
Пошагово — рецепт [«Разбор журнала PostgreSQL»](../cookbook/log-triage.md).
