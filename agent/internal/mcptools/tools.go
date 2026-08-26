package mcptools

import "github.com/cloudwego/eino/components/tool"

func Tools() []tool.BaseTool {
	lock.RLock()
	defer lock.RUnlock()

	return tools
}
