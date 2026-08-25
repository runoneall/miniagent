package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"miniagent/config"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func Run() error {
	cfg := config.Get()

	for {
		ctx := context.Background()

		model, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL: cfg.AI.BaseURL,
			APIKey:  cfg.AI.APIKey,
			Model:   cfg.AI.Model,
		})

		if err != nil {
			return err
		}

		now := time.Now()
		currentTime := now.Format("2006-01-02 15:04:05")

		zoneName, offset := now.Zone()
		timeZone := fmt.Sprintf("%s (UTC%s%d)", zoneName, func() string {
			if offset >= 0 {
				return "+"
			}

			return ""
		}(), offset/3600)

		agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
			Model:       model,
			Name:        "miniagent",
			Description: "A friendly, all-around agent",
			Instruction: fmt.Sprintf(
				"You are a friendly, all-around agent. Current Time: %s. Time Zone: %s. Please respond to the user in a warm tone.",
				currentTime, timeZone,
			),
		})

		if err != nil {
			return err
		}

		runner := adk.NewRunner(ctx, adk.RunnerConfig{
			Agent:           agent,
			EnableStreaming: true,
		})

		input := []adk.Message{
			schema.UserMessage("<TODO>"),
		}

		events := runner.Run(ctx, input)
		for {
			event, ok := events.Next()
			if !ok {
				break
			}

			if event.Err != nil {
				slog.Error(event.Err.Error())
				break
			}

			if event.Output != nil && event.Output.MessageOutput != nil {
				stream := event.Output.MessageOutput.MessageStream

				if stream != nil {
					for {
						msg, err := stream.Recv()
						if errors.Is(err, io.EOF) {
							break
						}

						if err != nil {
							slog.Error(err.Error())
							break
						}

						if msg != nil {
							fmt.Print(msg.Content)
						}
					}
				}
			}
		}

		fmt.Print("\n")
	}
}
