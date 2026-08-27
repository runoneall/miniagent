package onexit

import (
	"os"
	"os/signal"
	"sync"
)

var (
	funcs []func()
	lock  sync.RWMutex
)

func Listen() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	go func(sigChan chan os.Signal) {
		<-sigChan

		lock.RLock()
		defer lock.RUnlock()

		for _, f := range funcs {
			f()
		}

		os.Exit(0)
	}(sigChan)
}
