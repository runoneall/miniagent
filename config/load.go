package config

import (
	"encoding/json"
	"os"
	"sync"
)

const file = "miniagent.json"

var (
	cfg  Config
	lock sync.RWMutex
)

func Load() error {
	lock.Lock()
	defer lock.Unlock()

	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}

	defer f.Close()
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		cfg = Config{
			KVStoreFile: "kv-store.json",
			AgentFile:   "agent.md",
			AI:          AI{},
			MCP:         []MCP{},
		}

		if err := f.Truncate(0); err != nil {
			return err
		}

		if _, err := f.Seek(0, 0); err != nil {
			return err
		}

		if err := jsonNewEncoder(f).Encode(cfg); err != nil {
			return err
		}
	}

	return nil
}
