package config

import (
	"encoding/json"
	"os"
)

func Dump(cfg1 Config) error {
	lock.Lock()
	defer lock.Unlock()

	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	defer f.Close()
	if err := json.NewEncoder(f).Encode(cfg1); err != nil {
		return err
	}

	cfg = cfg1
	return nil
}
