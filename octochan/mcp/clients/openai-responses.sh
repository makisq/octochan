#!/bin/sh
# OpenAI Responses API: инструмент mcp. Сервер должен быть доступен из
# интернета по https.
#
# require_approval: "never" — вызовы идут без подтверждения в приложении:
# через ochan-mcp ничего не выполняется, а заявку всё равно согласует
# человек в ochan. Нужно подтверждение на каждый вызов — "always".
curl https://api.openai.com/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -d '{
    "model": "gpt-5",
    "input": "Compare prod.conf with test.conf and file a request to align max_connections.",
    "tools": [
      {
        "type": "mcp",
        "server_label": "octochan",
        "server_url": "https://mcp.example.com/mcp",
        "headers": {"Authorization": "Bearer '"$OCHAN_MCP_TOKEN"'"},
        "require_approval": "never"
      }
    ]
  }'
