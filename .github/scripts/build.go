package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

type Target struct {
	GOOS   string `json:"GOOS"`
	GOARCH string `json:"GOARCH"`
}

func main() {
	f, err := os.Open(".github/build/targets.json")
	if err != nil {
		log.Fatalln(err)
	}
	defer f.Close()

	var targets []Target
	if err := json.NewDecoder(f).Decode(&targets); err != nil {
		log.Fatalln(err)
	}

	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)

	for _, target := range targets {
		wg.Add(1)
		limit <- struct{}{}

		go func(target Target) {
			defer func() {
				wg.Done()
				<-limit
			}()

			log.Println("build", fmt.Sprintf("%s/%s", target.GOOS, target.GOARCH))

			ext := ""
			if target.GOOS == "windows" {
				ext = ".exe"
			}

			name := fmt.Sprintf("miniagent-%s-%s%s", target.GOOS, target.GOARCH, ext)
			file := filepath.Join(".github/build/dist", name)

			run(
				[]string{"go", "build", "-trimpath", "-ldflags", "-s -w", "-o", file, "."},
				[]string{
					"CGO_ENABLED=0",
					"GOMAXPROCS=1",
					fmt.Sprintf("HOME=%s", os.Getenv("HOME")),
					fmt.Sprintf("GOOS=%s", target.GOOS),
					fmt.Sprintf("GOARCH=%s", target.GOARCH),
				},
			)
		}(target)
	}

	wg.Wait()
}

func run(parts, env []string) {
	cmd := exec.Command(parts[0], parts[1:]...)

	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Run()
}
