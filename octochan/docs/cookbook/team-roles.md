# Роли для команды и CI

**Задача.** Стажёр только смотрит, CI проверяет и планирует, ведущий
инженер согласует заявки, а выполняет и управляет ролями только `master`.

## Роли

```
$ ochan role create intern --allow read
$ ochan role create ci --allow read,plan,request
$ ochan role create lead --allow read,plan,request,approve --nlogin lead --npsw
Новый пароль роли lead: (ввод скрыт)
$ ochan role update master --npsw          # пароль и у master
$ ochan role list
* master           all                              встроенная, пароль
  llm              read, plan, request              встроенная
  intern           read
  ci               read, plan, request
  lead             read, plan, request, approve     логин, пароль
```

Права — группы (`read`, `plan`, `request`, `approve`, `apply`, `install`,
`auth`, `admin`, `server`, `remote`), `all` или имя отдельной команды,
например команды модуля: `ochan role grant ci hello`. Неизвестное право —
ошибка, а не пустое место.

## CI — роль из окружения

```
$ OCTOCHAN_ROLE=ci ochan validate prod.conf
Конфиг валиден: prod.conf (3 параметров)
$ OCTOCHAN_ROLE=ci ochan patch prod.conf changes.conf -o x.conf
ошибка: роль ci не может выполнять patch: нужно право apply
```

Окружением задаётся только роль без логина и пароля:

```
$ OCTOCHAN_ROLE=lead ochan validate prod.conf
ошибка: OCTOCHAN_ROLE=lead: роль защищена логином или паролем, задать её окружением нельзя; войдите командой set role lead
```

## Человек — вход и возврат

```
$ ochan set role lead
Логин: lead
Пароль: (ввод скрыт)
Роль: lead
$ ochan approve 2fb5 --comment "ок"
$ ochan back role
Роль: master
```

`back role` возвращает без пароля: в master уже входили. Два возврата
подряд — туда же, откуда начали. Не на терминале логин и пароль читаются
построчно из stdin: `printf 'lead\nпароль\n' | ochan set role lead`.

## Что важно понимать

- Локально роли — защита от ошибки и граница для нейросети, но не от
  владельца компьютера: кто может удалить `~/.octochan`, тот вернёт роли к
  исходным. Границей между людьми они становятся на
  [общем оркестраторе](shared-orchestrator.md).
- Сменили пароль роли — она уходит из истории переключений, и `back role`
  в неё без входа не пустит.
- Роль `llm` — для нейросетей; её потолок `read`, `plan`, `request` не
  поднять даже master.

## Смотрите также

- [Роли и права](../guide/roles.md)
