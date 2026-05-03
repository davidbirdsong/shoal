package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"strings"
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

func getLogger(nodeName string) zerolog.Logger {
	var logWriter io.Writer = os.Stderr
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		logWriter = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
	}
	return zerolog.New(logWriter).With().Timestamp().Str("node", nodeName).
		Logger().Level(zerolog.DebugLevel)
}

func mustHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	s := strings.TrimPrefix(hostname, "ip-")
	s, _, _ = strings.Cut(s, ".")
	return "node-" + s
}

var nodeNameGlobal = fmt.Sprintf("%s-%x", mustHostname(), rand.Uint32()&0xfffff)

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	l := getLogger(nodeNameGlobal).With().Logger()

	go func() {
		<-sigs
		l.Warn().Msg("got signal shutting down")
		cancel()
	}()

	ctx = l.WithContext(ctx)

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
