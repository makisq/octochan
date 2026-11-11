# Своя модель ℒ: обучение на видеокарте

Своя модель ochan (`llm.mode: embedded`) знает только формальный промпт.
Это модель ℒ с нуля: веса и словарь только ℒ, логика и кольца — в её
коде. Всё у неё — в ochan, на Go, одной реализацией:

| Что | Где |
|---|---|
| словарь модели (алфавит ℒ, переменные для частных имён) | `ochan llm init`, пакет `own` |
| стартовые веса — обученная модель из бинарника | `ochan llm init --pretrained` |
| корпус: задачи, ответы логики ochan, токены | `ochan llm corpus` |
| обучение и дообучение весов сети, на процессоре | `ochan llm train` |
| сама модель — сеть, логика (`fp`), кольца (`rings`) — в процессе ochan | `llm.model`, `ochan llm generate`, `ochan llm run` |
| параметры колец по журналу решений и по инцидентам | `ochan llm fit`, `ochan llm learn` |
| пакеты знаний для других машин | `ochan llm learn --package` |
| квантование int8/int4 | `ochan llm quantize` |
| модель для ochan на другой машине | `ochan llm serve` |
| стенд эксперимента 9.2 | `go run ./experiment` |

Python для своей модели не нужен: обученные веса доставляются вместе с
ochan ([own/pretrained](../own/pretrained/README.md)), а обучает и
дообучает сеть `ochan llm train` — на процессоре.

Здесь — `train_own.py`: тот же градиентный спуск, который torch считает
на видеокарте. Он нужен там, где процессора мало, — обучить модель с нуля.
На одной машине (Ryzen 5 7500F, RTX 5070), модель `small`, 14,8 млн
параметров:

| | токенов в секунду |
|---|---|
| `ochan llm train`, процессор, 6 ядер | 480 |
| `train_own.py --device cpu` | 1 000 |
| `train_own.py`, видеокарта | 90 000 |

Дообучение на сотне пар — полторы минуты на эпоху и на процессоре. Модель
`small` с нуля (65 тысяч пар, 8 эпох — 160 млн токенов) — около четырёх
суток на процессоре и полчаса на видеокарте.

Счёт у обоих один: потери по всей паре, батч по числу токенов, AdamW,
обрезка нормы градиента, то же расписание скорости. Каталог модели и
корпус общие, веса (`model.safetensors`) читают оба. Градиент каждого
тензора и шаги AdamW на Go сверены с `train_own.py` в тестах (`go test
./own`, эталон — `own/testdata/train/make.py`); дообучение стартовых весов
на 109 парах дало у обоих одни потери до третьего знака, веса разошлись в
среднем на 10⁻⁹.

Логики и колец в Python нет: каждую пару корпуса вывела и проверила логика
ochan, а `train_own.py` видит лишь номера токенов. Как модель работает в
ochan — [«Своя модель»](../docs/guide/embedded.md); план и история —
[docs/plans/llama-fpm.md](../docs/plans/llama-fpm.md), прежние замеры —
[docs/plans/llama-measurements.md](../docs/plans/llama-measurements.md).

## От словаря до ответа

```
ochan llm init --out ~/models/own --size small
ochan llm corpus --model ~/models/own --count 60000 --out corpus.jsonl
python llama/train_own.py --model ~/models/own --corpus corpus.jsonl   # на процессоре: ochan llm train --model ~/models/own --corpus corpus.jsonl
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
видеокарты `train_own.py` считает на процессоре (`--device cpu`; по
умолчанию — видеокарта, если она есть): тест обучения на крошечной модели
проходит в облачном контейнере без видеокарты.

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
Состояние обучения у `train_own.py` и у `ochan llm train` своё: начатое
одним другой не продолжает.

**Дообучение** — на парах кластеров инцидентов с примесью базового
корпуса (`ochan llm learn --pairs pairs.jsonl --replay corpus.jsonl`):

```
python llama/train_own.py --init ~/models/own --model ~/models/own-2 --corpus pairs.jsonl --epochs 1 --lr 1e-4
```

Новые веса — в новый каталог (`--model`), прежний остаётся для отката;
словарь, параметры колец (`rings.json`) и их версии переходят с весами.
`train_own.py` дообучает только полные веса: стартовую модель для него
распаковывает `ochan llm init --pretrained --full`. `ochan llm train
--init` берёт и квантованные — распаковывает сам.

## Проверка

```
make llm-test          # cd llama && python -m unittest discover -s tests -v
```

Тест учит крошечную модель на процессоре по корпусу в формате ochan и
проверяет, что потери падают, веса читаются, корпус без токенов и
дообучение в тот же каталог — отказ. Всё, что модель делает при ответе, и
обучение на Go — тесты Go: `go test ./own` (словарь, сеть, кольца,
генерация, корпус, обучение), `go test ./cmd` (команды `ochan llm`).

## tasks/

Задачи эксплуатации на ℒ для ручной проверки модели; каждая проходит
`ochan fp check`: `ochan llm generate --prompt llama/tasks/pg_memory.fp`.
