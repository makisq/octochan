# Модель в облаке и конфиги на своей машине

**Задача.** Модель работает у поставщика — Claude API, OpenAI, ChatGPT,
Gemini, — а конфигурации лежат на вашей машине. Нужно, чтобы модель
читала и сравнивала их и предлагала правки заявками, но ничего не меняла
сама и не дотянулась ни до чего, кроме каталога с конфигами.

## Сервер

На машине с конфигурациями:

```
$ ochan-mcp --root /srv/configs --listen 127.0.0.1:8765
ochan-mcp: 2026/10/23 13:33:04 запущен: ochan /usr/local/bin/ochan, каталоги /srv/configs
ochan-mcp: 2026/10/23 13:33:04 создан токен доступа: /home/dba/.config/ochan-mcp/token
ochan-mcp: 2026/10/23 13:33:04 слушаю http://127.0.0.1:8765/mcp
```

Токен — 256 случайных бит, файл с правами 0600; сам токен в журнал не
пишется. Проверка:

```
$ TOKEN=$(cat ~/.config/ochan-mcp/token)
$ curl -si -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -X POST http://127.0.0.1:8765/mcp \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}'
HTTP/1.1 200 OK
Mcp-Session-Id: 2187b1b0c28a838231bdf6fe24c365cb
…
$ curl -s -H "Content-Type: application/json" -X POST http://127.0.0.1:8765/mcp -d '{}'
{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"a valid bearer token is required"}}
```

## Адрес для поставщика

Модель поставщика ходит к серверу из интернета, поэтому нужен публичный
адрес `https` с настоящим сертификатом. Сервер слушает только loopback, а
TLS снимает прокси или туннель перед ним — например:

```
caddy reverse-proxy --from mcp.example.com --to 127.0.0.1:8765
```

Или сразу TLS в самом сервере — тогда и сетевой адрес:

```
ochan-mcp --root /srv/configs --listen 0.0.0.0:8443 \
  --tls-cert /etc/letsencrypt/live/mcp.example.com/fullchain.pem \
  --tls-key  /etc/letsencrypt/live/mcp.example.com/privkey.pem
```

Без TLS сервер на сетевом адресе не стартует: токен ушёл бы открытым
текстом.

## Модель

Claude API — коннектор MCP прямо в запросе Messages, без своего клиента
([claude-api.sh](../../mcp/clients/claude-api.sh)):

```json
"mcp_servers": [{
  "type": "url",
  "url": "https://mcp.example.com/mcp",
  "name": "octochan",
  "authorization_token": "…токен…"
}],
"tools": [{"type": "mcp_toolset", "mcp_server_name": "octochan"}]
```

с заголовком `anthropic-beta: mcp-client-2025-11-20`. OpenAI Responses
API — инструмент `{"type": "mcp", "server_url": …, "headers": {"Authorization": "Bearer …"}}`
([openai-responses.sh](../../mcp/clients/openai-responses.sh)). ChatGPT и
Claude.ai подключают свой коннектор в настройках: адрес
`https://mcp.example.com/mcp/<токен>` и без аутентификации — токен уже в
адресе. Остальные клиенты — в [mcp/clients/](../../mcp/clients/).

Этот шаг — запрос к API поставщика, и вывод модели в рецепте не
приводится: он у каждой модели свой. Важно, что она получит первым делом —
протокол формального промпта в `instructions` — и что сможет: читать,
сравнивать, планировать и оформлять заявки.

## Заявка модели

Попросив «выровняй max_connections на prod по test», модель вызывает
`request_config`, и у вас появляется заявка:

```
$ ochan requests
2026-10-23 13:33  216351534a  ждёт решения  /srv/configs/prod.conf  правка /srv/configs/prod.conf, параметров: 1
```

Заявку нейросети согласуют, только посмотрев план. На терминале `approve`
показывает его и просит ввести номер заявки; не на терминале — `--yes`:

```
$ ochan approve 216351534a
Заявку создала нейросеть (роль llm). Согласуйте её, только проверив план:

План: /srv/configs/test.conf -> /srv/configs/prod.conf

Изменений: 1

  /srv/configs/prod.conf
    изменить   max_connections: 100 -> 200

ошибка: заявку нейросети согласуют, проверив план выше: повторите с --yes
$ ochan approve --yes 216351534a
Заявка 216351534a: согласована (root, роль master)
Выполнить: ochan apply --request 216351534a
```

Выполняет — `ochan apply --request 216351534a` — тоже человек.

## Что видно потом

```
$ ochan audit --role llm
+ 2026-10-23 13:33:04  llm        root             fp protocol
+ 2026-10-23 13:33:04  llm        mcp 127.0.0.1    request /srv/configs/prod.conf /srv/configs/test.conf
```

Каждый вызов удалённой модели — роль `llm` и адрес клиента. За прокси
адрес — прокси: сервер не верит заголовкам `X-Forwarded-For`.

## Что держит модель

- Инструменты — закрытая таблица: `apply`, `approve`, `rollback`, `run`
  в ней нет, и ochan под сервером — в роли `llm`, которой их не выдать.
- Файлы — только в `--root`; живые источники и переменные окружения — только
  разрешённые флагами; сценарии модели — в песочнице ОС.
- Предел `llm` — 30 заявок в час.
- Сессия выдаётся при `initialize` и истекает через час без запросов;
  запрос из браузера с чужого Origin — 403.

## Смотрите также

- [Сервер MCP](../../mcp/README.md#удалённый-доступ-по-http) — флаги, защита, клиенты.
- [Нейросеть пишет сценарий по формальному промпту](ai-script.md).
- [Безопасность](../guide/security.md).
