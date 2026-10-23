# Разработка

## Тесты

```
go test ./... ./mcp/...
go test -race ./... ./mcp/...
go vet ./... ./mcp/...
gofmt -l .
```

`mcp/` — отдельный Go-модуль, и шаблон `./...` в него не заходит: его
нужно назвать явно. `go.work` позволяет сделать это из корня одной
командой.

Тесты, которые держат инварианты, а не отдельные функции:

| Тест | Что держит |
|---|---|
| `lang`: `TestCheckFindingsAreRuntimeErrors`, `TestCheckAcceptsWorkingPrograms` | каждая находка `ochan check` — настоящая ошибка выполнения, а рабочие сценарии проверку проходят |
| `lang`: `TestPlanMatchesRealRun`, `TestPrintPlanMatchesRun` | план сценария совпадает с выполнением, в том числе `parallel for` и печать в файл |
| `cmd`: `TestEveryCommandHasPermission` | у каждой команды есть право |
| `cmd`: `TestEnglishCoversHelp`, `TestEnglishCoversMessages` | у каждого текста справки и сообщения через `i18n` есть английский перевод |
| `cmd`: `TestReference` | справочник команд и функций совпадает с кодом |
| `mcp`: `TestTableOnlyReadsOrRequests`, `TestToolDescriptionsAreEnglish` | модели недоступно выполнение; описания инструментов — по-английски |
| `pluginhost`: `TestLanguageModuleOverGRPC` | модуль языка собирается, ставится и отдаёт словарь по gRPC |

## Справочник

`docs/reference/commands.md` и `docs/reference/builtins.md` собираются из
кода — из дерева команд и набора встроенных функций. После правки команды,
флага или функции:

```
go test ./cmd -run TestReference -update
```

Без `-update` тест падает, если справочник разошёлся с кодом.

## Перевод

Новый текст справки или сообщение через `i18n.T`, `i18n.Sprintf`,
`i18n.Errorf` требует английского перевода в `i18n/en.go` — иначе тесты
упадут и назовут строку. Шаблон всех текстов для перевода —
`ochan lang template`.

## gRPC модулей

Код из `pluginhost/command.proto` генерируется так, чтобы повторная
генерация давала тот же файл байт в байт. Версии генераторов закреплены:

```
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.1
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
# protoc 30.1: https://github.com/protocolbuffers/protobuf/releases/tag/v30.1
cd pluginhost
protoc -I.. --go_out=.. --go_opt=paths=source_relative \
       --go-grpc_out=.. --go-grpc_opt=paths=source_relative pluginhost/command.proto
```

Новый метод сервиса не ломает старые модули, если хост понимает ответ
`Unimplemented` как «не поддерживается» — так сделан `GetTranslations`.

## Сборка релиза

```bash
VERSION=0.1.0-alpha
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  os=${target%/*}; arch=${target#*/}; ext=""; [ "$os" = windows ] && ext=.exe
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X octochan/cmd.Version=$VERSION" \
    -o dist/ochan-$VERSION-$os-$arch$ext .
  (cd mcp && GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.serverVersion=$VERSION" \
    -o ../dist/ochan-mcp-$VERSION-$os-$arch$ext .)
done
(cd dist && sha256sum * > SHA256SUMS)
```

Что вошло в версию — [CHANGELOG](../../CHANGELOG.md).

## Правила кода

- Комментарии и сообщения — по-русски; текст для модели (протокол,
  описания инструментов MCP) — по-английски.
- Ошибка объясняет, что не так и что сделать, — как «нужно право apply»,
  а не «access denied».
- Ложная тревога хуже пропуска: проверка отвергает только то, что неверно
  наверняка.
- То, что согласовали, и то, что выполнится, сверяется, а не предполагается.
