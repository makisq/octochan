# Релиз из CI: роль на время окна

**Задача.** Роль развёртывания нужна только во время релиза и только для
боевых серверов. Пароль от неё, утёкший в понедельник, не должен давать
ничего; релиз, зависший на полпути, не должен оставлять роль открытой.

## Триггер

```yaml
# ~/.octochan/config.yaml
triggers:
  release:
    event: ci/release
    grant:
      role: deployer
      scope: [group=prod]
      max_window: 2h
```

```yaml
# ~/.octochan/inventory.yaml
hosts:
  app1: {groups: [app, prod], svm_ci: CI101}
  app2: {groups: [app, prod], svm_ci: CI102}
  app9: {groups: [app, stage], svm_ci: CI109}
```

```yaml
# deploy.yaml
service: demo
parameters: {version: 2.4.1}
targets:
  - group: app
```

```
$ ochan role create deployer --allow read,plan,request,apply
Роль deployer создана: read, plan, request, apply
$ ochan role create ci --allow trigger
Роль ci создана: trigger
```

Роль `ci` может только открывать и закрывать окна: какую роль открывает
окно и над чем, решает `config.yaml`, а не CI.

## Вне окна роль не может ничего

```
$ OCTOCHAN_ROLE=deployer ochan plan deploy.yaml
ошибка: роль deployer действует только в окне триггера (release): не активна — ждёт события

$ ochan role show deployer
…
Триггеры:  release (событие ci/release, окно до 2h)
Сейчас:    не активна — ждёт события
```

## CI начал релиз

```
$ OCTOCHAN_ROLE=ci ochan trigger fire release --id 1234
Окно release#1234: роль deployer активна до 12:03, цели — group=prod
Закрыть раньше: ochan trigger end release --id 1234
```

Без клиента ochan — вебхуком сервера: секрет `ochan trigger secret release`
и `POST /v1/events/release` с телом `{"id": "1234", "state": "start"}`.

В окне — свои права, но только над `group=prod`:

```
$ OCTOCHAN_ROLE=deployer ochan plan deploy.yaml
ошибка: цель app9 вне области окна release#1234 (group=prod): роль в окне триггера работает только над ней

$ OCTOCHAN_ROLE=deployer ochan apply deploy.yaml --limit group=prod
Сервис demo, исполнитель Demo
План: deploy.yaml (demo, исполнитель Demo) -> —

Изменений: 2

  CI101
    задача     version = 2.4.1

  CI102
    задача     version = 2.4.1

Создано:
  - demo-CI101: version = 2.4.1
  - demo-CI102: version = 2.4.1

$ OCTOCHAN_ROLE=deployer ochan request deploy.yaml --limit group=prod
Заявка 0387daea58 создана (ждёт решения)

$ ochan requests --after release#1234
Окно release#1234: роль deployer, с 2026-10-30 10:03, действует до 12:03
2026-10-30 10:03  0387daea58  ждёт решения  demo  demo через Demo, операций: 2
```

## Релиз кончился

```
$ OCTOCHAN_ROLE=ci ochan trigger end release --id 1234 --reason "релиз 2.4.1 выкачен"
Окно release#1234 закрыто: роль deployer снята

$ OCTOCHAN_ROLE=deployer ochan plan deploy.yaml --limit group=prod
ошибка: роль deployer действует только в окне триггера (release): не активна — окно release#1234 закрыто: релиз 2.4.1 выкачен

$ ochan trigger
release          событие ci/release → роль deployer, group=prod, окно до 2h
  release#1234  2026-10-30 10:03  root, роль ci — закрыто: релиз 2.4.1 выкачен
```

Не пришло событие конца — окно закроется само через `max_window`, и
журнал аудита запишет «истекло … роль deployer снята»:

```
$ ochan audit
+ 2026-10-30 10:03:04  ci         root   trigger fire --id=1234 release
+ 2026-10-30 10:03:04  deployer   root   trigger: release#1234 → роль deployer выдана: group=prod, до 12:03
+ 2026-10-30 10:03:12  ci         root   trigger end --id=1234 --reason=релиз 2.4.1 выкачен release
+ 2026-10-30 10:03:12  deployer   root   trigger: release#1234 закрыто — роль deployer снята: релиз 2.4.1 выкачен
```

## Подводные камни

- То же событие второй раз окна не открывает и не продлевает: CI,
  повторивший шаг, не растянет срок роли.
- Область — селекторы инвентаря: цели, которых нет в инвентаре, в окне не
  пройдут. Это нарочно.
- Триггер не открывает `master` и `llm`; без `max_window` его нет.

## Смотрите также

- [Роли: по событию](../guide/roles.md#по-событию-триггеры)
- [Сервер: вебхуки](../guide/server.md#вебхуки)
