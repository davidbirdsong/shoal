package child

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func ArgsFromCobra(cmd *cobra.Command, args []string) ([]string, error) {
	dashAt := cmd.ArgsLenAtDash()
	if dashAt >= 0 {
		src := args[dashAt:]
		dst := make([]string, len(src))
		copy(dst, src)
		return dst, nil
	}
	return []string{}, fmt.Errorf("No -- found on command line or nothing after")
}

func SubProcFromArgs(ctx context.Context, args []string) *exec.Cmd {
	logger := zerolog.Ctx(ctx)

	logger.Debug().Str("exec", args[0]).
		Str("args", strings.Join(args[0:], ",")).
		Msg("execing into subprocess ")
	return exec.Command(args[0], args[1:]...)
}
