# Общие строки: a @ (b | c)

**Задача.** К узлу node_n обращаются по API (POST) сценариями b и c. Ответ
на b несёт строку `string_i`, а c без неё получает 500 или 404. Узнать
строку иначе нельзя — ни зайти на узел, ни спросить у коллег. Нужно, чтобы
ochan сам взял её из ответа на b и подставил в c.

Строки ниже — настоящие: модуль [examples/classmodule](../../examples/classmodule/main.go)
изображает узел командой `node call <плейбук>`.

## Плейбуки

```yaml
# login.yaml — сценарий b: его ответ несёт string_i
service: node_login
parameters:
  user: ops
```

```yaml
# c.yaml — сценарий c: string_i неизвестна, на её месте ?
service: node_tune
parameters:
  work_mem: 64MB
  string_i: ?
```

Напрямую c не проходит:

```
$ ochan node call c.yaml
ошибка: HTTP 500: нет верного string_i (получено "?")
```

## Вызов

```
$ ochan shell
ochan[master]> node.call @ ("c.yaml" | "login.yaml")
[HTTP 200
string_i: k-42-session, HTTP 200: параметры применены]
```

Что произошло:

1. ochan увидел `?` в c.yaml: c ждёт `string_i`.
2. c отложен, выполнен `node call login.yaml` — ответ `string_i: k-42-session`.
3. Строка найдена — c выполнен с копией плейбука, где она стоит:

```yaml
# c.at.yaml — записал ochan; c.yaml не изменился
service: node_tune
parameters:
  work_mem: 64MB
  string_i: "k-42-session"
```

Порядок в скобке не важен: c ждёт строку, сколько бы вызовов ни стояло
между ним и тем, кто её даёт. Значение выражения — ответы по порядку
вызовов: `r = node.call @ (…)`, `len(r)`.

## Модуль знает сам

Команда `node call` объявила аннотацией `shared: "node_tune:string_i"`, что
у плейбуков сервиса `node_tune` строка `string_i` — общая. Тогда `?` писать
не нужно — ochan поставит его сам:

```yaml
# tune.yaml — string_i нет вовсе
# настройка узла
service: node_tune
parameters:
  work_mem: 32MB
```

```
ochan[master]> node.call @ ("tune.yaml" | "login.yaml")
[HTTP 200
string_i: k-42-session, HTTP 200: параметры применены]
```

```yaml
# tune.at.yaml
# настройка узла
service: node_tune
parameters:
  work_mem: 32MB
  string_i: "k-42-session"
```

Без того, кто строку даёт, — ошибка с её именем:

```
ochan[master]> node.call @ ("tune.yaml")
ошибка: <shell>:1:11: общая строка не найдена ни в одном ответе — вызовы ждут её: node call(tune.yaml): string_i
```

## Заявки: approve @ (…)

`@` — не только для плейбуков. Две заявки — согласовать первую, затем
вторую:

```
ochan[master]> approve @ ("0f88eebd40" | "97581deb28")
[Заявка 0f88eebd40: согласована (KONF\Admin, роль master)
Выполнить: ochan apply --request 0f88eebd40, Заявка 97581deb28: согласована (KONF\Admin, роль master)
Выполнить: ochan apply --request 97581deb28]
```

Вызовы идут строго по порядку: если заявки пересекаются по времени,
первая согласована раньше второй.

## Вложенность

`a @ (a @ (b | c) | d)` — по алгебре a²(b + c) + ad: ab, ac, ab, ac, ad.
Глубже — раньше, вызовов столько, сколько написано. Подробно —
[«Язык алиасов: общий множитель»](../guide/language.md#общий-множитель-).

## Что учесть

- Номера заявок и пути — в кавычках: это строки языка.
- Слева — алиас, функция или команда ochan; права — права команды у роли.
  `node call` здесь — класс `execution`: роли `llm` он недоступен.
- Копия `*.at.yaml` лежит рядом с плейбуком и перезаписывается при
  следующем вызове. В ней общая строка — если это секрет, удалите копию
  или держите плейбуки в закрытой папке.
- Команда модуля печатает ответ через `cmd.Println`: так он доходит до `@`
  ([«Модули»](../guide/modules.md#общие-строки-команды-shared)).
