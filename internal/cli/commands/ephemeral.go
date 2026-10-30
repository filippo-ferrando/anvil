package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newEphemeralCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ephemeral",
		Short: "Inspect the clones of ephemeral runs, the ones kept by --keep-failed above all",
	}
	cmd.AddCommand(newEphemeralListCommand(flags), newEphemeralShellCommand(flags), newEphemeralRmCommand(flags))
	return cmd
}

func newEphemeralListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List running and kept clones",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Ephemeral.CloneList(cmd.Context(), &anvilv1.CloneListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(os.Stdout, reply)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tDISTRO\tSTATE\tEXIT\tAGE")
				for _, cl := range reply.GetClones() {
					state, exit := "running", "-"
					if cl.GetExited() {
						state, exit = "done", fmt.Sprint(cl.GetExitCode())
					}
					if cl.GetKept() {
						state = "kept"
					}
					age := time.Since(time.Unix(cl.GetStartedAtUnix(), 0)).Round(time.Second)
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", cl.GetId(), cl.GetDistro(), state, exit, age)
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newEphemeralShellCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell <clone> [-- <command> [args...]]",
		Short: "Open a root shell in a clone (or run a command there)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			code, err := cloneExec(cmd, c, args[0], args[1:])
			if err != nil {
				return err
			}
			return ExitError{Code: code}.orNil()
		},
	}
	return cmd
}

// cloneExec runs command (a login shell when empty) in clone id, on a terminal when
// stdin is one, and returns its exit code.
func cloneExec(cmd *cobra.Command, c *client.Client, id string, command []string) (int, error) {
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM)
	defer stop()
	stream, err := c.Ephemeral.CloneExec(ctx)
	if err != nil {
		return -1, err
	}
	tty := term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
	req := &anvilv1.CloneExecRequest{CloneId: id, Command: command, Tty: tty}
	if tty {
		if w, h, err := term.GetSize(os.Stdout.Fd()); err == nil {
			req.Size = &anvilv1.TerminalSize{Rows: uint32(h), Cols: uint32(w)}
		}
	}
	var sendMu sync.Mutex
	send := func(in *anvilv1.CloneExecInput) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(in)
	}
	if err := send(&anvilv1.CloneExecInput{Input: &anvilv1.CloneExecInput_Request{Request: req}}); err != nil {
		return -1, err
	}
	restore := func() {}
	if tty {
		if state, err := term.MakeRaw(os.Stdin.Fd()); err == nil {
			var once sync.Once
			restore = func() { once.Do(func() { _ = term.Restore(os.Stdin.Fd(), state) }) }
		}
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if w, h, err := term.GetSize(os.Stdout.Fd()); err == nil {
					_ = send(&anvilv1.CloneExecInput{Input: &anvilv1.CloneExecInput_Resize{Resize: &anvilv1.TerminalSize{Rows: uint32(h), Cols: uint32(w)}}})
				}
			}
		}()
	}
	defer restore()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && send(&anvilv1.CloneExecInput{Input: &anvilv1.CloneExecInput_Stdin{Stdin: append([]byte(nil), buf[:n]...)}}) != nil {
				return
			}
			if err != nil {
				_ = send(&anvilv1.CloneExecInput{Input: &anvilv1.CloneExecInput_StdinClose{StdinClose: true}})
				return
			}
		}
	}()
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return -1, errors.New("the daemon ended the session without an exit code")
		}
		if err != nil {
			return -1, err
		}
		switch o := out.GetOutput().(type) {
		case *anvilv1.CloneExecOutput_Stdout:
			_, _ = os.Stdout.Write(o.Stdout)
		case *anvilv1.CloneExecOutput_Stderr:
			_, _ = os.Stderr.Write(o.Stderr)
		case *anvilv1.CloneExecOutput_ExitCode:
			return int(o.ExitCode), nil
		}
	}
}

func newEphemeralRmCommand(flags *globalFlags) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "rm <clone>...",
		Aliases: []string{"remove"},
		Short:   "Delete clones, kept or still running",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !all {
				return fmt.Errorf("name the clones to delete, or --all")
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Ephemeral.CloneDelete(cmd.Context(), &anvilv1.CloneDeleteRequest{Ids: args, All: all})
				for _, id := range reply.GetDeleted() {
					fmt.Println("deleted", id)
				}
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "delete every clone")
	return cmd
}
