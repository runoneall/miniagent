package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"miniagent/config"

	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "管理 miniagent 配置",
	Run: func(cmd *cobra.Command, args []string) {
		cfg := config.Get()

		KVStoreFile, _ := cmd.Flags().GetString("kv-store-file")
		if KVStoreFile != "" {
			cfg.KVStoreFile = KVStoreFile
		}

		AgentFile, _ := cmd.Flags().GetString("agent-file")
		if AgentFile != "" {
			cfg.AgentFile = AgentFile
		}

		if KVStoreFile == "" && AgentFile == "" {
			rawJSON, err := json.MarshalIndent(cfg, "", config.Indent)
			if err != nil {
				log.Fatalln(err)
			}

			fmt.Println(string(rawJSON))
			return
		}

		if err := config.Dump(cfg); err != nil {
			log.Fatalln(err)
		}
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.Flags().String("kv-store-file", "", `json:"kv_store_file"`)
	configCmd.Flags().String("agent-file", "", `json:"agent_file"`)
}
