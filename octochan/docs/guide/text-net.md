# Текст и сеть

То, ради чего на сервере держат `grep`, `awk`, `tail`, `diff`, `nc`, `dig`,
`curl` и `openssl`, — внутри ochan: на закрытой машине их может не быть, а
права, область роли и журнал аудита у ochan свои. Все команды только
читают и вызываются и из сценария — `ochan("text grep …")`,
[«Команды ochan»](language.md#команды-ochan).

## Текст

```
ochan text grep -i 'error|fatal' /var/log/postgresql/*.log
ochan text grep -c timeout app.log
ochan logs rank --min warning | ochan text grep -n replica
ochan text fields -f 1,4 access.log
ochan text fields -d : -f 1,7 /etc/passwd
ochan text fields --match ERROR -f 2- app.log
ochan text tail -n 50 /var/log/postgresql/postgresql.log
ochan text tail -f app.log
```

- `grep` — шаблон RE2: без возвратов, поэтому время поиска не взрывается
  на любом шаблоне. `-i`, `-v`, `-n`, `-c`, `-F` (строка, а не выражение),
  `-m` (не больше строк). Файлов несколько — перед строкой имя файла;
  шаблон файлов (`*.log`) раскрывает сам ochan. Совпадений нет — пустой
  вывод, а не ошибка: в сценарии это пустая строка.
- `fields` — поля строк, как `awk '{print $1}'` и `cut`: делит пробелами и
  табуляциями или `-d`; `-f 1,3-4`, `2-` (до конца); `--match` — только
  подходящие строки; `-s` — разделитель в выводе.
- `tail` читает файл с конца — большой журнал целиком не читается. `-f`
  печатает дописанное, пока не прервут; усечённый или повёрнутый файл
  читает сначала. Из сценария `-f` нельзя: сценарий не кончился бы.
- Без файла — stdin из конвейера; в сценарии — отказ: его ввод — не
  терминал.
- Право — `read`. Пути проверяются, как у любой команды: роли с областью —
  только в ней, рабочий каталог ochan — никому, кроме master. Шаблон `grep`
  путём не считается: `ochan text grep /var/lib app.log` у роли с областью
  работает.

## Сравнение

Сравнение у ochan одно — `ochan diff`. Конфиги и живые источники — по
смыслу ([«Конфигурации»](configs.md)): `8GB` и `8192MB` равны, порядок
параметров неважен. Всё остальное — построчно, в формате `diff -u`:

```
$ ochan diff app.log app.log.1
app.log и app.log.1 — не конфиг: сравниваю построчно (по смыслу — --config)
--- app.log
+++ app.log.1
@@ -1,3 +1,3 @@
 2026-11-01 ERROR conn refused
-2026-11-01 INFO ok
+2026-11-01 WARN slow
 2026-11-01 INFO done
```

Конфигом считается текст, где почти каждая значимая строка — `[секция]`
или «имя = значение» (`include` в `postgresql.conf` — тоже). Раньше разбор
конфига пропускал строки журнала, и два разных журнала выходили
одинаковыми. `--lines` сравнивает построчно и конфиги, `--config` — по
смыслу любой текст; `-U` — строк контекста.

## Сеть

```
ochan net port db7:5432 db8:5432 pgbouncer:6432 --timeout 1s
ochan net dns db7.example.com
ochan net dns example.com --type MX
ochan net dns 10.0.0.7 --type PTR --server 10.0.0.2:53
ochan net http https://grafana.example.com/api/health
ochan net http http://db7:8008/primary --expect 200
ochan net http https://vault.internal/v1/sys/health --ca corp-ca.pem --body
ochan net tls grafana.example.com
ochan net tls db7:5433 --name db7.internal --ca corp-ca.pem --warn 14d
```

```
$ ochan net port db7:5432 db8:5432
db7:5432                     открыт   3 мс
db8:5432                     закрыт   connection refused
ошибка: не отвечает 1 из 2: db8:5432

$ ochan net tls grafana.example.com
кому:      grafana.example.com
выдал:     R11
действует: с 2026-09-30 до 2026-12-29 (осталось 57 д.)
имена:     grafana.example.com
TLS:       TLS 1.3
```

- Недоступное — ошибка, и после вывода по всем целям: в сценарии её
  ловит `try`, в расписании — код возврата.
- `http`: код 400 и выше — ошибка, `--expect` — ровно этот код; `--body`
  печатает тело (не больше 1 МиБ), `-H` — заголовок.
- `tls`: порт по умолчанию — 443. Сертификат проверяется по системным
  корням и `--ca` (свой центр сертификации). Не прошёл проверку — сведения
  о нём печатаются всё равно, а команда — ошибка. Истёк или осталось
  меньше `--warn` (по умолчанию `30d`) — ошибка.
- Проверку сертификата ochan не выключает никогда: флага «не проверять» нет.
- `--timeout` — сколько ждать ответа (по умолчанию 5 с).

Право — отдельная группа `net`, а не `read`: проверять сеть с машины ochan —
возможность, которую выдают явно. Нейросети, которой подсунули вредную
инструкцию, незачем сканировать внутренние порты, поэтому у `llm` после
установки `net` нет; при инцидентах master выдаёт его сам —
`ochan role grant llm net`.

Из сценария сетевые команды — читающие: `ochan("net port db7:5432")`
выполняется и в плане.

```
try {
    ochan("net port " + host + ":5432 --timeout 1s")
} catch e {
    ochan("incident", "open", "--level", "high", "--host", host, "--summary", "PostgreSQL на " + host + " не отвечает")
}
```
