# Пара серверов и агенты

**Задача.** Общий оркестратор не должен быть единственной точкой отказа.
Второй узел держит копию состояния; когда первый пропал, человек повышает
второй, и клиенты с агентами переходят на него сами. Ни одна заявка при
этом не выполняется дважды.

Узлы — `a.example` и `b.example`, порт 7443. Чтобы попробовать на одной
машине, хватит двух рабочих каталогов (`HOME`) и двух портов на
`127.0.0.1`: выводы ниже получены именно так, адреса заменены.

## 1. Узлы

Один сертификат на пару — с именами обоих узлов. Узлы проверяют друг
друга им же, поэтому `tls.ca` на узлах — тот же файл:

```
a$ ochan tls init --host a.example --host b.example
a$ scp ~/.octochan/tls/server.crt ~/.octochan/tls/server.key b.example:.octochan/tls/
```

```yaml
# a: ~/.octochan/config.yaml            # b: то же, адреса наоборот
tls:
  ca: /home/octochan/.octochan/tls/server.crt     # полный путь: ~ здесь не раскрывается
ha:
  self: a.example:7443
  peer: b.example:7443
```

```
a$ ochan ha init --node a --role primary
Общий секрет пары: /home/octochan/.octochan/ha.token (права 0600) — скопируйте его на второй узел
Узел a: primary, эпоха 1 (/home/octochan/.octochan/ha.json)

a$ scp ~/.octochan/ha.token b.example:.octochan/
b$ ochan ha init --node b --role standby
Узел b: standby, эпоха 0 (/home/octochan/.octochan/ha.json)

a$ ochan serve --listen 0.0.0.0:7443
Сервер слушает https://0.0.0.0:7443
Каталог:   /srv/octochan
Пара:      узел a — primary, эпоха 1; второй узел b.example:7443
Сертификат: 48:22:B1:42:EC:22:83:81:…

b$ ochan serve --listen 0.0.0.0:7443
Сервер слушает https://0.0.0.0:7443
Каталог:   /srv/octochan
Пара:      узел b — standby, эпоха 0; второй узел a.example:7443
Сертификат: 48:22:B1:42:EC:22:83:81:…
```

Эпоху standby берёт у primary с первой копией:

```
$ ochan ha status a.example:7443,b.example:7443
a.example:7443           a — primary, эпоха 1
b.example:7443           b — standby, эпоха 1, копия от 2026-10-29 14:57:23 (2s назад)
```

Пароль master и роли людей заводят, как на
[общем оркестраторе](shared-orchestrator.md): роли с паролями копируются
на standby вместе с заявками и журналами.

## 2. Клиент знает оба адреса

```
$ ochan remote a.example:7443,b.example:7443 request prod.conf changes.conf
Подключено к a.example:7443, роль master
Заявка 6ef95ca170 создана (ждёт решения)
Изменений: 1
Согласовать: ochan approve 6ef95ca170
```

Клиент сам находит primary. Чтение можно направить на оба узла по
очереди: `--balance` или `remote.balance_reads: true`. Standby отвечает на
чтение по копии, а запись отклоняет и называет primary:

```
$ ochan remote b.example:7443 requests
Подключено к b.example:7443, роль master
внимание: primary не найден: b.example:7443: standby (эпоха 1) — подключено к standby: только чтение по копии, запись он отклонит
2026-10-29 14:57  6ef95ca170  ждёт решения  /srv/octochan/prod.conf  правка prod.conf, параметров: 1

$ ochan remote b.example:7443 approve 6ef9
Подключено к b.example:7443, роль master
внимание: primary не найден: b.example:7443: standby (эпоха 1) — подключено к standby: только чтение по копии, запись он отклонит
ошибка: узел b — standby (эпоха 1): здесь только чтение; запись — на primary a.example:7443
```

Перед выполнением заявка помечается «выполняется», и primary ждёт, пока
пометка дойдёт до standby. Пометка на резерве — то, что не даст выполнить
заявку второй раз после переключения.

```
$ ochan remote a.example:7443,b.example:7443 approve 6ef9
$ ochan remote a.example:7443,b.example:7443 apply --request 6ef9
Подключено к a.example:7443, роль master
Заявка 6ef95ca170 выполнена: /srv/octochan/prod.conf, изменено параметров: 1
Откат: ochan rollback 68543e1dbc
```

## 3. Primary пропал

```
$ ochan ha status a.example:7443,b.example:7443
a.example:7443           нет ответа: нет связи с https://a.example:7443: … connect: connection refused
b.example:7443           b — standby, эпоха 1, копия от 2026-10-29 14:59:28 (2s назад)
```

Пока человек не решил, клиенты читают со standby — журнал и заявки там
есть, — а запись им не удаётся:

```
$ ochan remote a.example:7443,b.example:7443 history
Подключено к b.example:7443, роль master
внимание: primary не найден: a.example:7443: нет связи с https://a.example:7443: …; b.example:7443: standby (эпоха 1) — подключено к standby: только чтение по копии, запись он отклонит
+ 2026-10-29 14:57:33  68543e1dbc  правка файла  /srv/octochan/prod.conf  правка по заявке 6ef95ca170, параметров: 1
```

Сама пара не переключается: без консенсуса автоматика при разрыве сети
дала бы два primary. Повышает человек — убедившись, что `a` действительно
не работает или отрезан от клиентов:

```
b$ ochan ha promote
Второй узел a.example:7443 не отвечает: нет связи с https://a.example:7443: …
Узел b — primary, эпоха 2
```

Заявки, которые в момент отказа выполнялись на `a`, `promote` объявляет
упавшими — их исход неизвестен, и второй раз они не выполняются:

```
Заявка 0badc0de00 упала: исход неизвестен: заявка выполнялась (узел a (эпоха 1), a-host, процесс 4121, с …), когда узел a перестал быть primary
```

Клиенты с обоими адресами переходят на `b` сами:

```
$ ochan remote a.example:7443,b.example:7443 request prod.conf changes.conf
Подключено к b.example:7443, роль master
Заявка 45ecb94ea0 создана (ждёт решения)
```

## 4. Бывший primary вернулся

`a` запускается, всё ещё считая себя primary эпохи 1, видит у `b` эпоху 2
и отступает сам. Своё состояние он не сливает и не стирает, а
откладывает целиком — с тем, что мог успеть записать после разрыва:

```
a$ ochan serve --listen 0.0.0.0:7443
...
ochan serve: 2026/10/29 14:59:43 пара: второй узел стал primary с эпохой 2 — этот узел теперь standby; прежнее состояние отложено в ha/diverged-e1-20261029T145943

a$ ochan ha status
Этот узел: a — standby, эпоха 2, с 2026-10-29 14:59:43 (второй узел стал primary с эпохой 2)
Отложено расходящееся состояние: ha/diverged-e1-20261029T145943
a.example:7443           a — standby, эпоха 2, копия от 2026-10-29 14:59:43 (0s назад)
b.example:7443           b — primary, эпоха 2
```

Что делать с отложенным, решает человек. Два primary одной эпохи — конфликт:
оба закрывают запись, пока один не повышен.

## 5. Агенты

Агент — машина, которая сама сообщает серверу о себе. На сервере для него
роль с правом `agent`: оно открывает только `agents beat`.

```
$ ochan remote a.example:7443,b.example:7443 role create agent --allow agent --nlogin agent --npsw
Подключено к b.example:7443, роль master
Новый пароль роли agent: Новый пароль роли agent: Роль agent создана: agent
```

На машине агента — оба адреса пары; логин и пароль только из окружения,
спросить некого:

```
db7$ OCTOCHAN_REMOTE_LOGIN=agent OCTOCHAN_REMOTE_PASSWORD=… \
     ochan agent run --server a.example:7443,b.example:7443 \
       --name db7 --group db,db_new --var svm_ci=CI007 --every 1m
Подключено к b.example:7443, роль agent
15:00:05  Агент db7 зарегистрирован (10.0.0.7)
```

`--once` — один раз, для cron. Роль агента больше ничего не может:

```
$ OCTOCHAN_REMOTE_LOGIN=agent OCTOCHAN_REMOTE_PASSWORD=… ochan remote --role agent a.example:7443,b.example:7443 requests
Подключено к b.example:7443, роль agent
ошибка: роль agent не может выполнять requests: нужно право read
```

Реестр — источник инвентаря. На обоих узлах:

```yaml
inventory:
  sources:
    agents: agents://online      # agents:// — все, online — сообщавшие о себе недавно
```

```
$ ochan remote a.example:7443,b.example:7443 agents
db7              на связи   10.0.0.7               db, db_new           0s назад, роль agent

$ ochan remote a.example:7443,b.example:7443 inventory
Источник agents: agents://online

+ db7              db, db_new               agent_address=10.0.0.7 agent_last_seen=2026-10-29T15:00:05Z agent_online=true svm_ci=CI007

Хостов: 1, новых: 1 (+)
```

Новый агент — новый хост для `ochan watch --source agents --policy …`:
машина, которую подняли, сама получает настройку по политике
([рецепт](new-servers.md)).

## Чего пара не делает

- **Не переключается сама.** Если бывший primary жив и отрезан сетью, до
  восстановления связи он принимает запись: остановите его или отрежьте от
  клиентов, прежде чем повышать резерв.
- **Не копирует файлы в `server.dir`**, которые правят команды: держите
  их на общем хранилище или работайте через исполнителей. Копируется
  состояние ochan: заявки, журналы, решения, политики, роли, аудит,
  инвентарь, реестр агентов, библиотека алиасов.
- **Не копирует настройки узла** (`config.yaml`, `tls/`, `ha.json`,
  `ha.token`), токены исполнителей и модули — их ставят на оба узла.
- `ha.require_standby: true` — не начинать выполнение, если резерв не
  подтвердил пометку; без него выполнение идёт с предупреждением.

## Смотрите также

- [Серверный режим: пара и агенты](../guide/server.md#пара-серверов-primary-и-standby)
- [Общий оркестратор](shared-orchestrator.md)
- [Порядок между заявками и состояние «упала»](../guide/requests.md#порядок-между-заявками)
