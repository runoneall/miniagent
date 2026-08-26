package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"miniagent/agent/internal/agentfile"
	"miniagent/agent/internal/kvstore"
	"miniagent/agent/internal/markdown/terminal"
	"miniagent/config"
	"os"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func Run() error {
	if err := kvstore.Init(); err != nil {
		return err
	}

	for {
		cfg := config.Get()
		ctx := context.Background()
		render := terminal.NewStreamRenderer(os.Stdout)
		logger := log.New(render, "", log.LstdFlags)

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

		kvTools, err := kvstore.Tools()
		if err != nil {
			return err
		}

		agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
			Model:       model,
			Name:        "miniagent",
			Description: "A friendly, all-around agent",
			Instruction: fmt.Sprintf(
				"You are a friendly, all-around agent. Current Time: %s. Time Zone: %s. Please respond to the user in a warm tone.",
				currentTime, timeZone,
			),
			ToolsConfig: adk.ToolsConfig{
				ToolsNodeConfig: compose.ToolsNodeConfig{
					Tools: kvTools,
					ToolCallMiddlewares: []compose.ToolMiddleware{
						{
							Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
								return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
									fmt.Fprint(render, "\n\n")
									logger.Printf("INFO 工具调用 tool=%s arguments=%s\n\n", input.Name, input.Arguments)

									output, err := next(ctx, input)
									if err != nil {
										logger.Printf("ERROR %v\n\n", err)

										return &compose.ToolOutput{
											Result: fmt.Sprintf("Tool %s execution failed with error: %v. Please correct your input or handle this fallback.", input.Name, err),
										}, nil
									}

									return output, nil
								}
							},
						},
					},
				},
			},
		})

		if err != nil {
			return err
		}

		runner := adk.NewRunner(ctx, adk.RunnerConfig{
			Agent:           agent,
			EnableStreaming: true,
		})

		tellAgent, err := agentfile.Read()
		if err != nil {
			return err
		}

		input := []adk.Message{
			schema.UserMessage(tellAgent),
		}

		events := runner.Run(ctx, input)
		for {
			event, ok := events.Next()
			if !ok {
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
							logger.Printf("ERROR %v\n\n", err)
							break
						}

						if msg != nil {
							fmt.Fprint(render, msg.Content)
						}
					}
				}
			}
		}

		fmt.Fprint(render, "\n\n")
	}
}
