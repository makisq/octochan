#!/bin/sh
# Claude API: MCP-коннектор Messages API подключается к ochan-mcp сам —
# MCP-клиент писать не нужно. Адрес должен быть доступен из интернета по
# https. Токен уходит серверу заголовком Authorization: Bearer.
#
# fallbacks: "default" — если модель откажет по политике безопасности,
# запрос обслужит запасная модель, а не вернётся пустым.
curl https://api.anthropic.com/v1/messages \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "anthropic-beta: mcp-client-2025-11-20,server-side-fallback-2026-07-01" \
  -d '{
    "model": "claude-opus-5-5",
    "max_tokens": 16000,
    "fallbacks": "default",
    "messages": [{"role": "user", "content": "Compare prod.conf with test.conf and file a request to align max_connections."}],
    "mcp_servers": [
      {
        "type": "url",
        "url": "https://mcp.example.com/mcp",
        "name": "octochan",
        "authorization_token": "'"$OCHAN_MCP_TOKEN"'"
      }
    ],
    "tools": [{"type": "mcp_toolset", "mcp_server_name": "octochan"}]
  }'
