# miniagent

基于 OpenAI + MCP 实现的 Agent 框架

## 基础配置

```json
{
    "kv_store_file": "kv-store.json",
    "agent_file": "agent.md"
}
```

## OpenAI 示例配置

```json
    "ai": {
        "base_url": "",
        "api_key": "",
        "model": ""
    },
```

## MCP 示例配置

```json
    "mcp": [
        {
            "type": "stdio",
            "command": "",
            "env_vars": {}
        },
        {
            "type": "http",
            "url": "",
            "http_header": {}
        }
    ]
```
