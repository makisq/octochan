# Своя модель без модели: проверить цепочку до LLaMA

**Задача.** Своя модель (LLaMA на fp) ещё не подключена, а контур надо
проверить заранее: доходит ли ответ модели через кольцо 0 и маску языка, что
делается с попыткой заговорить по-человечески и уходит ли формальное письмо
оператору. Для этого вместо модели берут демонстрационный движок — тот же
протокол v1, только без знаний.

Выводы ниже — настоящие. Почта уходила в локальный приёмник SMTP.

## 1. Движок

Демонстрационная модель ведёт к ответу из файла. В начале первой строки она
пробует сказать «Sure, here is what I found», в начале шестой (строки
считаются с нуля: `--chatter 5=…`) — «Готово, вот что я нашла». В этом она
уверена больше, чем в ответе.

```
# answer.fp — «знание» демонстрационной модели
goal: connection

classes:
  Connection : "connection"
  Limit      : "limit"

cell db7:
  set Connection.active = 200
  set Limit.max_connections = 200
  set Limit.reserved = 3
```

```
$ go build -o embedded-demo ./examples/embeddedengine
$ ./embedded-demo --answer answer.fp --listen 127.0.0.1:8090 \
    --chatter "0=Sure, here is what I found" --chatter "5=Готово, вот что я нашла"
Демонстрационный движок: http://127.0.0.1:8090, протокол v1; ответ — answer.fp; знаний у модели нет
```

## 2. ochan

```yaml
# ~/.octochan/config.yaml
llm:
  mode: embedded
  endpoint: http://127.0.0.1:8090
  model: ./models/llama-3.1-8b-q4.gguf
  rings:
    tau: 0.42
mail:
  operator: operators
  groups:
    operators: [ops@corp.example]
```

```
$ ochan llm
Режим:   embedded (кольцо 0 активно, τ=0.42: на позицию проходит не больше 2 токенов)
Движок:  http://127.0.0.1:8090 — протокол v1, ochan-demo, модель demo (answer.fp)
Модель:  ./models/llama-3.1-8b-q4.gguf (её загружает адаптер)
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
```

```
$ ochan llm generate --prompt task.fp
Утверждений: 8, токенов: 46, обращений к модели: 54
Кольцо 0:    отклонено 45 (токенов до порога τ=0.42)
Язык:        утверждений вне ℒ 1, токенов вне языка 1 — в ответе их нет
Откатов:     1 к началу утверждения; позиций без токена выше порога: 0
goal: connection

classes:
  Connection : "connection"
  Limit      : "limit"

cell db7:
  set Connection.active = 200
  set Limit.max_connections = 200
  set Limit.reserved = 3
```

- «Sure, here is what I found» модель собрала до конца строки. Строка не в
  языке, поэтому ochan откатился к её началу и запретил первый токен: в
  ответе этой фразы нет.
- «Готово» отвергнуто на первом токене: символ вне ASCII.
- Второй кандидат на каждом шаге был ниже порога — это 45 отклонённых
  кольцом 0.

```
$ ochan fp journal
Режим: embedded (кольцо 0 активно, τ=0.42: на позицию проходит не больше 2 токенов)
Отклонено на кольце 0: 45 (токенов до порога); генераций 1, с ответом 1
Отклонено языком:      2 (утверждений и токенов вне ℒ — в ответах их нет)
Отклонено на кольце 2: 0 (нижняя граница < well_True)
Отклонено на кольце 3: 0 (противоречие)
```

## 4. Письмо оператору и агенту

Ответ — в stdout, сводка — в stderr, поэтому ответ можно сразу отправить:

```
$ ochan llm generate --prompt task.fp 2>/dev/null | ochan mail send --to operators -
Письмо 1cb7d81c8d (формальный промпт)
  почтой: ops@corp.example — ушло
```

```
Subject: [ochan] goal: connection — llm, агент db7
To: ops@corp.example

goal: connection

classes:
  Connection : "connection"
  Limit      : "limit"

cell db7:
  set Connection.active = 200
  set Limit.max_connections = 200
  set Limit.reserved = 3
```

От имени модели (`OCTOCHAN_ROLE=llm`, агент db7) естественный язык не
проходит никуда:

```
$ ochan mail send --to operators "всё починила"
ошибка: письмо не на языке: своя модель (llm.mode: embedded) пишет только формальным промптом
  1:1: ⊥syn: символ «в» вне ASCII: язык — замкнутое подмножество английского, модель пишет только его
  1:1: ⊥syn: такой последовательности нет в языке; раздел начинается с одного из: goal:, classes:, relations:, priority:, generalization, instance, requires:, import, hard forbid|require, ??n_|??a_, cell, report:
  1:1: ⊥syn: программа начинается с заголовка goal: слово

$ ochan mail send --to db8 answer.fp
Письмо 399aadf010 (формальный промпт)
  в ящики: db8
```

## Подводные камни

- Демонстрационный движок — не модель: «знание» у него — файл ответа. Он
  проверяет декодер, протокол и настройки, а не то, как модель решает
  задачи.
- Адресов модель не выбирает: только агенты и группы `mail.groups`.
- `llm.mode: embedded` вместе с `fp.mode: natural` — отказ: своя модель
  знает только формальный промпт.

## Смотрите также

- [Своя модель: режим embedded](../guide/embedded.md) — протокол движка v1 для адаптера.
- [Почта: агенты и люди](../guide/mail.md)
