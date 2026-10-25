"""Своя программа на официальном MCP SDK для Python (2.x): так к
ochan-mcp подключается модель, у клиента которой нет встроенного MCP.

    pip install "mcp>=2"
    OCHAN_MCP_TOKEN=... python python_client.py https://mcp.example.com/mcp
"""

import asyncio
import os
import sys

import httpx2
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client


async def main(url: str) -> None:
    headers = {"Authorization": "Bearer " + os.environ["OCHAN_MCP_TOKEN"]}
    async with httpx2.AsyncClient(headers=headers, timeout=180) as http:
        async with streamable_http_client(url, http_client=http) as (read, write):
            async with ClientSession(read, write) as session:
                info = await session.initialize()
                print("server:", info.server_info.name, info.server_info.version,
                      "protocol", info.protocol_version)
                tools = await session.list_tools()
                print("tools:", ", ".join(tool.name for tool in tools.tools))
                result = await session.call_tool("diff", {"left": "prod.conf", "right": "test.conf"})
                for block in result.content:
                    print(getattr(block, "text", block))


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8765/mcp"))
