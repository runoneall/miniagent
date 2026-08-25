package config

import (
	"github.com/barkimedes/go-deepcopy"
)

func Get() Config {
	lock.RLock()
	defer lock.RUnlock()

	return deepcopy.MustAnything(cfg).(Config)
}
