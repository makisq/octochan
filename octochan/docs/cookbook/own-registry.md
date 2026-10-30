# Свой реестр пакетов с подписью

**Задача.** Команда держит пакеты алиасов в корпоративном git. Ставить их
нужно с проверкой, что пакет тот, который опубликовали, — по подписи, а не
по доверию к серверу, — и через согласование, как любое изменение.

## 1. Ключ и публикация

```
$ ochan '!!registry' keygen corp-release
Закрытый ключ: corp-release.key (права 0600) — им подписывают индекс, из реестра его не выкладывают
Открытый ключ: corp-release.pub
В config.yaml тех, кто ставит:
  registry:
    keys:
      corp-release: iv9ZPR/pj7WTx2QNmN/bS+rqPwAuqvrkSgbIRDQohPM=
```

```
# pgconf/package.yaml
version: 1.4.0
description: правила для postgresql.conf, словарь fp
modes: postgresql.conf, pgbouncer.ini
```

```
$ ochan '!!registry' publish ./pgconf --registry ./octochan-registry --key corp-release.key
Опубликован @pgconf v1.4.0: ./octochan-registry/pgconf-1.4.0.tar.gz, файлов 3, sha256 1c72…e3bd
Индекс подписан заново
$ cd octochan-registry && git add -A && git commit -m "@pgconf v1.4.0" && git push
```

Пакет перед публикацией проверяется так же, как перед установкой. Архив
воспроизводим: тот же пакет — та же сумма; опубликованную версию с другим
содержимым заменить нельзя.

## 2. Источник у тех, кто ставит

```yaml
registry:
  default: corp
  sources:
    corp:
      url: https://stash.corp.example/scm/~ops/octochan-registry
      signed_by: corp-release
  keys:
    corp-release: iv9ZPR/pj7WTx2QNmN/bS+rqPwAuqvrkSgbIRDQohPM=
```

Адрес с `/scm/` — git: ochan клонирует реестр в `~/.octochan/registry/cache`
и обновляет клон при каждом поиске (нужна программа `git`).

```
$ ochan '!!search' pgconf
Из corp:
  @pgconf v1.4.0   правила для postgresql.conf, словарь fp           (corp)
  @pgconf v1.3.0   отозван: CVE-2026-1234: store пишет мимо --root   (corp)
  @pgconf v1.2.1   то же, старее                                     (corp)
```

## 3. Установка — заявкой

```
$ ochan '!!rin' @pgconf
План установки:
  @pgconf v1.4.0
    источник:    corp (https://stash.corp.example/scm/~ops/octochan-registry)
    разместит:   /home/ops/.octochan/modules/pgconf/, файлов: 3
    режимы:      postgresql.conf, pgbouncer.ini
    зависимости: ничего
    подпись:     corp-release ✓ (индекс)
    sha256:      1c72…e3bd (сверяется при установке)
    протокол:    v1 (поддерживается)
Заявка 553abed9ba создана (ждёт решения): установка @pgconf v1.4.0
Согласовать: ochan approve 553abed9ba; поставить: ochan apply --request 553abed9ba

$ ochan approve 553a && ochan apply --request 553a
Установлен @pgconf v1.4.0 из corp: sha256 1c72…e3bd ✓, файлов 3
Пакет алиасов pgconf v1.4.0 установлен: алиасов 2 — ochan aliases pgconf
Словарь пакета: слов 1 — ochan fp words --skill pgconf
Алиасы: set_param, store
Заявка 553abed9ba выполнена: установка @pgconf v1.4.0
```

Ставится ровно согласованный архив: сумма — в заявке, и все архивы
проверяются до того, как что-то поставлено.

## 4. Подмена и отзыв

Индекс поправили, не подписав, — ему не верят:

```
$ ochan '!!search' pgconf
недоступен: источник corp: подпись индекса не сходится с ключом corp-release — реестру не верим
```

Архив подменили — не та сумма:

```
$ ochan '!!rin' @pgconf --approve
…
ошибка: источник corp: sha256 архива pgconf-1.4.0.tar.gz — 7c8b…26e2, а в индексе 1c72…e3bd: архив подменён или повреждён
```

Автор отозвал версию:

```
$ ochan '!!registry' revoke @pgconf v1.3.0 --registry ./octochan-registry --key corp-release.key \
    --reason "CVE-2026-1234: store пишет мимо --root"
@pgconf v1.3.0 отозван: CVE-2026-1234: store пишет мимо --root

$ ochan '!!rin' @pgconf v1.3.0
ошибка: @pgconf v1.3.0 отозван автором (CVE-2026-1234: store пишет мимо --root) — не ставится; доступна v1.4.0
```

Чего нет в настроенных источниках, того ochan не ищет:

```
$ ochan '!!rin' @pgconf v2.0.0
ошибка: пакет @pgconf v2.0.0 не найден в настроенных источниках (corp); других ochan не спрашивает — сеть вне них не использовалась
```

## Подводные камни

- Закрытый ключ — у того, кто публикует, и только у него: с ключом можно
  подписать что угодно. В реестр кладут только `.pub` — и то необязательно.
- Индекс без отметки времени: от отката на старую подписанную версию держит
  отзыв, а не подпись.
- Публикация без `--key` снимает прежнюю подпись — к новому индексу она не
  подходит; источнику с `signed_by` такой реестр не годится.

## Смотрите также

- [Реестр пакетов](../guide/modules.md#реестр-пакетов)
- [Свой пакет алиасов как скилл](skill-package.md)
