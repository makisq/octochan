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
| `cmd`: `TestApplyRequestUsesApprovedPlan`, `rlm`: `TestExecuteScenarioIsBoundToPlan` | исполнитель применяет ровно согласованный план; запрос сверх плана — отказ до первой отправки |
| `workspace`: `TestGraph` | порядок между заявками: «ожидает», «не выполнится», без циклов |
| `cmd`: `TestWatchPolicy` | без человека выполняется только то, что укладывается в политику; отзыв действует со следующего прохода |
| `cmd`: `TestSystemRolePolicy`, `rbac`: `TestSystemFileCheck` | хранилище ролей сужает права, но не поднимает выше потолка; политике, которую может править не root, не верят |
| `sandbox`: `TestKernelEnforcesPolicy`, `TestExecOnlyAllowedPrograms` | ядро держит файлы, системные вызовы и запуск программ, а не только язык |
| `cmd`: `TestServerPair`, `remote`: `TestHAReplicationAndFailover` | два процесса `serve`: копия, отказ записи на standby, переключение, заявка не выполняется дважды, бывший primary отступает |
| `cmd`: `TestPlanChecksGeneralizationSlots`, `TestReportBoundToPlan` | значения модели — из слотов обобщений; отчёт модели совпадает с планом |
| `rings`: `TestPlanSupport`, `cmd`: `TestWeakPlanNeedsOperator`, `workspace`: `TestFailureAttribution` | поддержка плана по различным правилам; слабый план — только с оператором; ошибки модели не снижают надёжность скиллов |

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
make dist                                        # dist/: ochan и ochan-mcp под все платформы и SHA256SUMS
make dist VERSION=0.2.1-alpha                    # своя версия
make dist SYSTEM_ROLES=/opt/octochan/roles.yaml  # свой путь системной политики ролей
```

Платформы — linux/amd64, linux/arm64, darwin/amd64, darwin/arm64,
windows/amd64. Версия и путь системной политики зашиваются флагами
компоновщика (`-X octochan/cmd.Version`, `-X octochan/rbac.SystemPolicyPath`);
собирая руками через `go build`, их передают так же — иначе путь политики
будет по умолчанию, `/etc/octochan/roles.yaml`
([«Установка»](install.md#go-build--без-make)).

Что вошло в версию — [CHANGELOG](../../CHANGELOG.md).

## Правила кода

- Комментарии и сообщения — по-русски; текст для модели (протокол,
  описания инструментов MCP) — по-английски.
- Ошибка объясняет, что не так и что сделать, — как «нужно право apply»,
  а не «access denied».
- Ложная тревога хуже пропуска: проверка отвергает только то, что неверно
  наверняка.
- То, что согласовали, и то, что выполнится, сверяется, а не предполагается.
