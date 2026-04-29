package main

import (
	"bufio"
	"io"
	"os/exec"

	"github.com/rs/zerolog"
)

func pipeToLog(r io.Reader, event func() *zerolog.Event) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		event().Msg(s.Text())
	}
}

func pipeWorkers(w *exec.Cmd, l zerolog.Logger) func() {
	stdout, _ := w.StdoutPipe()
	stderr, _ := w.StderrPipe()

	return func() {
		go pipeToLog(stdout, l.Info)
		go pipeToLog(stderr, l.Warn)
	}
}
