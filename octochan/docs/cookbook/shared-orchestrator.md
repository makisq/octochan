# Общий оркестратор

**Задача.** Один ochan на сервере `orchestrator.example` с ролями и
паролями; команда подключается к нему по сети, каждый — в своей роли, и
работает над файлами сервера.

## На сервере

```
$ ochan tls init --host orchestrator.example --host 10.0.0.5
Сертификат: /home/octochan/.octochan/tls/server.crt
Ключ:       /home/octochan/.octochan/tls/server.key (права 0600)
Имена:      localhost, 127.0.0.1, ::1, orchestrator.example, 10.0.0.5
Отпечаток:  21:47:56:A7:02:63:0A:EF:…
Клиентам: скопировать сертификат и указать его в tls.ca их config.yaml

$ ochan role create Dan --allow read,plan --nlogin dan --npsw
$ ochan role update master --npsw          # обязательно: иначе в master войдёт любой

$ cd /srv/octochan && ochan serve --listen 0.0.0.0:7443
Сервер слушает https://0.0.0.0:7443
Каталог:   /srv/octochan
Сертификат: 21:47:56:A7:02:63:0A:EF:…
```

Команды клиентов выполняются над файлами каталога, где запущен `serve`
(или `server.dir`). Как служба — см. [systemd](../guide/server.md#запуск-как-служба).

## На клиенте

Сертификат сервера передают любым надёжным путём и сверяют отпечаток.
Клиенту он указывается как `tls.ca`:

```yaml
# ~/.octochan/config.yaml клиента
tls:
  ca: /home/dan/.octochan/orchestrator.crt
```

Сессия:

```
$ ochan connect orchestrator.example:7443 --role Dan
Логин: dan
Пароль: (ввод скрыт)
Подключено к orchestrator.example:7443, роль Dan
ochan@orchestrator.example:7443[Dan]> diff prod.conf test.conf
ochan@orchestrator.example:7443[Dan]> exit
```

Одна команда — например, из скрипта:

```
$ export OCTOCHAN_REMOTE_LOGIN=dan OCTOCHAN_REMOTE_PASSWORD=…
$ ochan remote --role Dan orchestrator.example:7443 diff --fast prod.conf test.conf
Подключено к orchestrator.example:7443, роль Dan
Найдены различия (2):
  max_connections: 100 -> 200 [Modified]
  log_min_duration_statement: — -> 1s [OnlyInRight]

$ ochan remote --role Dan orchestrator.example:7443 patch prod.conf test.conf
Подключено к orchestrator.example:7443, роль Dan
ошибка: роль Dan не может выполнять patch: нужно право apply
```

Свой файл клиента серверу передаётся через stdin:

```
cat новый.conf | ochan remote --role Dan orchestrator.example:7443 validate -
```

## Почему это граница между людьми

- Роли и хеши паролей лежат у сервера; у клиента хранилища нет, обойти
  вход ему нечем.
- Без `ssl` сервер слушает только эту машину — пароли не идут по сети
  открытым текстом. Проверку сертификата на клиенте отключить нельзя.
- Пять неудачных входов подряд блокируют пару «адрес, роль» на минуту;
  несуществующая роль отвечает так же, как роль с паролем.
- Сменили пароль роли — её сессии закрываются.
- Права `server` и `remote` не выдаются роли `llm`.
- Роль клиента не выходит за каталог сервера, а рабочий каталог ochan с
  ключом подписи допусков закрыт; что ещё держит сервер, если ролью
  завладел чужой, — рецепт [«Общий сервер против чужой роли»](hardened-server.md).

## Смотрите также

- [Серверный режим](../guide/server.md) — настройки, защита входа.
- [Роли для команды и CI](team-roles.md).
