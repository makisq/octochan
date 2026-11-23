# Отладка сценария

**Задача.** Сценарий падает, а ошибка указывает только строку. Нужно
понять, что было в переменных в этот момент, через какие алиасы прошёл
вызов и что вернул каждый шаг. При этом в вывод не должны попасть токены.

## Сценарий с ошибкой

```
alias limit_for(conf) {
    var max = int(conf["max_connections"])
    return max * 2
}
var api_token = env("API_TOKEN", "")
var prod = config("prod.conf")
var want = limit_for(prod)
print("лимит:", want)
var test = config("test.conf")
var have = int(test["max_conections"])
```

```
$ ochan run sync.ochan
лимит: 200
ошибка: sync.ochan:10:20: параметр "max_conections" не найден в test.conf
```

## --trace

```
$ ochan run --trace sync.ochan
trace sync.ochan:5:1: var api_token = …
  → api_token = …
trace sync.ochan:6:1: var prod = config("prod.conf")
  → prod = конфиг prod.conf (1 параметров)
trace sync.ochan:7:1: var want = limit_for(prod)
→ вызов limit_for(конфиг prod.conf (1 параметров))
  trace sync.ochan:2:5: var max = int(conf["max_connections"])
    → max = 100
  trace sync.ochan:3:5: return max * 2
    → return 200
  → want = 200
trace sync.ochan:8:1: print("лимит:", want)
лимит: 200
trace sync.ochan:9:1: var test = config("test.conf")
  → test = конфиг test.conf (1 параметров)
trace sync.ochan:10:1: var have = int(test["max_conections"])
  ✗ sync.ochan:10:20: параметр "max_conections" не найден в test.conf
    переменные: api_token = …, prod = конфиг prod.conf (1 параметров), test = конфиг test.conf (1 параметров), want = 200
ошибка: sync.ochan:10:20: параметр "max_conections" не найден в test.conf
```

Как читать трассировку:

- **`trace файл:строка:столбец: текст`** — инструкция перед выполнением;
- **`→`** — её итог: новое значение переменной, значение выражения,
  `return`;
- **`→ вызов алиас(аргументы)`** — вход в алиас; его инструкции идут со
  сдвигом;
- **`✗`** — ошибка. Под ней один раз выводятся переменные в этот момент,
  а внутри алиаса ещё и цепочка вызовов (`вызовы: a → b`).

Здесь видно, что `test` загрузился, а ключа `max_conections` в нём нет.
Это опечатка, правильно — `max_connections`.

## Секреты

Значение переменной или параметра с именем вроде `token`, `password`,
`secret`, `key`, `cookie`, `auth` выводится как `…`. Присваивание такой
переменной показано без значения и в тексте строки (`var api_token = …`):
токен, вписанный прямо в сценарий, в трассировку не попадёт. Литерал
внутри вызова (`check(1, "pw")`) в тексте строки виден, а значение
параметра `password` — нет. Не вписывайте секреты в сценарий, берите их из
`env()`.

## Подводные камни

- **Трассировка идёт в stderr**, вывод сценария — в stdout. Их можно
  развести: `ochan run --trace s.ochan 2> trace.log`.
- **`--trace` не работает вместе с `--sandbox`.** Сценарий в песочнице
  выполняется отдельным процессом, отлаживайте его без неё.
- **Длинное значение** обрезается до 120 символов, переводы строк
  выводятся как `\n`.
- **`print` не повторяется** в трассировке: его вывод и так виден рядом.
- **`ochan plan` и `ochan check`** трассировку не печатают. План
  показывает изменения, а проверка ловит ошибки до запуска, например
  неизвестную функцию или неверный тип.

## Смотрите также

- [Язык алиасов: отладка](../guide/language.md#отладка---trace)
- [Проверка до запуска](../guide/language.md#проверка-до-запуска)
