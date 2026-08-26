package kvstore

import (
	"encoding/json"
	"miniagent/config"
	"os"
	"sync"
)

var (
	store = map[string]string{}
	lock  sync.RWMutex
)

func Init() error {
	lock.Lock()
	defer lock.Unlock()

	cfg := config.Get()
	f, err := os.OpenFile(cfg.KVStoreFile, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}

	defer f.Close()
	if err := json.NewDecoder(f).Decode(&store); err != nil {
		store = map[string]string{}

		if err := f.Truncate(0); err != nil {
			return err
		}

		if _, err := f.Seek(0, 0); err != nil {
			return err
		}

		if err := json.NewEncoder(f).Encode(store); err != nil {
			return err
		}
	}

	return nil
}
