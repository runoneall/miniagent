package kvstore

import (
	"fmt"
)

func kvDelete(key string) error {
	lock.Lock()
	defer lock.Unlock()

	if _, ok := store[key]; !ok {
		return fmt.Errorf("key '%s' not found", key)
	}

	delete(store, key)
	return save()
}
