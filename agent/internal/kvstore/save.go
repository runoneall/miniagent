package kvstore

import (
	"encoding/json"
	"miniagent/config"
	"os"
)

func save() error {
	cfg := config.Get()
	f, err := os.OpenFile(cfg.KVStoreFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	defer f.Close()
	return json.NewEncoder(f).Encode(store)
}
