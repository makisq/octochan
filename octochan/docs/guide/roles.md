# Роли и права

Каждая команда проверяет права текущей роли — и в командной строке, и в
оболочке, и на сервере. После установки есть две роли:

| Роль | Права | |
|---|---|---|
| `master` | `all` — всё | первая и высшая; на своём компьютере ты master |
| `llm` | `read`, `plan`, `request` | для нейросетей: читать, планировать, оформлять заявки |

Остальные роли создаёт `master`. Право — группа, `all` или имя отдельной
команды (вложенная пишется через точку: `role.create`):

| Группа | Команды |
|---|---|
| `read` | aliases, check, diff, else, find, fp, history, if, lang, logs, modules, print, requests, services, stats, status, validate |
| `plan` | plan |
| `request` | request |
| `approve` | approve, reject |
| `apply` | apply, eval, patch, rollback, run, srs, tee |
| `install` | !install, inspect |
| `auth` | auth, timer |
| `admin` | role create, role delete, role grant, role revoke, role update |
| `server` | serve, tls init |
| `remote` | connect, remote |

`help`, `shell`, `role`, `role list`, `set role` и `back role` доступны
любой роли: без них нельзя узнать, кто ты, и вернуться обратно. Право на
каждую команду — в [справочнике команд](../reference/commands.md).

## Управление ролями

```
ochan role create slave --allow read,plan
ochan role create Dan --allow read,plan,request,approve --nlogin dan --npsw
ochan role grant slave approve
ochan role revoke slave approve
ochan role update master --npsw
ochan role update slave --nlogin dan --npsw --rename worker
ochan role delete worker
ochan role list
```

```
* master           all                              встроенная
  llm              read, plan, request              встроенная
  ci               read, plan, request
  Dan              read, plan, request, approve     логин, пароль
```

Неизвестное право — ошибка, а не пустое место: опечатка не выдаст роли
ничего молча. Права модулей команд тоже можно выдавать — по имени команды.

## Переключение

```
$ ochan set role slave
Роль: slave
$ ochan apply --request 3ff2
ошибка: роль slave не может выполнять apply: нужно право apply
$ ochan back role
Роль: master
```

`back role` возвращает к роли, которая была текущей до последнего
переключения; два возврата подряд приводят туда же, откуда начали:

```
ochan set role slave     # master -> slave
ochan back role          # slave  -> master
ochan back role          # master -> slave
ochan back role master   # к конкретной роли из истории
```

Логин и пароль `back role` не спрашивает: в роль уже входили. Поэтому,
зайдя мастером и переключившись проверить роль слейва, к master
возвращаешься без пароля. Если у роли сменили логин или пароль, она уходит
из истории, и вернуться в неё можно только через `set role` со входом.

Текущая роль и история хранятся в `roles.bin` и переживают перезапуск;
в приглашении оболочки видна роль: `ochan[slave]>`.

## Логин и пароль

Если у роли задан логин или пароль, `set role` их спрашивает. Пароль
вводится без эха; не на терминале — одной строкой из stdin:

```
printf 'dan\nпароль\n' | ochan set role Dan
```

Задаётся и меняется через `--npsw`, логин — через `--nlogin`, снимаются
через `--drop-psw` и `--drop-login`.

Хранилище — двоичный `~/.octochan/roles.bin` с правами 0600 и контрольной
суммой. Логин и пароль в нём — только соль и хеш argon2id. Испорченный
файл — ошибка, а не тихий откат к master без паролей.

## Роль из окружения

```
OCTOCHAN_ROLE=ci ochan validate prod.conf
```

Так удобно запускать ochan в CI и из других программ. Окружением задаётся
только роль **без** логина и пароля — иначе `OCTOCHAN_ROLE=master` обходил
бы пароль master:

```
ошибка: OCTOCHAN_ROLE=Dan: роль защищена логином или паролем, задать её окружением нельзя; войдите командой set role Dan
```

## Роль llm

`ochan-mcp` запускает `ochan` с `OCTOCHAN_ROLE=llm`: что бы ни было
текущей ролью пользователя, нейросеть работает в своей. Это второй рубеж
за закрытой таблицей инструментов сервера MCP: даже при ошибке в таблице
`ochan` откажет в выполнении сам.

Потолок `llm` — `read`, `plan`, `request`. Выдать ей `apply`, `approve`,
`admin` и прочее нельзя и master: нейросеть предлагает, решает человек.

## Чем роли являются и чем нет

Локально роли — защита от ошибки и граница для нейросети, а не от
владельца компьютера: кто может удалить `~/.octochan`, тот вернёт роли к
исходным. Границей против других людей они становятся в
[серверном режиме](server.md): хранилище ролей лежит у сервера, а
подключаются к нему по сети.
