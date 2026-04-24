package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
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
	ctx, cancel := context.WithCancel(context.Background())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigs
		cancel()
	}()

	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Str("role", "task").Logger()
	ctx = log.WithContext(ctx)

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
