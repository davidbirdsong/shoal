package main

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "shoal",
	Short: "Gossip-empowered service bridge for ECS tasks and HAProxy",
}

var sidecarCmd = &cobra.Command{
	Use:   "sidecar",
	Short: "Run the HAProxy sidecar — joins gossip cluster and drives HAProxy runtime socket",
	RunE:  runSidecar,
}

var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Run the task parent — binds ephemeral port, forks worker, joins gossip cluster",
	RunE:  runTask,
}

func init() {
	rootCmd.AddCommand(sidecarCmd, taskCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
