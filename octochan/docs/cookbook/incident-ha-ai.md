# Модель чинит инцидент, не успела — инженерам

**Задача.** На серверах контура без дежурного (HA-AI) PostgreSQL перестал
принимать соединения. Модель должна починить сама — но не бесконечно: у
критичного инцидента пятнадцать минут, потом — письмо инженерной группе со
всем, что модель успела сделать и увидеть.

В рецепте срок `critical` — одна минута, чтобы дождаться эскалации; выводы
настоящие. Исполнитель — демонстрационный модуль `examples/runnermodule`
(сервис `demo`), почта — локальный приёмник SMTP.

## 1. Контур и free

```yaml
# ~/.octochan/config.yaml
defaults:
  smtp_host: smtp.corp.example
  smtp_from: ochan@corp.example
incidents:
  levels: {low: 4h, medium: 1h, high: 30m, critical: 1m}
  engineers: [dba@corp.example]
  notify:
    critical: [oncall@corp.example, dba@corp.example]
```

```yaml
# raise.yaml — что модель может сделать сама
service: demo
parameters: {max_connections: 300}
targets:
  - group: db_new
```

```yaml
# fix-db.yaml — в каких пределах
name: fix-db
scenario: raise.yaml
targets: [group=db_new]
params:
  max_connections: 200..400
expires: 2026-12-31
```

Всё ниже до раздела «Модель» — от master:

```
$ ochan policy add fix-db.yaml
$ ochan role create deployer --allow read,request,apply
$ ochan ha init --node a --role primary --ai-contour
Общий секрет пары: /home/octochan/.octochan/ha.token (права 0600) — скопируйте его на второй узел
Узел a: primary, эпоха 1 (/home/octochan/.octochan/ha.json)
Контур:    HA-AI
внимание: в этом контуре нет человека-оператора; free включает только master — ochan agents free

$ ochan agents free db7 --role deployer --for 4h --policy fix-db
Агент db7: isolation free (до 14:03, роль deployer, политика fix-db)
Отозвать: ochan agents free db7 --revoke
```

## 2. Модель

Модель работает в роли `llm` — через MCP или командами:

```
$ ochan incident open --level critical --host db7 "PostgreSQL на db7 не принимает новые соединения"
Инцидент 88fb030508 (critical) открыт: модель чинит сама до 10:04 (1m)
Не успеет — эскалация инженерам: oncall@corp.example, dba@corp.example

$ ochan incident note 88fb "pg_stat_activity: 200 соединений из max_connections 200, держит пул приложения"
Инцидент 88fb030508: заметка записана, шагов 2

$ ochan incident attach 88fb /var/log/postgresql/postgresql.log
Инцидент 88fb030508: приложен postgresql.log, байт 261

$ ochan request raise.yaml --incident 88fb
Заявка 5ce65d2552 создана (ждёт решения)
Изменений: 1
Заявка 5ce65d2552 в free агента db7 (политика fix-db) — выполняется без человека, от имени роли deployer
Заявка 5ce65d2552, сервис demo, исполнитель Demo
Создано:
  - demo-CI007: max_connections = 300
```

Заявка укладывается в политику — выполнена без человека. Заявка за её
пределами ждала бы master:

```
$ ochan request wide.yaml                  # max_connections: 900
Заявка c56db80d63 вне free: db7 (fix-db): параметр max_connections = 900 вне допустимого (200..400) — ждёт master
```

Помогло бы — модель закрыла бы инцидент сама: `ochan incident resolve 88fb
"…"`. Не помогло, а срок прошёл.

## 3. Срок вышел

```
$ ochan incident
2026-10-30 10:03  88fb030508  critical  открыт, модель чинит ещё 46s       PostgreSQL на db7 не принимает новые соединения

… минута …

$ ochan incident note 88fb "попробую ещё раз"
ошибка: инцидент 88fb030508 эскалирован инженерам (время вышло: модель не решила за 1m): модель по нему больше не действует
```

Эскалацию сделала эта же команда по пути; сервер (`ochan serve`) на
primary сделал бы её сам, не позже чем через 30 секунд после срока, а без
сервера — `ochan incident check` в cron.

```
$ ochan incident 88fb
Инцидент:  88fb030508
Состояние: эскалирован
Уровень:   critical (модель чинит сама 1m)
Открыт:    2026-10-30 10:03:46 (llm)
Итог:      время вышло: модель не решила за 1m (2026-10-30 10:05:03)
Письмо:    oncall@corp.example, dba@corp.example
Хосты:     db7
Что:       PostgreSQL на db7 не принимает новые соединения

Шаги:
  10:03:46  llm        open: PostgreSQL на db7 не принимает новые соединения
  10:03:47  llm        note: pg_stat_activity: 200 соединений из max_connections 200, держит пул приложения
  10:03:47  llm        attach postgresql.log: журнал postgresql.log, байт 261
  10:03:47  llm        request 5ce65d2552: demo через Demo, операций: 1
  10:03:47  deployer   apply 5ce65d2552: выполнена
  10:05:03  ochan      escalate: время вышло: модель не решила за 1m

Журналы:
  postgresql.log, байт 261
```

## 4. Письмо инженерам

```
Subject: [ochan] Инцидент 88fb030508 (critical) эскалирован: время вышло: модель не решила за 1m
To: oncall@corp.example, dba@corp.example

Инцидент 88fb030508 — эскалация инженерам

Уровень:  critical (модель чинит сама 1m)
Открыт:   2026-10-30 10:03 (llm)
Хосты:    db7
Что:      PostgreSQL на db7 не принимает новые соединения
Причина:  время вышло: модель не решила за 1m

Что сделано:
  10:03:46  llm        open: PostgreSQL на db7 не принимает новые соединения
  10:03:47  llm        note: pg_stat_activity: 200 соединений из max_connections 200, держит пул приложения
  10:03:47  llm        attach postgresql.log: журнал postgresql.log, байт 261
  10:03:47  llm        request 5ce65d2552: demo через Demo, операций: 1
  10:03:47  deployer   apply 5ce65d2552: выполнена
  10:05:03  ochan      escalate: время вышло: модель не решила за 1m

Заявки по инциденту:
  5ce65d2552  выполнена  demo через Demo, операций: 1

Журналы (важное; целиком — во вложениях):
  postgresql.log:
    fatal  2026-10-30 14:02:03 MSK FATAL:  sorry, too many clients already
    fatal  2026-10-30 14:02:04 MSK FATAL:  sorry, too many clients already
    warning  2026-10-30 14:02:09 MSK WARNING:  could not send data to client: Broken pipe

Журнал аудита за время инцидента:
  + 10:03:46 llm        incident: 88fb030508 открыт (critical, модель чинит до 10:04): PostgreSQL на db7 не принимает новые соединения
  + 10:03:47 llm        incident note 88fb030508 pg_stat_activity: 200 соединений из max_connections 200, держит пул приложения
  + 10:03:47 llm        request --incident=88fb030508 raise.yaml
  + 10:03:47 deployer   free: agent db7 approve 5ce65d2552 (free: политика fix-db, без человека)
  + 10:03:47 deployer   free: agent db7 apply 5ce65d2552 (free: политика fix-db, без человека)
  …

Вложения: incident-88fb030508.docx, postgresql.log
```

Строки журнала отобраны правилами `logs.rules` — тем же, что `ochan logs
rank`. Инженер видит, что модель подняла `max_connections` и что этого не
хватило, — и не начинает с того же.

## Что дальше

- Инцидент у людей: модель по нему не действует, free заявки с
  `--incident 88fb` не выполняет. Закрывает его человек: `ochan incident
  resolve 88fb "инженер ограничил пул приложения"`.
- Модель может отдать инцидент и раньше срока, когда понимает, что её
  знаний не хватит: `ochan incident escalate 88fb "причина в сети, а не в
  конфиге"` — в MCP это `incident_escalate`, и описание инструмента прямо
  велит так делать вместо новых попыток.
- Мониторинг открывает инциденты сам: `POST /v1/incidents` на сервере с
  секретом `ochan incident secret` ([«HA-AI»](../guide/ha-ai.md#мониторинг)).

## Смотрите также

- [HA-AI, free и инциденты](../guide/ha-ai.md)
- [Новые серверы без человека](new-servers.md) — та же политика для `ochan watch`.
