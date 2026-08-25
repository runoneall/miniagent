package agentfile

import (
	"os"
	"sync"
	"time"

	"miniagent/config"
)

var (
	lock          sync.RWMutex
	cachedContent string
	cachedModTime time.Time
)

func Read() (string, error) {
	cfg := config.Get()
	info, err := os.Stat(cfg.AgentFile)
	if err != nil {
		if os.IsNotExist(err) {
			lock.Lock()
			defer lock.Unlock()

			cachedContent = ""
			cachedModTime = time.Time{}

			return "", nil
		}

		return "", err
	}

	modTime := info.ModTime()

	lock.RLock()
	if modTime.Equal(cachedModTime) && cachedContent != "" {
		content := cachedContent

		lock.RUnlock()
		return content, nil
	}
	lock.RUnlock()

	lock.Lock()
	defer lock.Unlock()

	if modTime.Equal(cachedModTime) && cachedContent != "" {
		return cachedContent, nil
	}

	data, err := os.ReadFile(cfg.AgentFile)
	if err != nil {
		if os.IsNotExist(err) {
			cachedContent = ""
			cachedModTime = time.Time{}

			return "", nil
		}

		return "", err
	}

	cachedContent = string(data)
	cachedModTime = modTime

	return cachedContent, nil
}
