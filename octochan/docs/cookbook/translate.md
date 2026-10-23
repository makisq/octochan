# ochan на своём языке

**Задача.** Команда говорит по-немецки (японски, китайски — неважно).
Нужен ochan, который показывает справку на этом языке, без пересборки
самого ochan.

## Английский — встроен

```yaml
# ~/.octochan/config.yaml
lang: eng
```

Со следующего запуска справка всех команд, оболочка и общие сообщения —
по-английски.

## Любой другой — модулем

Язык приносит модуль: Go-плагин, который вместо команд отдаёт словарь.
Возьмём пример — [examples/langmodule](../../examples/langmodule).

### 1. Шаблон перевода

```
$ ochan lang template de > de.json
```

```json
{
  "Доступные команды:": "",
  "Отклонить заявку": "",
  "Показать журнал операций": "",
  "Согласовать заявку": "",
  …
}
```

Ключ — русский текст справки или сообщения, значение — перевод. Пустое
значение — перевода нет. Переводить всё сразу не обязательно: чего нет в
словаре, берётся из английского, а чего нет и там — остаётся по-русски.

### 2. Словарь — в модуль

Заполненный `de.json` кладётся на место `examples/langmodule/de.json`. Сам
модуль — двадцать строк:

```go
//go:embed de.json
var dictionary []byte

type language struct{ entries map[string]string }

func (language) GetCommands() []*cobra.Command { return nil }
func (language) Init(map[string]any) error     { return nil }
func (language) Name() string                  { return "lang-de" }
func (language) Version() string               { return "0.1.0" }

func (l language) Translations() []pluginhost.Language {
    return []pluginhost.Language{{Code: "de", Name: "Deutsch", Entries: l.entries}}
}
```

Для японского — `Code: "jp"`, `Name: "日本語"`, `Name()` модуля —
`lang-jp`.

### 3. Сборка и установка

```
$ go build -o lang-de.hcplugin ./examples/langmodule
$ ochan '!install' lang-de.hcplugin
Модуль lang-de.hcplugin установлен и загружен
```

### 4. Включить

Выйти из ochan, в `config.yaml` написать `lang: de`, запустить снова:

```
$ ochan lang
* de   Deutsch      module lang-de       help translated: 8 of 159
  en   English      built-in             help translated: 159 of 159
  ru   Русский      built-in             help translated: 159 of 159

* — current language (lang in config.yaml)

$ ochan help
Verfügbare Befehle:
------------------
!install                 Install a command module
…
approve                  Antrag genehmigen
```

В примере переведено 8 строк из 159 — остальные пришли из английского.
`ochan lang` показывает, сколько справки переведено на каждый язык.

Модуль остаётся установленным, роль и история оболочки — прежними:
перезапуск их не трогает.

## Что важно знать

- Имя команды не переводится никогда — `ochan approve` остаётся
  `approve` на любом языке.
- Встроенный английский модуль переписать не может.
- Модуль языка — исполняемый файл, поэтому ставится только через
  `!install`, с проверкой контрольной суммы.
- Протокол формального промпта и описания инструментов MCP остаются
  английскими при любом `lang`: их читает модель.
- Если в `lang` указан язык, модуль которого не установлен, ochan
  запустится по-русски и скажет на двух языках, чего не хватает.

## Смотрите также

- [Язык интерфейса](../guide/languages.md)
