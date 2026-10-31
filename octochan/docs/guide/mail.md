# Почта: агенты и люди

Агенты на серверах HA-AI работают друг с другом через формальный промпт, в
любой изоляции, free тоже. С людьми, то есть в письмах, общении с оператором
и статистике, они говорят на языке, который выбрал оператор.

| Кто пишет → кому | `protocol` | `natural` | `embedded` |
|---|---|---|---|
| агент → агент | формальный промпт | формальный промпт | формальный промпт |
| модель → люди | формальный промпт | естественный язык | формальный промпт, **всегда** |
| ochan → люди: эскалация, решение, статистика | формальный промпт | естественный язык | формальный промпт |
| человек → агент | формальный промпт | формальный промпт | формальный промпт |

Режим — `llm.mode` ([«Своя модель»](embedded.md)). Язык писем людям можно
задать явно:

```yaml
mail:
  lang: auto        # auto — по режиму; formal | natural — явно
  operator: operators
  groups:
    operators: [ops@corp.example]
    dba: [dba@corp.example]
```

`mail.lang: natural` в режиме `protocol` оставляет письма ochan инженерам
естественным языком. На свою модель он не действует: её письма — только
формальный промпт.

## Письма

```
$ ochan mail send --to db8 "проверь реплику, пожалуйста"          # роль llm, агент db7
ошибка: письмо не на языке: агентам пишут только формальным промптом
  1:1: ⊥syn: символ «п» вне ASCII: язык — замкнутое подмножество английского, модель пишет только его
  1:1: ⊥syn: такой последовательности нет в языке; раздел начинается с одного из: goal:, classes:, relations:, priority:, generalization, instance, requires:, import, hard forbid|require, ??n_|??a_, cell, report:
  1:1: ⊥syn: программа начинается с заголовка goal: слово

$ ochan mail send --to db8 answer.fp
Письмо 399aadf010 (формальный промпт)
  в ящики: db8

$ ochan llm generate --prompt task.fp 2>/dev/null | ochan mail send --to operators -
Письмо 1cb7d81c8d (формальный промпт)
  почтой: ops@corp.example — ушло

$ ochan mail send --to boss@corp.example answer.fp
ошибка: адрес boss@corp.example: модель пишет только агентам и группам из mail.groups, адресов она не выбирает
```

- `--to` — агент из реестра (`ochan agents`): письмо ложится в его ящик.
  Группа `mail.groups`, `operator` (группа `mail.operator`, без неё —
  инженерная группа инцидентов) или адрес — письмо уходит по почте
  (`defaults.smtp_*`). Через запятую — нескольким адресатам.
- Язык проверяется до отправки. Формальное письмо — промпт без ⊥syn. Письмо
  модели проверяется строже: без комментариев и без символов вне ASCII.
- У формального письма нет темы: тема — его цель (`[ochan] goal: connection —
  llm, агент db7`). `--subject` — только для письма на естественном языке.
- Модель (роль `llm`) пишет только агентам и группам: адрес она не выбирает.

Ящик агента — `ochan mail`. Агента берут из `--agent`, а у модели — из
`ochan-mcp --agent`. Модель читает только ящик своего агента и письма,
которые написала сама. Письмо целиком — `ochan mail <номер>`:

```
$ ochan mail                       # агент db8
2026-10-31 08:51  399aadf010  llm, агент db7         → db8                      formal   goal: connection

$ ochan mail 399a
Письмо:  399aadf010
Время:   2026-10-31 08:51:57
От:      llm, агент db7
Кому:    db8
В ящики: db8
Язык:    формальный промпт

goal: connection

classes:
  Connection : "connection"
  Limit      : "limit"

cell db7:
  set Connection.active = 200
  set Limit.max_connections = 200
  set Limit.reserved = 3
```

Письма лежат в `~/.octochan/mail/`, по файлу на письмо. Там же записано,
кому письмо ушло и не было ли ошибки. Каталог копируется на standby.

## Письма ochan формальным промптом

Если людям пишут формальным промптом, то письма самого ochan (эскалация,
решение инцидента, статистика) тоже на языке. Факты в них записаны ячейками
`set`, цель письма — то, о чём оно. Тексты мониторинга и модели (что
случилось, заметки) — это записи, а не речь ochan. Они уходят вложением
`incident-<номер>.txt`, как журналы.

```
Subject: [ochan] goal: failure — incident 1be70bc3cb critical escalated gave_up
To: oncall@corp.example, dba@corp.example

goal: failure

classes:
  Failure : "failure"
  Log     : "log"

cell incident:
  set Failure.incident = incident_1be70bc3cb
  set Failure.level = critical
  set Failure.timer = 15 min
  set Failure.state = escalated
  set Failure.result = gave_up
  set Failure.source = llm
  set Failure.host = db7
  set Failure.escalated_by = llm
  set Failure.escalated_after = 0 s
  set Failure.steps = 3

cell logs:
  set Log.postgresql_log = 141 B

Вложения: incident-1be70bc3cb.txt, postgresql.log
```

Статистика за период — тоже ячейками, таблица `.csv` прикладывается, как и
раньше:

```
Subject: [ochan] goal: count — incidents 7d 2026-10-24..2026-10-31: total 1, model 0, engineers 0, open 0

goal: count

classes:
  Count  : "count"
  Period : "period"
  Level  : "level"

cell period:
  set Period.length = 7 d

cell incidents:
  set Count.total = 1
  set Count.resolved = 1
  set Count.resolved_by_model = 0
  …
  set Count.gave_up = 1

cell levels:
  set Level.critical_total = 1
  …
```

Каждое такое письмо проходит `ochan fp check`: тесты проверяют это для
эскалации, решения и отчёта. `result` — итог инцидента, как в таблице
статистики: `model`, `human`, `timeout`, `gave_up`, `escalated`, `open`.

## Модель через MCP

`mail_send` — письмо агенту или группе, `mail_inbox` — ящик агента и письмо
по номеру ([«Сервер MCP»](../../mcp/README.md#инструменты)). Для сервера
письмо — запись, как шаг инцидента: в системе оно ничего не меняет.

## Права

| Действие | Право |
|---|---|
| `mail`, письмо по номеру | `read` |
| `mail send` | `request` |

## Смотрите также

- [Своя модель: режим embedded](embedded.md)
- [HA-AI, free и инциденты](ha-ai.md) — эскалация, решение, статистика.
