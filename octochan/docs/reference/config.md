# Настройки

`~/.octochan/config.yaml` (другой путь — `--config` или `OCTOCHAN_CONFIG`).
При первом запуске ochan создаёт его со значениями по умолчанию. Секретов
в нём нет и быть не должно — см. [переменные окружения](#переменные-окружения).

## Общее

| Ключ | По умолчанию | |
|---|---|---|
| `lang` | `ru` | язык справки и сообщений: `ru`, `eng` (`en`) или язык модуля; см. [«Язык интерфейса»](../guide/languages.md) |

## defaults

| Ключ | По умолчанию | |
|---|---|---|
| `api_url` | `""` | адрес API исполнителя RLM; без него сценарии RLM не планируются и не выполняются |
| `http_timeout` | `30s` | предел одного запроса к API |
| `auto_check_status` | `false` | `true` — `apply` всегда ждёт завершения задач, как с `--wait` |
| `token_timer_enabled` | `false` | напоминать в оболочке о смене токена API (`ochan timer on\|off`); только для токена, сохранённого `ochan auth` |
| `token_lifetime` | `20m` | время жизни токена для напоминания, от его сохранения |
| `max_parallel_tasks` | `5` | одновременных итераций `parallel for` |
| `scenario_path` | `~/.octochan/scenarios` | куда `ochan srs` сохраняет сценарии |
| `log_path` | `~/.octochan/logs` | каталог журнала ochan |
| `module_command_timeout` | `10m` | предел времени команды модуля |
| `plugin_log_level` | `warn` | уровень журнала внутри процессов модулей |
| `allow_unverified_modules` | `false` | загружать модули без контрольной суммы — только для разработки |
| `smtp_host` | `""` | сервер почты; пусто — почта не настроена, и это не ошибка |
| `smtp_port` | `25` | |
| `smtp_from` | `""` | отправитель |
| `smtp_to` | `[]` | получатели |
| `smtp_user` | `""` | логин; пароль — `OCTOCHAN_SMTP_PASSWORD` |
| `smtp_starttls` | `false` | `true` — без шифрования письмо не уйдёт |
| `notify_always` | `false` | `true` — слать уведомления без флага `--notify` |

## logs

| Ключ | По умолчанию | |
|---|---|---|
| `max_size` | `100MB` | размер, при котором журнал ochan уходит в архив; `512KB`, `1GB`, число без единицы — мегабайты |
| `prefix` | `%Y-%m-%d_%H-%M-%S_` | префикс архива: `%Y %m %d %H %M %S` и `%%`; разделителей пути быть не может |
| `keep` | `10` | сколько архивов хранить; `0` — все |
| `levels` | `[debug, info, notice, warning, error, fatal, panic]` | уровни `ochan logs rank`, от наименее важного |
| `min_level` | `warning` | уровень по умолчанию для `--min` |
| `default_level` | `info` | уровень строки, которой не подошло ни одно правило |
| `rules` | метки PANIC … DEBUG | правила: `level` и `match` (регулярное выражение RE2), первое совпадение решает; группа `(?P<msg>…)` — суть сообщения |

Прежние `defaults.log_max_size` и `defaults.log_max_backups` не читаются.
Подробнее — [«Журналы»](../guide/logs.md).

## shell

| Ключ | По умолчанию | |
|---|---|---|
| `history_size` | `1000` | сколько команд оболочки помнить между запусками (`state.json`) |

## fp — формальный промпт

| Ключ | По умолчанию | |
|---|---|---|
| `max_meanings` | `3` | k: сколько классов может означать слово |
| `consistency` | `strict` | `strict` (λ1) — ссылка вперёд в ячейках — отказ; `reconcile` (λ0) — замечание |
| `quiet` | `[]` | Q_off: вопросы отчёта, о которых не сообщать |

## Серверный режим

| Ключ | По умолчанию | |
|---|---|---|
| `ssl` | `true` | шифрование соединения; без него — только эта машина (или `--allow-plaintext`) |
| `tls.min_version` | `1.2` | `1.2` или `1.3` |
| `tls.cert` | `~/.octochan/tls/server.crt` | сертификат сервера (`ochan tls init`) |
| `tls.key` | `~/.octochan/tls/server.key` | ключ сервера |
| `tls.ca` | `""` | клиент: чем проверять сервер; пусто — системные корневые сертификаты |
| `tls.server_name` | `""` | клиент: имя в сертификате, если не совпадает с адресом |
| `server.listen` | `127.0.0.1:7443` | адрес сервера |
| `server.dir` | `""` | каталог, над файлами которого работают команды; пусто — где запущен `serve` |
| `server.session_timeout` | `30m` | сессия без запросов |
| `server.command_timeout` | `10m` | одна команда |

Подробнее — [«Серверный режим»](../guide/server.md).

## sandbox — песочница ОС

| Ключ | По умолчанию | |
|---|---|---|
| `strict` | `false` | `true` — не выполнять сценарий с `--sandbox`, если ядро не даёт полной изоляции (`ochan sandbox` покажет, чего нет) |

Подробнее — [«Безопасность»](../guide/security.md).

## Переменные окружения

| Переменная | |
|---|---|
| `OCTOCHAN_CONFIG` | путь до `config.yaml` |
| `OCTOCHAN_API_TOKEN` | токен API исполнителя RLM — вместо `ochan auth` |
| `OCTOCHAN_SMTP_PASSWORD` | пароль почты |
| `OCTOCHAN_ROLE` | роль для этого запуска — только роль без логина и пароля |
| `OCTOCHAN_REMOTE_LOGIN`, `OCTOCHAN_REMOTE_PASSWORD` | вход на сервер без вопросов (`connect`, `remote`) |
| `PGPASSWORD`, `~/.pgpass` | пароль живого PostgreSQL — не в строке подключения |

Служебные, их задаёт сам ochan: `OCTOCHAN_ROLE_GRANT` — допуск сервера
для дочернего процесса; `OCTOCHAN_REMOTE_ADDR` — адрес клиента сервера или
ochan-mcp для журнала аудита; `OCTOCHAN_SANDBOX_STAGE` — шаг процесса
песочницы; `OCTOCHAN_PLUGIN` и `OCTOCHAN_PLUGIN_LOG_LEVEL` — рукопожатие и
журнал модулей.

## Сервер MCP

`ochan-mcp` настраивается флагами (`--root`, `--allow-live`, `--allow-env`,
`--ochan`, `--timeout`, `--max-output`; для HTTP — `--listen`, `--path`,
`--token-file`, `--tls-cert`, `--tls-key`, `--allow-plaintext`,
`--allow-origin`, `--session-idle`), а `ochan` под ним — этим же конфигом.
Токен HTTP можно задать окружением `OCHAN_MCP_TOKEN`. См.
[mcp/README.md](../../mcp/README.md).
