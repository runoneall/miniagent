package kvstore

import (
	"maps"
	"slices"
)

func kvList() []string {
	lock.RLock()
	defer lock.RUnlock()

	return slices.Collect(maps.Keys(store))
}
