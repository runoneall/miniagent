package config

type Config struct {
	KVStoreFile string `json:"kv_store_file"`
	AgentFile   string `json:"agent_file"`
	AI          AI     `json:"ai"`
	MCP         []MCP  `json:"mcp"`
}

type AI struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

type MCP struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	URL        string            `json:"url"`
	HTTPHeader map[string]string `json:"http_header"`
	Command    string            `json:"command"`
	EnvVars    map[string]string `json:"env_vars"`
}
