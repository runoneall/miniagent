package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"miniagent/agent/internal/agentfile"
	"miniagent/agent/internal/kvstore"
	"miniagent/agent/internal/mcptools"
	"miniagent/agent/internal/onexit"
	"miniagent/agent/internal/stdout"
	"miniagent/config"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func Run() error {
	onexit.Listen()

	if err := kvstore.Init(); err != nil {
		return err
	}

	if err := mcptools.Init(); err != nil {
		return err
	}

	for {
		cfg := config.Get()
		ctx := context.Background()
		logger := log.New(stdout.Writer, "", log.LstdFlags)

		var (
			MaxTokens           int = 16384
			MaxCompletionTokens     = MaxTokens
		)

		model, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL:             cfg.AI.BaseURL,
			APIKey:              cfg.AI.APIKey,
			Model:               cfg.AI.Model,
			MaxTokens:           &MaxTokens,
			MaxCompletionTokens: &MaxCompletionTokens,
		})

		if err != nil {
			return err
		}

		kvTools, err := kvstore.Tools()
		if err != nil {
			return err
		}

		agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
			Model:         model,
			Name:          "miniagent",
			Description:   "A friendly, all-around agent",
			Instruction:   "You are a friendly, all-around agent.",
			MaxIterations: math.MaxInt,

			ToolsConfig: adk.ToolsConfig{
				ToolsNodeConfig: compose.ToolsNodeConfig{
					Tools: append(kvTools, mcptools.Tools()...),
					ToolCallMiddlewares: []compose.ToolMiddleware{
						{
							Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
								return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
									fmt.Fprintln(stdout.Writer)
									logger.Printf("INFO 工具调用 tool=%s arguments=%s\n", input.Name, input.Arguments)

									output, err := next(ctx, input)
									if err != nil {
										logger.Printf("ERROR %v\n", err)

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

		now := time.Now()
		currentTime := now.Format("2006-01-02 15:04:05")

		zoneName, offset := now.Zone()
		timeZone := fmt.Sprintf("%s (UTC%s%d)", zoneName, func() string {
			if offset >= 0 {
				return "+"
			}

			return ""
		}(), offset/3600)

		tellAgent, err := agentfile.Read()
		if err != nil {
			return err
		}

		input := []adk.Message{
			schema.SystemMessage(fmt.Sprintf("Current Time: %s. Time Zone: %s.", currentTime, timeZone)),
			schema.UserMessage(tellAgent),
		}

		runner := adk.NewRunner(ctx, adk.RunnerConfig{
			Agent:           agent,
			EnableStreaming: true,
		})

		events := runner.Run(ctx, input)
		for {
			event, ok := events.Next()
			if !ok {
				break
			}

			if event.Err != nil {
				logger.Printf("ERROR %v\n", event.Err)
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
							logger.Printf("ERROR %v\n", err)
							break
						}

						if msg != nil {
							fmt.Fprint(stdout.Writer, msg.Content)
						}
					}
				}
			}
		}

		fmt.Fprintln(stdout.Writer)
		logger.Println("INFO Agent 已退出任务")
	}
}
