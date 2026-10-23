# Дрейф конфигурации в CI

**Задача.** Конфиги серверов хранятся в репозитории рядом с эталоном.
CI должен падать, если конфиг разошёлся с эталоном, и проверять сценарии и
формальные промпты до слияния.

## Почему не просто `ochan diff`

`diff` нашёл различия — он сделал свою работу и вернул 0: различие — не
ошибка. Падение на дрейфе — решение, и его пишут сценарием с `fail()`.

## Сценарий

```
# ci/drift.ochan — test.conf должен совпадать с эталоном prod.conf
эталон = config("prod.conf")
цель = config("test.conf")
var n = diff(эталон, цель)
if n != 0 {
    print(stderr(), "дрейф:", diff_list(эталон, цель))
    fail("расхождений:", n)
}
print("дрейфа нет")
```

```
$ ochan check ci/drift.ochan
ci/drift.ochan: ошибок не найдено

$ ochan run ci/drift.ochan; echo "код $?"
дрейф: [max_connections, log_min_duration_statement]
ошибка: ci/drift.ochan:7:9: расхождений: 2
код 1
```

Сравнение — по смыслу: `8GB` и `8192MB` не дрейф. Вместо файла можно
сравнивать с живым сервером — `config("postgres://ci@db1:5432/postgres")`,
пароль в `PGPASSWORD`.

## Шаг CI

```yaml
# .github/workflows/configs.yml
name: configs
on: [pull_request]
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.22' }
      - name: ochan
        run: |
          git clone --depth 1 https://github.com/makisq/PACAL /tmp/pacal
          (cd /tmp/pacal/octochan && go build -o /usr/local/bin/ochan .)
      - name: конфиги разбираются
        run: for f in configs/*.conf; do ochan validate "$f"; done
      - name: сценарии и промпты без ошибок
        run: |
          ochan check scripts/*.ochan
          for p in prompts/*.fp; do ochan fp check "$p"; done
      - name: дрейф
        run: ochan run ci/drift.ochan
```

Первый запуск ochan в CI сам создаёт `~/.octochan` и конфиг. Каждая
команда возвращает ненулевой код при ошибке, поэтому шаг падает сам.

## Если нужно ограничить CI

```
ochan role create ci --allow read      # один раз, на машине с ochan
OCTOCHAN_ROLE=ci ochan run ci/drift.ochan
```

`run` требует права `apply`: сценарий умеет писать файлы. Для CI, которому
писать нельзя, вместо `run` — `plan` с тем же сценарием: `fail()` прерывает
и план, а записей не будет даже по ошибке:

```
OCTOCHAN_ROLE=ci ochan plan ci/drift.ochan     # роль: read,plan
```

## Смотрите также

- [Конфигурации](../guide/configs.md) — сравнение по смыслу, живой PostgreSQL.
- [Роли и права](../guide/roles.md) — `OCTOCHAN_ROLE`.
