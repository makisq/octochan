# Своя модель без модели: проверить цепочку до весов

**Задача.** Веса своей модели ℒ ещё не обучены, а контур надо проверить
заранее: доходит ли ответ модели через кольцо 0 и маску языка, что делается
с попыткой заговорить по-человечески и уходит ли формальное письмо
оператору. Для этого вместо модели берут демонстрационный движок — тот же
протокол v1, только без знаний.

Выводы ниже — настоящие. Почта уходила в локальный приёмник SMTP.

## 1. Движок

Демонстрационная модель ведёт к ответу из файла. Копию задачи в ответе
пишет ochan, поэтому файл начинается с неё — в канонической записи, с
одиночными пробелами. Модель пишет только новую ячейку `c2`. В начале
строки 13 (строки считаются с нуля: `--chatter 13=…`) она пробует сказать
«Sure, here is what I found», в начале строки 14 — «Готово, вот что я
нашла». В этом она уверена больше, чем в ответе.

```
# answer.fp — «знание» демонстрационной модели
goal: connection

classes:
  Connection : "connection"
  Limit : "limit"
  Node : "host"

relations:
  Limit --limits--> Connection (connection_cap)

cell c1:
  set Limit.max_connections = 200

cell c2:
  set Connection.max_connections = 200
```

```
$ go build -o embedded-demo ./examples/embeddedengine
$ ./embedded-demo --answer answer.fp --listen 127.0.0.1:8090 \
    --chatter "13=Sure, here is what I found" --chatter "14=Готово, вот что я нашла"
Демонстрационный движок: http://127.0.0.1:8090, протокол v1; ответ — answer.fp; знаний у модели нет
```

## 2. ochan

Демонстрационный движок отдаёт только распределение следующего токена, и
его ведёт декодер ochan. `derive: false` — решения пишет модель по токену:
иначе вывод из посылок написала бы логика ochan, и модели нечего было бы
сказать.

```yaml
# ~/.octochan/config.yaml
llm:
  mode: embedded
  endpoint: http://127.0.0.1:8090
  decoder:
    tau: 0.42
    derive: false
mail:
  operator: operators
  groups:
    operators: [ops@corp.example]
defaults:
  smtp_host: 127.0.0.1
  smtp_port: 2525
  smtp_from: ochan@corp.example
```

```
$ ochan llm
Режим:   embedded (своя модель — логика и кольца в её коде; чужой движок ведёт декодер ochan: кольцо 0, τ=0.42 — на позицию не больше 2 токенов)
Движок:  http://127.0.0.1:8090 — протокол v1, ochan-demo, модель demo (answer.fp)
Подход:  протокольный — чужой движок ведёт декодер ochan: кольцо 0, τ=0.42; выводимое из посылок пишет модель (llm.decoder.derive: false)
Язык:    только формальный промпт: в ответе модели — утверждения ℒ, ничего на естественном языке
```

## 3. Ответ модели

Задача оператора — формальный промпт:

```
# task.fp
goal: connection

classes:
  Connection : "connection"
  Limit      : "limit"
  Node       : "host"

relations:
  Limit --limits--> Connection (connection_cap)

cell c1:
  set Limit.max_connections = 200
```

```
$ ochan llm generate --prompt task.fp
Утверждений: 2, токенов: 12, обращений к модели: 20
Кольцо 0:    отклонено 11 (токенов до порога τ=0.42)
Язык:        утверждений вне ℒ 1, токенов вне языка 1 — в ответе их нет
Откатов:     1 к началу утверждения; позиций без токена выше порога: 0
goal: connection

classes:
  Connection : "connection"
  Limit : "limit"
  Node : "host"

relations:
  Limit --limits--> Connection (connection_cap)

cell c1:
  set Limit.max_connections = 200

cell c2:
  set Connection.max_connections = 200
```

- «Sure, here is what I found» модель собрала до конца строки. Строка не в
  языке, поэтому ochan откатился к её началу и запретил первый токен: в
  ответе этой фразы нет.
- «Готово» отвергнуто на первом токене: символ вне ASCII.
- Второй кандидат на каждом шаге был ниже порога — это 11 отклонённых
  кольцом 0.
- Копию задачи модель не писала: её пишет ochan.

```
$ ochan fp journal
Режим: embedded (своя модель — логика и кольца в её коде; чужой движок ведёт декодер ochan: кольцо 0, τ=0.42 — на позицию не больше 2 токенов)
Отклонено на кольце 0: 11 (токенов до порога); генераций 1, с ответом 1
Отклонено языком:      2 (утверждений и токенов вне ℒ — в ответах их нет)
Отклонено на кольце 2: 0 (нижняя граница < well_True)
Отклонено на кольце 3: 0 (противоречие)
```

## 4. Письмо оператору и агенту

Дальше — от имени модели: `OCTOCHAN_ROLE=llm`, агент db7. Ответ — в
stdout, сводка — в stderr, поэтому ответ можно сразу отправить. В ответе
есть имя `max_connections`: оно не из словаря W, и модель может его писать,
только отвечая на задачу, где оно есть, — `--prompt task.fp`:

```
$ ochan llm generate --prompt task.fp 2>/dev/null | ochan mail send --prompt task.fp --to operators -
Письмо fda5a14d75 (формальный промпт)
  почтой: ops@corp.example — ушло
```

```
From: ochan@corp.example
To: ops@corp.example
Subject: [ochan] goal: connection — llm, агент db7

goal: connection

classes:
  Connection : "connection"
  Limit : "limit"
  Node : "host"

relations:
  Limit --limits--> Connection (connection_cap)

cell c1:
  set Limit.max_connections = 200

cell c2:
  set Connection.max_connections = 200
```

Естественный язык не проходит никуда, открытое имя без задачи — тоже:

```
$ ochan mail send --to operators "всё починила"
ошибка: письмо не на языке: своя модель (llm.mode: embedded) пишет только формальным промптом
  1:1: ⊥syn: символ «в» вне ASCII: язык — замкнутое подмножество английского, модель пишет только его
  1:1: ⊥syn: такой последовательности нет в языке; раздел начинается с одного из: goal:, classes:, relations:, priority:, generalization, instance, requires:, import, hard forbid|require, ??n_|??a_, cell, report:
  1:1: ⊥syn: программа начинается с заголовка goal: слово

$ ochan mail send --to db8 answer.fp
ошибка: письмо не на языке: агентам пишут только формальным промптом
  12:13: ⊥syn: имя «max_connections» не из задачи и не из словаря объектов ochan, а слов языка в нём нет: модель имён не придумывает — открытым именем она говорила бы мимо языка

$ ochan mail send --to db8 --prompt task.fp answer.fp
Письмо cf31aa0f91 (формальный промпт)
  в ящики: db8
```

## Подводные камни

- Демонстрационный движок — не модель: «знание» у него — файл ответа. Он
  проверяет декодер, протокол и настройки, а не то, как модель решает
  задачи. Свою модель ℒ проверяют так же, но с адаптером и весами
  (`ochan llm serve`, [«Своя модель»](../guide/embedded.md#запуск-адаптер-своей-модели)).
- Файл ответа начинается с копии задачи в канонической записи: декодер
  пишет копию сам, и демонстрационная модель продолжает с её конца.
- Адресов модель не выбирает: только агенты и группы `mail.groups`.
- `llm.mode: embedded` вместе с `fp.mode: natural` — отказ: своя модель
  знает только формальный промпт.

## Смотрите также

- [Своя модель: режим embedded](../guide/embedded.md) — протокол движка v1, адаптер и цикл агента.
- [Почта: агенты и люди](../guide/mail.md)
