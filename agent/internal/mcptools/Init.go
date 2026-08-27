package mcptools

import (
	"context"
	"miniagent/agent/internal/onexit"
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

		onexit.Do(func() {
			cli.Close()
		})

		mcpTool, err := getMCPTool(cli)
		if err != nil {
			return err
		}

		tools = append(tools, mcpTool...)
	}

	return nil
}
