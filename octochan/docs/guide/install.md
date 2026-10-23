# Установка и настройка

## Сборка

Нужен Go 1.22 или новее. Внешних программ ochan не требует: ни Word для
документов, ни клиента PostgreSQL для живых источников.

```bash
git clone https://github.com/makisq/PACAL
cd PACAL/octochan
go build -o ochan .                      # сам ochan
(cd mcp && go build -o ../ochan-mcp .)   # сервер MCP для нейросетей — по желанию
./ochan --version
```

```
ochan 0.1.0-alpha
```

Собирается под Linux, macOS и Windows, в том числе накрест:

```bash
GOOS=windows GOARCH=amd64 go build -o ochan.exe .
GOOS=darwin  GOARCH=arm64 go build -o ochan-mac .
GOOS=linux   GOARCH=arm64 go build -o ochan-arm64 .
```

Версия задаётся при сборке релиза:

```bash
go build -ldflags "-X octochan/cmd.Version=0.1.0-alpha" -o ochan .
(cd mcp && go build -ldflags "-X main.serverVersion=0.1.0-alpha" -o ../ochan-mcp .)
```

Положите `ochan` (и `ochan-mcp`, если нужен) в каталог из `PATH`.

## Первый запуск

При первом запуске ochan создаёт `~/.octochan` и `config.yaml` со
значениями по умолчанию. Всё, что ochan хранит, живёт там:

```
~/.octochan/
  config.yaml     настройки
  .env            токен API (права 0600)
  roles.bin       роли, их права, логины и пароли (хеши), текущая роль
  state.json      что ochan помнит между запусками: история оболочки
  modules/        модули команд и языков (*.hcplugin), пакеты алиасов
  aliases/        пользовательские алиасы (*.ochan)
  scenarios/      сохранённые сценарии (ochan srs)
  history/        журнал операций — по нему работает rollback
  requests/       заявки на согласование
  reports/        отчёты команды diff
  snapshots/      снапшоты расхождений
  logs/           журнал работы ochan и его архивы
  tls/            сертификат и ключ серверного режима (ochan tls init)
```

Каталоги создаются с правами 0700, файлы с секретами — 0600. Путь до
конфига переопределяется флагом `--config` или переменной
`OCTOCHAN_CONFIG` — так удобно держать несколько окружений.

## Конфиг

`config.yaml` разбит на разделы; полный список ключей с умолчаниями — в
[справочнике настроек](../reference/config.md). Чаще всего правят:

```yaml
lang: ru                   # язык справки и сообщений: ru, eng (en) или язык модуля

defaults:
  api_url: https://rlm.example.internal/api/tasks.json   # исполнитель RLM
  max_parallel_tasks: 5    # одновременных итераций parallel for
  allow_unverified_modules: false

logs:
  max_size: 100MB          # журнал ochan уходит в архив с датой в имени
  min_level: warning       # ochan logs rank: что показывать по умолчанию

server:
  listen: 127.0.0.1:7443   # серверный режим
```

Адрес API не зашит в код: без `defaults.api_url` команды, которым нужен
исполнитель RLM, откажут с объяснением. Всё остальное работает без него.

## Секреты

Секреты в `config.yaml` не кладутся никогда: конфиг лежит на диске и
попадает в резервные копии.

| Что | Где |
|---|---|
| Токен API | `ochan auth` (ввод скрыт, хранится в `~/.octochan/.env`, 0600) или `OCTOCHAN_API_TOKEN` |
| Пароль почты | `OCTOCHAN_SMTP_PASSWORD` |
| Пароль PostgreSQL | `PGPASSWORD` или `~/.pgpass` — не в строке подключения |
| Пароли ролей | `ochan role … --npsw` спрашивает их; хранятся только хеши argon2id |
| Вход на сервер | спрашивается при подключении или `OCTOCHAN_REMOTE_LOGIN` / `OCTOCHAN_REMOTE_PASSWORD` |

Все переменные окружения — в [справочнике настроек](../reference/config.md#переменные-окружения).

## Командная строка, конвейеры и CI

Команды читают stdin, если вместо файла указан `-` или файл не указан вовсе:

```bash
cat prod.conf | ochan validate
ochan print prod.conf | ochan diff - test.conf
```

Полезный вывод идёт в stdout, предупреждения и ошибки — в stderr, поэтому
конвейеры не ломаются. Ненулевой код возврата означает ошибку: отказ в
правах, неверный аргумент, сбой проверки, `fail()` в сценарии.

Различие конфигураций ошибкой не считается: `ochan diff` нашёл различия —
он сделал свою работу и вернул 0. Чтобы CI падал на дрейфе, нужен сценарий
с `fail()` — см. рецепт [«Дрейф конфигурации в CI»](../cookbook/ci-drift.md).

## Интерактивная оболочка

`ochan` без аргументов на терминале открывает оболочку (`ochan shell` —
то же самое). Каждая строка — команда ochan, с автодополнением и историей:

```
ochan[master]> diff prod.conf test.conf
ochan[master]> !history
ochan[master]> exit
```

В приглашении видна текущая роль. История команд переживает перезапуск
(стрелка вверх, `!history`); пароли в ссылках записываются как `***`, а
строка, начатая с пробела, не запоминается вовсе. Модули, изменившиеся на
диске, оболочка перезагружает сама.

Команды `if` и `else` работают только в оболочке: результат предыдущего
`if` живёт в памяти процесса.

## Язык интерфейса

```yaml
lang: eng
```

Со следующего запуска справка и сообщения — по-английски. Другие языки
ставятся модулями; подробности — в разделе [«Язык интерфейса»](languages.md).
