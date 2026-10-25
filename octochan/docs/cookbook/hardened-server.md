# Общий сервер против чужой роли

**Задача.** На [общем оркестраторе](shared-orchestrator.md) работает
команда, и одной из ролей завладел чужой: подсмотрел пароль, получил
ноутбук. Нужно, чтобы он не вышел за свои файлы, не согласовал сам себе,
не выполнил больше, чем положено, а следы попыток остались в журнале,
который не переписать незаметно.

## Роли

На сервере, в `/srv/octochan`:

```
$ ochan role create dan --allow read,plan,request,approve,apply --deny rollback --nlogin dan --npsw
Новый пароль роли dan: Роль dan создана: read, plan, request, approve, apply
Запрещено: rollback
$ ochan role create eve --allow read,approve --nlogin eve --npsw
$ ochan role limit dan run=2/1h
Роль dan: пределы run=2/1h
$ ochan serve --listen 0.0.0.0:7443
```

- `--deny rollback` — запрет: перекрывает право `apply`, в которое
  `rollback` входит.
- `run=2/1h` — не больше двух сценариев в час, по всем сессиям роли.
- Область не задана, но на сервере это не значит «всё»: роль без своей
  области ограничена каталогом сервера.

## Что получает чужой

Подключившись как dan:

```
$ ochan remote --role dan orchestrator.example:7443 print /etc/passwd
Подключено к orchestrator.example:7443, роль dan
ошибка: /etc/passwd вне области роли dan (/srv/octochan)
```

Хранилище ролей сервера с ключом подписи допусков — главная цель: прочитав
его, можно подделать допуск master. Сценарий клиента на сервере всегда идёт
в песочнице, а рабочий каталог ochan закрыт и сверх неё:

```
$ cat steal.ochan
print(read("/home/octochan/.octochan/roles.bin"))
$ ochan remote --role dan orchestrator.example:7443 run steal.ochan
ошибка: steal.ochan:1:11: не удалось прочитать /home/octochan/.octochan/roles.bin: /home/octochan/.octochan/roles.bin вне разрешённых каталогов (/srv/octochan)
$ ochan remote --role dan orchestrator.example:7443 run --sandbox --root /home/octochan steal.ochan
ошибка: /home/octochan вне области роли dan (/srv/octochan)
```

Своя заявка — только через другого человека:

```
$ ochan remote --role dan orchestrator.example:7443 request tune.ochan
Заявка 20011ccfc4 создана (ждёт решения)
$ ochan remote --role dan orchestrator.example:7443 approve 20011ccfc4
ошибка: заявку роли dan согласует другая роль: свою заявку не согласуют
```

Ева согласует, dan выполняет — а откатить, чтобы замести следы, не может:

```
$ ochan remote --role eve orchestrator.example:7443 approve 20011ccfc4
Заявка 20011ccfc4: согласована (root, роль eve)
$ ochan remote --role dan orchestrator.example:7443 apply --request 20011ccfc4
Заявка 20011ccfc4 выполнена, записано файлов: 1
Откат: ochan rollback 776fc62566
$ ochan remote --role dan orchestrator.example:7443 rollback 776fc62566
ошибка: команда rollback роли dan запрещена
```

Предел — третий сценарий за час:

```
$ ochan remote --role dan orchestrator.example:7443 run tune.ochan
$ ochan remote --role dan orchestrator.example:7443 run tune.ochan
$ ochan remote --role dan orchestrator.example:7443 run tune.ochan
ошибка: предел роли dan исчерпан: run=2/1h; следующая попытка через 59m59s
```

Предел считает допущенные команды, а не удачные: сценарий, упавший уже
внутри песочницы, тоже израсходовал единицу.

## Журнал

На сервере:

```
$ ochan audit --denied
- 2026-10-23 13:32:09  dan        203.0.113.7      approve 20011ccfc4
    заявку роли dan согласует другая роль: свою заявку не согласуют
- 2026-10-23 13:32:09  dan        203.0.113.7      rollback 776fc62566
    команда rollback роли dan запрещена
- 2026-10-23 13:32:10  dan        203.0.113.7      run tune.ochan
    предел роли dan исчерпан: run=2/1h; следующая попытка через 59m59s

+ разрешено, - отказано
$ ochan audit verify
Журнал аудита цел: записей 27, с 2026-10-23 13:32:06 по 2026-10-23 13:32:10
Хеш последней записи: 8a8e3e7d168968af9045f9b5dc27cbb2b1d39c673fc71cac15ddaa9894d35ab2
```

Без `--denied` видно всё: входы, команды, смены ролей, с адресом клиента.

Хеш последней записи унесите с машины — в тикет, в чат, в почту. Тот, кто
удалит из журнала неудобную строку, порвёт цепочку:

```
$ ochan audit verify
ошибка: журнал аудита не прошёл проверку: строка 12: ссылка на предыдущую запись не сходится: запись перед ней удалили, вставили или правили
```

А тот, кто обрежет хвост целиком, цепочку не порвёт — но сохранённый хеш
это покажет:

```
$ ochan audit verify --head 8a8e3e7d1689
ошибка: журнал аудита не прошёл проверку: записи 8a8e3e7d1689 в журнале нет: журнал обрезан или подменён
```

## Если нужно уже

Область — свои каталоги для каждой роли:

```
$ ochan role scope dan /srv/octochan/team-a
```

Тогда dan не работает и в остальной части `/srv/octochan`: ни аргументом,
ни песочницей, ни заявкой, ни откатом.

## Смотрите также

- [Безопасность](../guide/security.md) — модель угроз целиком.
- [Роли и права](../guide/roles.md) — запреты, области, пределы, журнал.
- [Общий оркестратор](shared-orchestrator.md) — как поднять сервер.
