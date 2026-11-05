# Своя модель ℒ: обучение весов

Своя модель ochan (`llm.mode: embedded`) знает только формальный промпт.
Это модель ℒ с нуля: веса и словарь только ℒ, логика и кольца — в её
коде. Всё, кроме обучения весов, — в ochan, на Go, одной реализацией:

| Что | Где |
|---|---|
| словарь модели (алфавит ℒ, переменные для частных имён) | `ochan llm init`, пакет `own` |
| корпус: задачи, ответы логики ochan, токены | `ochan llm corpus` |
| сама модель — сеть, логика (`fp`), кольца (`rings`) — в процессе ochan | `llm.model`, `ochan llm generate`, `ochan llm run` |
| параметры колец по журналу решений и по инцидентам | `ochan llm fit`, `ochan llm learn` |
| пакеты знаний для других машин | `ochan llm learn --package` |
| квантование int8/int4 | `ochan llm quantize` |
| модель для ochan на другой машине | `ochan llm serve` |
| стенд эксперимента 9.2 | `go run ./experiment` |

Здесь — только `train_own.py`: градиентный спуск по токенам корпуса на
видеокарте, который считает torch. Логики и колец в Python нет: каждую
пару корпуса вывела и проверила логика ochan, а `train_own.py` видит лишь
номера токенов. Как модель работает в ochan —
[«Своя модель»](../docs/guide/embedded.md); план и история —
[docs/plans/llama-fpm.md](../docs/plans/llama-fpm.md), прежние замеры —
[docs/plans/llama-measurements.md](../docs/plans/llama-measurements.md).

## От словаря до ответа

```
ochan llm init --out ~/models/own --size small
ochan llm corpus --model ~/models/own --count 60000 --out corpus.jsonl
python llama/train_own.py --model ~/models/own --corpus corpus.jsonl   # без видеокарты: --device cpu
```

```yaml
# ~/.octochan/config.yaml
llm:
  mode: embedded
  model: ~/models/own
```

```
ochan llm                                 # модель, подход, знания
ochan llm generate --prompt task.fp       # ответ модели на задачу
ochan llm run --agent db7                 # цикл агента: письма и инциденты → заметки, заявки, ответы
```

`ochan llm init --random-weights` пишет случайные веса: модель отвечает
сразу — ответ пишет её логика, сеть только выбирает порядок среди
допустимых. Это проверка контура до обучения, а не обученная модель.

## Окружение

```
python -m venv venv
venv/bin/python -m pip install torch --index-url https://download.pytorch.org/whl/cu128   # видеокарта NVIDIA
venv/bin/python -m pip install -r llama/requirements.txt
```

Модели, venv и кеши — не на системном диске (так у владельца, Windows):

```
py -3.12 -m venv G:\fpm-llm\venv
set PIP_CACHE_DIR=G:\fpm-llm\cache\pip
G:\fpm-llm\venv\Scripts\python -m pip install torch --index-url https://download.pytorch.org/whl/cu128
G:\fpm-llm\venv\Scripts\python -m pip install -r llama\requirements.txt
```

Проверено: Windows 10, RTX 5070 12 ГБ (sm_120), torch 2.11.0+cu128,
transformers 5.22.1. Для RTX 50xx нужна сборка CUDA 12.8 и новее. Без
видеокарты обучение идёт на процессоре (`--device cpu`; по умолчанию —
видеокарта, если она есть) — медленно, но работает: тест обучения на
крошечной модели проходит в облачном контейнере без видеокарты.

## Обучение

`train_own.py` читает каталог модели — словарь `fpmtok.json` и конфигурацию
`config.json` от `ochan llm init` — и корпус, у каждой пары которого есть
токены (`ids`, `start`: `ochan llm corpus --model`). Потери — по всей паре
`<bos> задача <answer> ответ <eos>`: модель учит и язык задач, и ответы.
Батч собирается по числу токенов (`--batch-tokens`), пары близкой длины —
вместе. Веса пишутся в тот же каталог (`model.safetensors`): это и есть
`llm.model` для ochan.

Каждые `--save-every` шагов каталог — рабочая модель и состояние обучения;
`--resume` продолжает с последнего сохранения. Под Windows драйвер иногда
сбрасывает видеокарту (событие `nvlddmkm` 153, TDR), процесс теряет CUDA:
с `--resume` повторный запуск продолжает с последнего сохранения.

**Дообучение** — на парах кластеров инцидентов с примесью базового
корпуса (`ochan llm learn --pairs pairs.jsonl --replay corpus.jsonl`):

```
python llama/train_own.py --init ~/models/own --model ~/models/own-2 --corpus pairs.jsonl --epochs 1 --lr 1e-4
```

Новые веса — в новый каталог (`--model`), прежний остаётся для отката;
словарь, параметры колец (`rings.json`) и их версии переходят с весами.
Квантованную модель не дообучают — только полные веса.

## Проверка

```
make llm-test          # cd llama && python -m unittest discover -s tests -v
```

Тест учит крошечную модель на процессоре по корпусу в формате ochan и
проверяет, что потери падают, веса читаются, корпус без токенов и
дообучение в тот же каталог — отказ. Всё, что модель делает при ответе, —
тесты Go: `go test ./own` (словарь, сеть, кольца, генерация, корпус),
`go test ./cmd` (команды `ochan llm`).

## tasks/

Задачи эксплуатации на ℒ для ручной проверки модели; каждая проходит
`ochan fp check`: `ochan llm generate --prompt llama/tasks/pg_memory.fp`.
