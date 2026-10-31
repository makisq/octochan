# Лама в ФПм-вид

Своя модель ochan (`llm.mode: embedded`) знает только формальный промпт.
Здесь — то, что превращает обычную модель архитектуры Llama в такую:
адаптер к протоколу движка v1, обрезка выходного словаря до алфавита ℒ и
замер каждого шага. План целиком — [docs/plans/llama-fpm.md](../docs/plans/llama-fpm.md).

| Файл | Что делает |
|---|---|
| `adapter.py` | сервер протокола движка v1: распределение следующего токена до выборки, кеш KV по общему префиксу; `raw` — сырой вывод без колец |
| `fpm.py` | модель в ФПм-виде: базовая модель и выходной слой, обрезанный до алфавита (`fpm.json`, `lm_head.safetensors`) |
| `prune.py` | шаг 2: обрезка выходного словаря до алфавита ℒ (`ochan fp alphabet`) |
| `measure.py` | замер: сырой вывод против встроенного режима ochan по набору задач |
| `tasks/` | задачи эксплуатации на ℒ; каждая проходит `ochan fp check` |

## Окружение

Модели, venv и кеши — не на системном диске:

```
py -3.12 -m venv G:\fpm-llm\venv
set PIP_CACHE_DIR=G:\fpm-llm\cache\pip
set HF_HOME=G:\fpm-llm\cache\hf
G:\fpm-llm\venv\Scripts\python -m pip install torch --index-url https://download.pytorch.org/whl/cu128
G:\fpm-llm\venv\Scripts\python -m pip install transformers safetensors accelerate peft huggingface_hub
```

Проверено: Windows 10, RTX 5070 12 ГБ (sm_120), torch 2.11.0+cu128,
transformers 5.22.1, peft 0.21.2. Для RTX 50xx нужна сборка CUDA 12.8 и
новее. Llama 3.2 у Meta — по лицензии: её принимает аккаунт Hugging Face
владельца, токен — переменная `HF_TOKEN`.

## Запуск

```
ochan fp alphabet > alphabet.tsv
python prune.py --model HuggingFaceTB/SmolLM2-135M-Instruct --alphabet alphabet.tsv --corpus "tasks/*.fp" --out G:/fpm-llm/models/smollm2-135m-fpm
python adapter.py serve --model G:/fpm-llm/models/smollm2-135m-fpm --listen 127.0.0.1:8091
```

```yaml
# config.yaml ochan
llm:
  mode: embedded
  engine: transformers
  endpoint: http://127.0.0.1:8091
  rings:
    tau: 0.42
```

```
ochan llm generate --prompt tasks/pg_memory.fp
python measure.py --model G:/fpm-llm/models/smollm2-135m-fpm --ochan ochan --endpoint http://127.0.0.1:8091 --home G:/fpm-llm/runs/home --out run.jsonl "tasks/*.fp"
```

`measure.py` запускает ochan в отдельном доме: журнал и роли пользователя
замер не трогает.

## Замеры

Модель для отладки — SmolLM2-135M-Instruct (архитектура Llama, Apache 2.0),
пока нет доступа к Llama 3.2 1B. Набор — 8 задач из `tasks/`. Числа — из
прогонов `measure.py` 31.10.2026.

**Шаг 1. Базовая модель через адаптер.**

| τ | raw в ℒ | embedded с ответом | откатов | утверждений вне ℒ | отклонено кольцом 0 | позиций без токена |
|---|---|---|---|---|---|---|
| 0,42 | 0/8 | 0/8 | 0 | 0 | 15 | 8 |
| 0,1 | 0/8 | 0/8 | 49 | 8 | 19 186 | 48 |

Сырой вывод — английская проза («The language does not have a `Backup`
class…»), а на задаче с русским комментарием модель повторяет его по-русски.
Во встроенном режиме при τ = 0,42 ответа нет ни разу: уже первый токен
утверждения не дотягивает до порога (самый вероятный — «To», p ≈ 0,17).
При τ = 0,1 кольца работают — откаты, утверждения вне ℒ, снятые позиции, —
но ни одно утверждение языка модель не собирает.

**Шаг 2. Словарь вывода = алфавит ℒ.** `ochan fp alphabet` — 425 записей
(слова, смыслы-классы, служебные слова, ключевые слова, единицы, знаки).
Вместе с их сочетаниями из корпуса, буквами и цифрами: словарь вывода
49 152 → 1 250 токенов, выходной слой 28,3 → 0,72 млн параметров, токенов
вне ℒ на выходе — 0. Входной словарь целый: модель читает протокол и задачу
как есть.

| τ | raw в ℒ | embedded с ответом | откатов | утверждений вне ℒ | отклонено кольцом 0 | позиций без токена |
|---|---|---|---|---|---|---|
| 0,42 | 0/8 | 0/8 | 0 | 0 | 16 | 8 |
| 0,1 | 0/8 | 0/8 | 14 | 9 | 55 254 | 10 |

Обрезка убирает чужие символы и языки — кириллицы, эмодзи, «Sure!» у модели
больше нет, — но не английский синтаксис. Сырой вывод по-прежнему проза:
«A user can set the default value of the "Backup" relation…» — она
складывается из слов самого алфавита (a, the, of, set, value, relation).
Словарь ℒ — подмножество английского, поэтому язык держат грамматика в
декодере ochan и обучение (шаг 3), а не словарь.
