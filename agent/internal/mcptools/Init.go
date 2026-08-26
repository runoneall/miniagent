package mcptools

import (
	"context"
	"miniagent/config"
	"sync"

	"github.com/cloudwego/eino/components/tool"
)

var (
	ctx   = context.Background()
	tools []tool.BaseTool
	lock  sync.RWMutex
)

func Init() error {
	lock.Lock()
	defer lock.Unlock()

	cfg := config.Get()
	for _, mcpCfg := range cfg.MCP {
		cli, err := newClient(mcpCfg)
		if err != nil {
			return err
		}

		mcpTool, err := getMCPTool(cli)
		if err != nil {
			return err
		}

		tools = append(tools, mcpTool...)
	}

	return nil
}
