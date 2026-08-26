package mcptools

import (
	"fmt"
	"miniagent/config"

	"github.com/arkady-emelyanov/go-shellparse"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
)

func newClient(cfg config.MCP) (*client.Client, error) {
	switch cfg.Type {

	case config.MCPTypeHTTP:
		return client.NewStreamableHttpClient(cfg.URL, transport.WithHTTPHeaders(cfg.HTTPHeader))

	case config.MCPTypeStdio:
		bin, args, err := shellparse.Command(cfg.Command)
		if err != nil {
			return nil, err
		}

		env := []string{}
		for k, v := range cfg.EnvVars {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}

		return client.NewStdioMCPClient(bin, env, args...)

	}

	return nil, fmt.Errorf("未知 MCP 类型: %s", cfg.Type)
}
