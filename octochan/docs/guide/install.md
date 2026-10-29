# Установка и настройка

## Сборка

Нужен Go 1.22 или новее. Внешних программ ochan не требует: ни Word для
документов, ни клиента PostgreSQL для живых источников.

```bash
git clone https://github.com/makisq/PACAL
cd PACAL/octochan
make install
ochan --version
```

```
ochan 0.2.0-alpha
```

`make install` собирает `ochan` и сервер MCP `ochan-mcp` и кладёт их в
`~/.local/bin` — дальше они вызываются по имени из любого каталога, без
`./` и пути. Если этого каталога ещё нет в `PATH`, установщик скажет, какую
строку добавить в профиль оболочки; сам профиль он не правит.

| | |
|---|---|
| `make install` | в `~/.local/bin` |
| `make install PREFIX=/usr/local` | в `/usr/local/bin`; копирование — через `sudo`, сборка — от вашего имени |
| `scripts/install.sh` | то же без `make` |
| `make uninstall` | убрать |
| `make build` | только собрать в `bin/` каталога репозитория |
| `make build OUT=$HOME/tools` | собрать в любой каталог, куда можно писать |
| `make vendor`, `make offline` | зависимости в `vendor/` и сборка из них без сети |
| `make dist` | релиз под все платформы в `dist/` с `SHA256SUMS` |
| `make version` | какую версию соберёт `make` |

На Windows:

```powershell
powershell -ExecutionPolicy Bypass -File scripts\install.ps1
```

Программы ставятся в `%LOCALAPPDATA%\Programs\ochan`, каталог
добавляется в `PATH` пользователя; в новом окне терминала `ochan` и
`ochan-mcp` вызываются по имени. Убрать — с `-Uninstall`.

### go build — без make

Собрать руками, без установки, — в любой каталог:

```bash
go build -o ochan .
(cd mcp && go build -o ../ochan-mcp .)
```

Собирается под Linux, macOS и Windows, в том числе накрест:

```bash
GOOS=windows GOARCH=amd64 go build -o ochan.exe .
GOOS=darwin  GOARCH=arm64 go build -o ochan-mac .
GOOS=linux   GOARCH=arm64 go build -o ochan-arm64 .
```

Версию печатают `ochan --version` и `ochan-mcp --version`; в исходниках
записана `0.2.0-alpha`, другая задаётся при сборке:

```bash
go build -ldflags "-X octochan/cmd.Version=0.2.0-alpha" -o ochan .
(cd mcp && go build -ldflags "-X main.serverVersion=0.2.0-alpha" -o ../ochan-mcp .)
make build VERSION=0.2.0-alpha+site     # то же через make
```

Путь системной политики ролей тоже задаётся при сборке, а не окружением
([«Безопасность»](security.md#роль-которую-подтверждает-не-рабочий-каталог)):

```bash
make build SYSTEM_ROLES=/opt/octochan/roles.yaml
go build -ldflags "-X octochan/rbac.SystemPolicyPath=/opt/octochan/roles.yaml" -o ochan .
```

По умолчанию — `/etc/octochan/roles.yaml`; файла нет — политики нет.

### Закрытые системы

Там, где класть программы в `/bin`, `/usr/bin` и `/usr/local/bin` нельзя,
а сети нет, ochan собирается и работает целиком в каталогах пользователя:
ни системных путей, ни прав root ему не нужно.

1. **Без системных каталогов.** `make build OUT=$HOME/tools` или
   `go build -o $HOME/tools/ochan .` — в любой каталог, куда можно писать.
   Запуск — по полному пути (`$HOME/tools/ochan --version`) или через свой
   каталог в `PATH` (`export PATH="$HOME/tools:$PATH"` в профиле). `make
   install PREFIX=$HOME/opt` кладёт в `$HOME/opt/bin` и `sudo` не просит,
   если каталог ваш.
2. **Без сети.** На машине с сетью: `make vendor` — зависимости обоих
   модулей (ochan и ochan-mcp) ложатся в `vendor/`. Каталог `octochan`
   целиком, вместе с `vendor/`, переносится на закрытую машину, и там
   `make offline` собирает из него, не обращаясь к сети (`GOFLAGS=-mod=vendor
   GOPROXY=off`). Без make — то же руками:

   ```bash
   GOFLAGS=-mod=vendor GOPROXY=off go build -o $HOME/tools/ochan .
   (cd mcp && GOFLAGS=-mod=vendor GOPROXY=off go build -o $HOME/tools/ochan-mcp .)
   ```

3. **Без записи в домашний каталог Go.** Кеш сборки и модулей Go по
   умолчанию живёт в `~/.cache/go-build` и `~/go`; если туда нельзя или
   `/tmp` смонтирован с `noexec`, их переносят в свой каталог:

   ```bash
   export GOCACHE=$HOME/work/go-cache GOMODCACHE=$HOME/work/go-mod GOTMPDIR=$HOME/work/go-tmp
   export GOTOOLCHAIN=local      # не скачивать другую версию Go
   mkdir -p "$GOTMPDIR"
   ```

4. **Только готовый файл.** Собрать на другой машине (`make dist` или
   `GOOS=linux GOARCH=amd64 go build …`) и перенести один исполняемый файл:
   зависимостей во время работы у ochan нет. Суммы — в `dist/SHA256SUMS`.

Рабочий каталог ochan — `$HOME/.octochan`. Если домашний каталог на
закрытой машине не годится, ochan запускают с другим `HOME`
(`HOME=/srv/ochan-home ochan …`); путь только к конфигу меняют `--config`
и `OCTOCHAN_CONFIG`.

## Первый запуск

При первом запуске ochan создаёт `~/.octochan` и `config.yaml` со
значениями по умолчанию. Всё, что ochan хранит, живёт там:

```
~/.octochan/
  config.yaml     настройки
  tokens/         токены исполнителей, файл на исполнителя (права 0600)
  roles.bin       роли, их права, логины и пароли (хеши), текущая роль
  state.json      что ochan помнит между запусками: история оболочки
  modules/        модули команд и языков (*.hcplugin), пакеты алиасов
  aliases/        пользовательские алиасы (*.ochan)
  scenarios/      сохранённые сценарии (ochan srs)
  history/        журнал операций — по нему работает rollback
  requests/       заявки на согласование
  decisions/      журнал решений по формальному промпту — для колец
  audit.log       журнал аудита с цепочкой хешей
  inventory.yaml  инвентарь — источник local, если файл есть
  inventory.seen.json  хосты, которые уже видели (ochan inventory ack, watch)
  policies/       политики ochan watch; отозванные и заменённые — в old/
  reports/        отчёты команды diff
  snapshots/      снапшоты расхождений
  logs/           журнал работы ochan и его архивы
  tls/            сертификат и ключ серверного режима (ochan tls init)
  ha.json, ha.token  пара серверов: роль узла, эпоха, общий секрет (ochan ha init)
  agents.json     реестр агентов на сервере
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
  max_parallel_tasks: 5    # одновременных итераций parallel for
  allow_unverified_modules: false

executors:
  rlm:                     # модуль RLM, если он стоит
    api_url: https://rlm.example.internal/api/tasks.json

logs:
  max_size: 100MB          # журнал ochan уходит в архив с датой в имени
  min_level: warning       # ochan logs rank: что показывать по умолчанию

server:
  listen: 127.0.0.1:7443   # серверный режим
```

Адрес API не зашит в код: без него исполнитель модуля откажет с
объяснением. Всё остальное работает без него. Модуль RLM ставит `make rlm`
(на Windows — `scripts\install.ps1 -Rlm`), см. [«Модуль RLM»](scenarios.md#модуль-rlm).

## Секреты

Секреты в `config.yaml` не кладутся никогда: конфиг лежит на диске и
попадает в резервные копии.

| Что | Где |
|---|---|
| Токен исполнителя | `ochan auth --executor <имя>` (ввод скрыт, `~/.octochan/tokens/<имя>`, 0600) или `OCTOCHAN_TOKEN_<ИМЯ>` |
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
