package commands

import (
	"io"
	"net"
	"os"

	"github.com/spf13/cobra"
)

// newDialStdioCommand pipes stdin/stdout to the local anvild socket. It is
// the far end of `anvil --remote`, run over ssh on the daemon's host.
func newDialStdioCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:    "dial-stdio",
		Short:  "Proxy stdin/stdout to the local anvild socket (used by --remote)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := net.Dial("unix", flags.socket)
			if err != nil {
				return err
			}
			defer conn.Close()
			return pipeStdio(conn.(*net.UnixConn), os.Stdin, os.Stdout)
		},
	}
}

// pipeStdio copies in to conn and conn to out until conn closes.
func pipeStdio(conn *net.UnixConn, in io.Reader, out io.Writer) error {
	go func() {
		_, _ = io.Copy(conn, in)
		_ = conn.CloseWrite()
	}()
	_, err := io.Copy(out, conn)
	return err
}
