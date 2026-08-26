package kvstore

import (
	"fmt"
)

func kvGet(key string) (string, error) {
	lock.RLock()
	defer lock.RUnlock()

	val, ok := store[key]
	if !ok {
		return "", fmt.Errorf("key '%s' not found", key)
	}

	return val, nil
}
