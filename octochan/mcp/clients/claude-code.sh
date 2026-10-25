#!/bin/sh
# Claude Code: удалённый сервер по Streamable HTTP.
claude mcp add --transport http octochan https://mcp.example.com/mcp \
  --header "Authorization: Bearer $OCHAN_MCP_TOKEN"

# Локально, по stdio, без сети и токена:
#   claude mcp add octochan -- /usr/local/bin/ochan-mcp --root /srv/configs
