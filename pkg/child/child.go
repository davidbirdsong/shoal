package child

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func SubProcFromArgs(cmd *cobra.Command, args []string, shouldExec bool) error {
	logger := zerolog.Ctx(cmd.Context())

	dashAt := cmd.ArgsLenAtDash()
	if dashAt >= 0 {

		args = args[dashAt:]
		logger.Debug().Str("exec", args[0]).
			Str("args", strings.Join(args[0:], ",")).
			Msg("execing into subprocess ")
		exec.Command(args[0], args[1:]...)
	}
	return fmt.Errorf("command args passed in missing '--' cmd")
}
