package cmd

import (
	"log"
	"miniagent/agent"

	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "运行 miniagent",
	Run: func(cmd *cobra.Command, args []string) {
		if err := agent.Run(); err != nil {
			log.Fatalln(err)
		}
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
}
