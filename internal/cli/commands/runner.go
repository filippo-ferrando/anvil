package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// runnerTokenEnv is where `anvil runner add` reads the forge token when there is no --token-file.
const runnerTokenEnv = "ANVIL_FORGE_TOKEN"

func newRunnerCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runner",
		Short: "Run Forgejo/Gitea or GitHub Actions jobs in ephemeral clones",
		Long: "A runner polls a forge for waiting jobs whose labels it maps to a distro, and runs each job in its own " +
			"throwaway clone with the forge's runner inside it, for that one job. Needs Forgejo 15 or later, or GitHub. " +
			"See docs/runner.md.",
	}
	cmd.AddCommand(newRunnerAddCommand(flags), newRunnerListCommand(flags), newRunnerRmCommand(flags),
		newRunnerPauseCommand(flags, true), newRunnerPauseCommand(flags, false), newRunnerJobsCommand(flags))
	return cmd
}

// parseLabels turns label=distro pairs into a map.
func parseLabels(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		label, distro, ok := strings.Cut(p, "=")
		if !ok || label == "" || distro == "" {
			return nil, fmt.Errorf("--label %q: want <job label>=<distro or @warm-key>", p)
		}
		out[label] = distro
	}
	return out, nil
}

// readToken reads the forge token from file ("-" for stdin), or else from $ANVIL_FORGE_TOKEN.
func readToken(file string, stdin io.Reader) (string, error) {
	var data []byte
	var err error
	switch file {
	case "":
		if t := strings.TrimSpace(os.Getenv(runnerTokenEnv)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("give the forge API token with --token-file <file> (- for stdin) or $%s", runnerTokenEnv)
	case "-":
		data, err = io.ReadAll(io.LimitReader(stdin, 64<<10))
	default:
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return "", fmt.Errorf("reading the token: %w", err)
	}
	t := strings.TrimSpace(string(data))
	if t == "" {
		return "", errors.New("the token is empty")
	}
	return t, nil
}

func newRunnerAddCommand(flags *globalFlags) *cobra.Command {
	var (
		r         anvilv1.Runner
		labels    []string
		tokenFile string
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a runner and start taking jobs",
		Example: "  anvil runner add ci --forge forgejo --url https://code.example.com --scope orgs/acme \\\n" +
			"      --label debian=debian-13 --label ubuntu=ubuntu-24.04 --token-file ~/.forgejo-token\n" +
			"  ANVIL_FORGE_TOKEN=ghp_... anvil runner add gh --forge github --scope repos/acme/app --label anvil=ubuntu-24.04",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := parseLabels(labels)
			if err != nil {
				return err
			}
			token, err := readToken(tokenFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			r.Name, r.Labels = args[0], m
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Runner.Add(cmd.Context(), &anvilv1.RunnerAddRequest{Runner: &r, Token: token})
				if err != nil {
					return err
				}
				fmt.Printf("added runner %s, polling %s %s\n", reply.GetRunner().GetName(), reply.GetRunner().GetUrl(), reply.GetRunner().GetScope())
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&r.Forge, "forge", "forgejo", `"forgejo" (also Gitea-compatible Forgejo 15+) or "github"`)
	cmd.Flags().StringVar(&r.Url, "url", "", "the forge's base URL (default https://github.com for github)")
	cmd.Flags().StringVar(&r.Scope, "scope", "", "where jobs come from: repos/<owner>/<repo>, orgs/<org> or admin (forgejo)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "map a job label to a distro or @<warm key>: <label>=<distro> (repeatable)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file with the forge API token, - for stdin (default $"+runnerTokenEnv+")")
	cmd.Flags().Int32Var(&r.Cpus, "cpus", 0, "vCPUs of each job's clone (default 2)")
	cmd.Flags().Int64Var(&r.MemoryMib, "memory", 0, "memory of each job's clone in MiB (default 2048)")
	cmd.Flags().Int32Var(&r.MaxJobs, "max-jobs", 0, "jobs at once (default 2)")
	cmd.Flags().StringVar(&r.Version, "version", "", "runner release to install in the clones (default the latest)")
	cmd.Flags().BoolVar(&r.Paused, "paused", false, "add it paused, see anvil runner resume")
	_ = cmd.MarkFlagRequired("scope")
	return cmd
}

func newRunnerListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List runners",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Runner.List(cmd.Context(), &anvilv1.RunnerListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(os.Stdout, reply)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tFORGE\tSCOPE\tLABELS\tJOBS\tSTATE")
				for _, r := range reply.GetRunners() {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\n", r.GetName(), r.GetForge(), r.GetScope(), runnerLabels(r.GetLabels()),
						r.GetRunning(), r.GetMaxJobs(), runnerState(r))
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

// runnerLabels is the LABELS column: label=distro pairs, sorted.
func runnerLabels(m map[string]string) string {
	var out []string
	for l, d := range m {
		out = append(out, l+"="+d)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// runnerState is the STATE column: paused, the last poll's error, or when it last polled.
func runnerState(r *anvilv1.Runner) string {
	switch {
	case r.GetPaused():
		return "paused"
	case r.GetLastError() != "":
		return "error: " + r.GetLastError()
	case r.GetLastPollUnix() == 0:
		return "starting"
	}
	return "polled " + time.Since(time.Unix(r.GetLastPollUnix(), 0)).Round(time.Second).String() + " ago"
}

func newRunnerRmCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a runner, stopping its running jobs",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				if _, err := c.Runner.Remove(cmd.Context(), &anvilv1.RunnerRemoveRequest{Name: args[0]}); err != nil {
					return err
				}
				fmt.Println("removed runner", args[0])
				return nil
			})
		},
	}
}

func newRunnerPauseCommand(flags *globalFlags, pause bool) *cobra.Command {
	use, short := "resume <name>", "Take new jobs again"
	if pause {
		use, short = "pause <name>", "Stop taking new jobs; running ones go on"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Runner.SetPaused(cmd.Context(), &anvilv1.RunnerSetPausedRequest{Name: args[0], Paused: pause})
				if err != nil {
					return err
				}
				fmt.Printf("runner %s: %s\n", reply.GetRunner().GetName(), runnerState(reply.GetRunner()))
				return nil
			})
		},
	}
}

func newRunnerJobsCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "jobs [runner]",
		Aliases: []string{"status"},
		Short:   "List running and recent jobs, newest first",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Runner.Jobs(cmd.Context(), &anvilv1.RunnerJobsRequest{Runner: name})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(os.Stdout, reply)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "RUNNER\tJOB\tNAME\tDISTRO\tSTATE\tCLONE\tSTARTED\tDURATION\tNOTE")
				for _, j := range reply.GetJobs() {
					fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.GetRunner(), j.GetJobId(), dash(j.GetName()), j.GetDistro(), j.GetState(),
						dash(j.GetCloneId()), time.Unix(j.GetStartedAtUnix(), 0).Format("01-02 15:04"), jobDuration(j.GetStartedAtUnix(), j.GetEndedAtUnix()), oneLine(j.GetError(), 60))
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// jobDuration is how long a job took, or has been running for.
func jobDuration(start, end int64) string {
	if end == 0 {
		return time.Since(time.Unix(start, 0)).Round(time.Second).String() + "+"
	}
	return (time.Duration(end-start) * time.Second).String()
}

// oneLine keeps the last line of s, cut to n runes.
func oneLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}
