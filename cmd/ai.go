package cmd

import (
	"log"
	"miniagent/config"

	"github.com/spf13/cobra"
)

var aiCmd = &cobra.Command{
	Use:   "ai",
	Short: "管理 AI 配置",
	Run: func(cmd *cobra.Command, args []string) {
		cfg := config.Get()

		BaseURL, _ := cmd.Flags().GetString("base-url")
		if BaseURL != "" {
			cfg.AI.BaseURL = BaseURL
		}

		APIKey, _ := cmd.Flags().GetString("api-key")
		if APIKey != "" {
			cfg.AI.APIKey = APIKey
		}

		Model, _ := cmd.Flags().GetString("model")
		if Model != "" {
			cfg.AI.Model = Model
		}

		if err := config.Dump(cfg); err != nil {
			log.Fatalln(err)
		}
	},
}

func init() {
	configCmd.AddCommand(aiCmd)
	aiCmd.Flags().String("base-url", "", `json:"base_url"`)
	aiCmd.Flags().String("api-key", "", `json:"api_key"`)
	aiCmd.Flags().String("model", "", `json:"model"`)
}
