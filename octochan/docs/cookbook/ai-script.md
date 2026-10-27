# Нейросеть пишет сценарий по формальному промпту

**Задача.** Поручить модели поднять лимит соединений на `prod.conf` так,
чтобы она не могла записать ничего другого, а результат прошёл через
согласование человеком.

## 1. Подключить ochan к модели

```bash
(cd octochan/mcp && go build -o ochan-mcp .)    # рядом с ochan
```

```json
{
  "mcpServers": {
    "octochan": {
      "command": "/usr/local/bin/ochan-mcp",
      "args": ["--root", "/srv/configs"]
    }
  }
}
```

Модель видит файлы только внутри `/srv/configs`, работает в роли `llm`
(читать, планировать, оформлять заявки) и при подключении первым получает
протокол формального промпта.

## 2. Скилл

Модель пишет «как» из операций скилла — пакета алиасов. Возьмём готовый
`pgconf` из примеров:

```
$ cp -r octochan/examples/fp/pgconf ~/.octochan/modules/
$ ochan aliases pgconf
Пакет pgconf 1.2.0 — правка параметров конфигурации PostgreSQL
  pgconf.set_param(c, name, value)             задать параметр конфига
  pgconf.store(c, path)                        записать конфиг в файл
```

## 3. Формальный промпт

```
# task.fp — поднять лимит соединений на prod, ничего другого не записывая
goal: tuned

classes:
  State  : "tuned"
  Config : "config"

relations:
  Config --strives_to--> State

import @pgconf v1
  provides: Config, set_param, store
  requires_class: Config

hard forbid store(except = {prod_conf})
hard forbid write()
```

```
$ ochan fp check task.fp
task.fp: P ∈ ℒ, слова однозначны, Req(P) ⊆ K, жёсткие правила совместны
Цель γ:  State
K:       set_param, store
```

Два жёстких правила: `store` пишет только в `prod.conf`, а `write` не
пишет никуда. Их проверяет не модель, а ochan — по плану сценария.

Слова промпта — из словаря языка: `tuned` и `config` есть в базовом
словаре, слова области приносит словарь скилла (`lexicon.yaml` пакета).
Отчёт модели тоже пишется словами словаря — `high`, `low` — или именами,
которые уже есть в промпте; список — `ochan fp words` или инструмент
`vocabulary`.

## 4. Что делает модель

Получив промпт, модель по протоколу:

1. вызывает `check_prompt` — промпт проходит;
2. пишет сценарий и вызывает `check_script`, затем `plan_script` с
   `script`, `prompt` и своим `report`:

   ```
   c = config("prod.conf")
   pgconf.set_param(c, "max_connections", "200")
   pgconf.store(c, "prod.conf")
   ```

   ```
   report:
     q: limit
       choice: high
       alternatives: {low, high}
       basis: not_in_prompt
   ```

3. видит план и проверку правил:

   ```
     /srv/configs/prod.conf
       изменить   max_connections: 100 -> 200

   Отчёт модели:
     limit: high из {low, high} — not_in_prompt

   Жёсткие правила промпта:
     соблюдено  hard forbid store(except = {prod_conf})
     соблюдено  hard forbid write()
   ```

4. вызывает `request_script` — заявка «ждёт решения» с промптом, отчётом и
   соблюдёнными правилами — и возвращает оператору её номер.

То же самое руками — `ochan plan raise.ochan --prompt task.fp --report report.fp`.

## 5. Если модель отступит от правил

Допишем в сценарий `write("note.txt", "x")`:

```
Жёсткие правила промпта:
  соблюдено  hard forbid store(except = {prod_conf})
  НАРУШЕНО   hard forbid write()
             write в /srv/configs/note.txt (note_txt)
ошибка: план нарушает жёсткие правила промпта: hard forbid write()
```

Заявка на такой план не создаётся. Модель получает ошибку с перечнем и
исправляет сценарий — правило она изменить не может.

## 6. Решает человек

```
ochan requests <id>               карточка: сценарий, план, промпт, правила, отчёт модели
ochan requests <id> --doc z.docx  то же документом
ochan approve <id>
ochan apply --request <id>        промпт и правила проверяются ещё раз
```

Выполнить заявку модель не может: `apply` нет ни в таблице инструментов
MCP, ни в правах роли `llm`.

## Смотрите также

- [Нейросети и формальный промпт](../guide/ai.md)
- [Протокол](../../fp/protocol.md) — что модель читает первым.
- [Свой пакет алиасов как скилл](skill-package.md).
