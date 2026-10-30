# Модель чинит инцидент, не успела — инженерам

**Задача.** На серверах контура без дежурного (HA-AI) PostgreSQL перестал
принимать соединения. Модель должна починить сама — но не бесконечно: у
критичного инцидента пятнадцать минут, потом — письмо инженерной группе со
всем, что модель успела сделать и увидеть. Решён инцидент — тоже письмо, а
раз в неделю руководителям — статистика: что модель решает сама, а что нет.

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
  notify_resolved: true            # по умолчанию: письмо и о решённом
  report:
    every: 7d                      # статистика раз в неделю, за неделю
    to: [leads@corp.example, dba@corp.example]
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
Инцидент b8f8921ac4 (critical) открыт: модель чинит сама до 10:36 (1m)
Не успеет — эскалация инженерам: oncall@corp.example, dba@corp.example

$ ochan incident note b8f8 "pg_stat_activity: 200 соединений из max_connections 200, держит пул приложения"
Инцидент b8f8921ac4: заметка записана, шагов 2

$ ochan incident attach b8f8 /var/log/postgresql/postgresql.log
Инцидент b8f8921ac4: приложен postgresql.log, байт 261

$ ochan request raise.yaml --incident b8f8
Заявка fe040010d2 создана (ждёт решения)
Изменений: 1
Заявка fe040010d2 в free агента db7 (политика fix-db) — выполняется без человека, от имени роли deployer
Заявка fe040010d2, сервис demo, исполнитель Demo
Создано:
  - demo-CI007: max_connections = 300
```

Заявка укладывается в политику — выполнена без человека. Заявка за её
пределами ждала бы master:

```
$ ochan request wide.yaml                  # max_connections: 900
Заявка c56db80d63 вне free: db7 (fix-db): параметр max_connections = 900 вне допустимого (200..400) — ждёт master
```

Помогло бы — модель закрыла бы инцидент сама: `ochan incident resolve b8f8
"…"`. Не помогло, а срок прошёл.

## 3. Срок вышел

```
$ ochan incident
2026-10-30 10:35  b8f8921ac4  critical  открыт, модель чинит ещё 49s       PostgreSQL на db7 не принимает новые соединения

… минута …

$ ochan incident note b8f8 "попробую ещё раз"
ошибка: инцидент b8f8921ac4 эскалирован инженерам (время вышло: модель не решила за 1m): модель по нему больше не действует
```

Эскалацию сделала эта же команда по пути; сервер (`ochan serve`) на
primary сделал бы её сам, не позже чем через 30 секунд после срока, а без
сервера — `ochan incident check` в cron.

## 4. Письмо инженерам

```
Subject: [ochan] Инцидент b8f8921ac4 (critical) эскалирован: время вышло: модель не решила за 1m
To: oncall@corp.example, dba@corp.example

Инцидент b8f8921ac4 — эскалация инженерам

Уровень:  critical (модель чинит сама 1m)
Открыт:   2026-10-30 10:35 (llm)
Хосты:    db7
Что:      PostgreSQL на db7 не принимает новые соединения
Причина:  время вышло: модель не решила за 1m

Что сделано:
  10:35:42  llm        open: PostgreSQL на db7 не принимает новые соединения
  10:35:42  llm        note: pg_stat_activity: 200 соединений из max_connections 200, держит пул приложения
  10:35:42  llm        attach postgresql.log: журнал postgresql.log, байт 261
  10:35:42  llm        request fe040010d2: demo через Demo, операций: 1
  10:35:42  deployer   apply fe040010d2: выполнена
  10:36:43  ochan      escalate: время вышло: модель не решила за 1m

Заявки по инциденту:
  fe040010d2  выполнена  demo через Demo, операций: 1

Журналы (важное; целиком — во вложениях):
  postgresql.log:
    fatal  2026-10-30 14:02:03 MSK FATAL:  sorry, too many clients already
    fatal  2026-10-30 14:02:04 MSK FATAL:  sorry, too many clients already
    warning  2026-10-30 14:02:09 MSK WARNING:  could not send data to client: Broken pipe

Журнал аудита за время инцидента:
  + 10:35:42 llm        incident: b8f8921ac4 открыт (critical, модель чинит до 10:36): PostgreSQL на db7 не принимает новые соединения
  + 10:35:42 llm        incident note b8f8 pg_stat_activity: 200 соединений из max_connections 200, держит пул приложения
  + 10:35:42 llm        request --incident=b8f8 raise.yaml
  + 10:35:42 deployer   free: agent db7 approve fe040010d2 (free: политика fix-db, без человека)
  + 10:35:42 deployer   free: agent db7 apply fe040010d2 (free: политика fix-db, без человека)
  …

Вложения: incident-b8f8921ac4.docx, postgresql.log
```

Строки журнала отобраны правилами `logs.rules` — тем же, что `ochan logs
rank`. Инженер видит, что модель подняла `max_connections` и что этого не
хватило, — и не начинает с того же.

## 5. Инженер решил — письмо о решении

Инцидент у людей: модель по нему не действует, free заявки с `--incident
b8f8` не выполняет. Закрывает его человек:

```
$ ochan incident resolve b8f8 "инженер ограничил пул приложения: pool_size 150, соединения идут"
Инцидент b8f8921ac4 решён за 1m24s: инженер ограничил пул приложения: pool_size 150, соединения идут
Письмо о решении: oncall@corp.example, dba@corp.example
```

Тем, кому ушла эскалация, уходит и письмо о решении — чтобы инцидент не
висел у них открытым:

```
Subject: [ochan] Инцидент b8f8921ac4 (critical) решён человеком (master) за 1m24s: инженер ограничил пул приложения: pool_size 150, соединения идут
To: oncall@corp.example, dba@corp.example

Инцидент b8f8921ac4 решён человеком (master) за 1m24s

Уровень:   critical (модель чинит сама 1m)
Открыт:    2026-10-30 10:35 (llm)
Хосты:     db7
Что:       PostgreSQL на db7 не принимает новые соединения
Эскалация: 2026-10-30 10:36 (время вышло: модель не решила за 1m)
Решение:   инженер ограничил пул приложения: pool_size 150, соединения идут

Что сделано:
  10:35:42  llm        open: PostgreSQL на db7 не принимает новые соединения
  …
  10:36:43  ochan      escalate: время вышло: модель не решила за 1m
  10:37:06  master     resolve: инженер ограничил пул приложения: pool_size 150, соединения идут

Заявки по инциденту:
  fe040010d2  выполнена  demo через Demo, операций: 1
```

Решила модель сама — письмо такое же, с темой «решён моделью за …»: люди
знают, что было и что модель сделала, хотя их и не звали. Не нужно —
`incidents.notify_resolved: false`.

В карточке видны оба письма и ушли ли они:

```
$ ochan incident b8f8
Инцидент:  b8f8921ac4
Состояние: решён
Уровень:   critical (модель чинит сама 1m)
Открыт:    2026-10-30 10:35:42 (llm)
Итог:      инженер ограничил пул приложения: pool_size 150, соединения идут (2026-10-30 10:37:06)
Письмо:    эскалация — oncall@corp.example, dba@corp.example
Письмо:    решение — oncall@corp.example, dba@corp.example
Хосты:     db7
Что:       PostgreSQL на db7 не принимает новые соединения

Шаги:
  10:35:42  llm        open: PostgreSQL на db7 не принимает новые соединения
  10:35:42  llm        note: pg_stat_activity: 200 соединений из max_connections 200, держит пул приложения
  10:35:42  llm        attach postgresql.log: журнал postgresql.log, байт 261
  10:35:42  llm        request fe040010d2: demo через Demo, операций: 1
  10:35:42  deployer   apply fe040010d2: выполнена
  10:36:43  ochan      escalate: время вышло: модель не решила за 1m
  10:37:06  master     resolve: инженер ограничил пул приложения: pool_size 150, соединения идут

Журналы:
  postgresql.log, байт 261
```

## 6. Статистика и отчёт

За то же время у модели были ещё два инцидента: один она решила, другой
сама отдала инженерам, поняв, что причина не в PostgreSQL:

```
$ ochan incident open --level low --host db7 "диск /var/lib/postgresql на db7 заполнен на 91%"
Инцидент f15a2f3421 (low) открыт: модель чинит сама до 14:35 (4h)
Не успеет — эскалация инженерам: dba@corp.example
$ ochan incident note f15a "старые сегменты WAL держит слот репликации standby_old, реплики нет с 2026-09-01"
Инцидент f15a2f3421: заметка записана, шагов 2
$ ochan incident resolve f15a "удалён слот standby_old, WAL очищен checkpoint, занято 58%"
Инцидент f15a2f3421 решён за 0s: удалён слот standby_old, WAL очищен checkpoint, занято 58%
Письмо о решении: dba@corp.example

$ ochan incident open --level medium --host db7 "репликация db7 → db8 отстаёт на 40 минут"
Инцидент 1643e8958e (medium) открыт: модель чинит сама до 11:35 (1h)
Не успеет — эскалация инженерам: dba@corp.example
$ ochan incident note 1643 "wal_receiver на db8 переподключается каждые 30 секунд: could not receive data from WAL stream"
Инцидент 1643e8958e: заметка записана, шагов 2
$ ochan incident escalate 1643 "обрывы соединения между db7 и db8 — причина в сети, а не в настройках PostgreSQL"
Инцидент 1643e8958e эскалирован инженерам (llm: обрывы соединения между db7 и db8 — причина в сети, а не в настройках PostgreSQL): письмо — dba@corp.example
```

Статистика считается по инцидентам, открытым за период (здесь — неделя
из `incidents.report.every`; в рецепте модель «чинит» за секунды, отсюда
`0s`):

```
$ ochan incident stats
Инциденты с 2026-10-23 10:37 по 2026-10-30 10:37 (7d): 3

Решены:                       2
  моделью до срока            1   в среднем 0s, медиана 0s
  человеком до эскалации      0
  людьми после эскалации      1   в среднем 23s, медиана 23s после эскалации
Не решены:                    1
  у инженеров                 1
  у модели, срок не вышел     0

Модель решила сама 1 из 3 (33%), не справилась — 2 (67%):
  срок вышел                  1
  отдала инженерам сама       1
  отдал инженерам человек     0

По уровням:
  уровень     срок  всего  модель  эскалация  не решены
  low           4h      1       1          0          0
  medium        1h      1       0          1          1
  critical      1m      1       0          1          0

Нерешённые:
  1643e8958e  medium    у инженеров  репликация db7 → db8 отстаёт на 40 минут

Отчёт: каждые 7d → leads@corp.example, dba@corp.example; начнётся при первой проверке: ochan serve или incident check
```

Эскалированный и потом решённый людьми инцидент считается дважды: в
«решены» (людьми после эскалации) и в «модель не справилась» — это разные
вопросы. Период — `--for 24h`, `--for 30d`, `--for all`. Модели та же
статистика доступна в MCP (`incident_stats`): по ней видно, какие
инциденты она обычно не вытягивает, — их стоит отдавать раньше.

Отчёт уходит сам: первая проверка начинает расписание, дальше — раз в
`every`, окна подряд, без пропусков и наложений. Сейчас, вне расписания, —
`incident report`:

```
$ ochan incident check
Отчёт по инцидентам: каждые 7d, первый — 2026-11-06 10:37

$ ochan incident report
Отчёт по инцидентам за 7d ушёл: leads@corp.example, dba@corp.example
```

```
Subject: [ochan] Инциденты за 7d (2026-10-23 — 2026-10-30): 3, решила модель 1, у инженеров 1, открыто 0
To: leads@corp.example, dba@corp.example

Инциденты с 2026-10-23 10:37 по 2026-10-30 10:37 (7d): 3
…
Вложения: incidents-2026-10-23_2026-10-30.csv
```

```
id,level,timer,opened,source,hosts,summary,result,state,escalated,escalated_by,reason,resolved,resolved_by,outcome,took,engineers_took
b8f8921ac4,critical,1m,2026-10-30 10:35:42,llm,db7,PostgreSQL на db7 не принимает новые соединения,timeout,resolved,2026-10-30 10:36:43,ochan,время вышло: модель не решила за 1m,2026-10-30 10:37:06,master,"инженер ограничил пул приложения: pool_size 150, соединения идут",1m24s,23s
f15a2f3421,low,4h,2026-10-30 10:35:42,llm,db7,диск /var/lib/postgresql на db7 заполнен на 91%,model,resolved,,,,2026-10-30 10:35:42,llm,"удалён слот standby_old, WAL очищен checkpoint, занято 58%",0s,
1643e8958e,medium,1h,2026-10-30 10:35:42,llm,db7,репликация db7 → db8 отстаёт на 40 минут,gave_up,escalated,2026-10-30 10:35:43,llm,"llm: обрывы соединения между db7 и db8 — причина в сети, а не в настройках PostgreSQL",,,"llm: обрывы соединения между db7 и db8 — причина в сети, а не в настройках PostgreSQL",,
```

`result` — итог для таблиц: `model`, `human`, `timeout`, `gave_up`,
`escalated`, `open`. Отчёт не ушёл (почта недоступна) — `incident stats`
покажет ошибку, повтор — через 10 минут, окно не сдвигается.

## Что дальше

- Модель может отдать инцидент и раньше срока, когда понимает, что её
  знаний не хватит: `ochan incident escalate 1643 "…"` — в MCP это
  `incident_escalate`, и описание инструмента прямо велит так делать
  вместо новых попыток.
- Мониторинг открывает инциденты сам: `POST /v1/incidents` на сервере с
  секретом `ochan incident secret` ([«HA-AI»](../guide/ha-ai.md#мониторинг)).

## Смотрите также

- [HA-AI, free и инциденты](../guide/ha-ai.md)
- [Новые серверы без человека](new-servers.md) — та же политика для `ochan watch`.
