package main

import (
	"log"
	"miniagent/cmd"
	"miniagent/config"
)

func main() {
	if err := config.Load(); err != nil {
		log.Fatalln(err)
	}

	cmd.Execute()
}
