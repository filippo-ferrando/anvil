package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newPipelineCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pipeline",
		Short: "Run anvil-ci.yaml pipelines on ephemeral clones",
		Long: "A pipeline file lists jobs, each a command run on one throwaway clone per distro, ordered by what they need. " +
			"`run` uploads a directory and runs its pipeline; `watch` runs a repository's pipeline on every new commit of a branch. " +
			"See docs/pipelines.md.",
	}
	cmd.AddCommand(newPipelineRunCommand(flags), newPipelineRunsCommand(flags), newPipelineShowCommand(flags),
		newPipelineLogsCommand(flags), newPipelineArtifactsCommand(flags), newPipelineRerunCommand(flags),
		newPipelineCancelCommand(flags), newPipelineWatchCommand(flags))
	return cmd
}

func newPipelineRunCommand(flags *globalFlags) *cobra.Command {
	var file, artifacts string
	cmd := &cobra.Command{
		Use:   "run [path]",
		Short: "Upload a directory (default .) and run its pipeline",
		Long: "Upload path (what git doesn't ignore, in a git repo) and run its pipeline file, streaming every job's output. " +
			"Ctrl-C cancels the run and deletes its clones. Exits non-zero when the run failed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				return fmt.Errorf("%s is not a directory", path)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			stream, err := c.Pipeline.Run(ctx)
			if err != nil {
				return err
			}
			if err := stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_Request{Request: &anvilv1.PipelineRunRequest{File: file}}}); err != nil {
				return err
			}
			if err := client.SendUpload(path, func(b []byte) error {
				return stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_UploadChunk{UploadChunk: b}})
			}); err != nil {
				return err
			}
			if err := stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_UploadDone{UploadDone: true}}); err != nil {
				return err
			}
			run, err := printPipeline(ctx, stream.Recv)
			if err != nil {
				return err
			}
			return finishPipeline(cmd.Context(), c, run, artifacts)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "the pipeline file, relative to path (default anvil-ci.yaml)")
	cmd.Flags().StringVar(&artifacts, "artifacts", "", "download the run's collected files into this directory afterwards")
	return cmd
}

// finishPipeline downloads a finished run's files when asked, and turns its state into
// the exit code.
func finishPipeline(ctx context.Context, c *client.Client, run *anvilv1.PipelineRun, artifacts string) error {
	if run == nil {
		return errors.New("the run ended without a result")
	}
	if artifacts != "" {
		if err := downloadArtifacts(ctx, c, run.GetId(), artifacts); err != nil {
			fmt.Fprintln(os.Stderr, "anvil: artifacts:", err)
		}
	}
	fmt.Fprintf(os.Stderr, "run %s %s\n", run.GetId(), run.GetState())
	if run.GetState() != "succeeded" {
		return ExitError{Code: 1}
	}
	return nil
}

// jobKey names a job on one distro in the output: <job>@<distro>.
func jobKey(job, distro string) string { return job + "@" + distro }

// printPipeline prints a run's events like a matrix run, one prefix per job and distro,
// and returns the run as it ended.
func printPipeline(ctx context.Context, recv func() (*anvilv1.PipelineEvent, error)) (*anvilv1.PipelineRun, error) {
	var (
		out  *runPrinter
		last *anvilv1.PipelineRun
		stop = func() {}
	)
	defer func() { stop() }()
	for {
		ev, err := recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if out != nil {
				out.stopBoard()
			}
			if status.Code(err) == codes.Canceled || ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "anvil: cancelled, the clones are deleted")
				return nil, ExitError{Code: 130}
			}
			return nil, err
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.PipelineEvent_Run:
			last = e.Run
			if out == nil && e.Run.GetState() == "running" {
				var keys []string
				for _, j := range e.Run.GetJobs() {
					keys = append(keys, jobKey(j.GetName(), j.GetDistro()))
				}
				out = newRunPrinter(os.Stdout, os.Stderr, keys, false)
				out.label = "JOB"
				stop = out.live()
			}
		case *anvilv1.PipelineEvent_Progress:
			p := e.Progress
			if p.GetJob() == "" || out == nil {
				fmt.Fprintln(os.Stderr, "anvil:", p.GetMessage())
				continue
			}
			out.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Progress{Progress: &anvilv1.RunProgress{Distro: jobKey(p.GetJob(), p.GetDistro()), Message: p.GetMessage()}}})
		case *anvilv1.PipelineEvent_Output:
			if out != nil {
				o := e.Output
				out.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Output{Output: &anvilv1.RunOutput{Distro: jobKey(o.GetJob(), o.GetDistro()), Stream: o.GetStream(), Data: o.GetData()}}})
			}
		case *anvilv1.PipelineEvent_Job:
			j := e.Job
			if out == nil || (j.GetState() != "succeeded" && j.GetState() != "failed" && j.GetState() != "skipped") {
				continue
			}
			x := &anvilv1.RunExit{Distro: jobKey(j.GetName(), j.GetDistro()), ExitCode: j.GetExitCode(), DurationMs: j.GetDurationMs(), Error: j.GetError()}
			if j.GetState() == "skipped" {
				x.ExitCode, x.Error = -1, "skipped: "+j.GetError()
			}
			out.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Exit{Exit: x}})
		}
	}
	if out != nil {
		stop()
		out.finish()
	}
	if last != nil && last.GetError() != "" {
		fmt.Fprintln(os.Stderr, "anvil:", last.GetError())
	}
	if last != nil && last.GetApplied() != "" {
		fmt.Fprintln(os.Stderr, "anvil: apply:", last.GetApplied())
	}
	return last, nil
}

// downloadArtifacts extracts a run's collected files into dir.
func downloadArtifacts(ctx context.Context, c *client.Client, id, dir string) error {
	stream, err := c.Pipeline.Artifacts(ctx, &anvilv1.PipelineArtifactsRequest{Id: id})
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := os.MkdirAll(dir, 0o755)
		if err == nil {
			err = guestexec.Extract(pr, dir)
		}
		pr.CloseWithError(err)
		_, _ = io.Copy(io.Discard, pr)
		done <- err
	}()
	for {
		ch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			pw.CloseWithError(err)
			<-done
			return err
		}
		if _, err := pw.Write(ch.GetData()); err != nil {
			break
		}
	}
	pw.Close()
	if err := <-done; err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "anvil: collected files in", dir)
	return nil
}

func newPipelineRunsCommand(flags *globalFlags) *cobra.Command {
	var (
		output string
		limit  int32
	)
	cmd := &cobra.Command{
		Use:     "runs",
		Aliases: []string{"list", "ls"},
		Short:   "List pipeline runs, newest first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Pipeline.ListRuns(cmd.Context(), &anvilv1.PipelineListRunsRequest{Limit: limit})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(os.Stdout, reply)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tSOURCE\tCOMMIT\tSTATE\tJOBS\tSTARTED\tDURATION")
				for _, r := range reply.GetRuns() {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.GetId(), r.GetSource(), dash(shortCommit(r.GetCommit())), r.GetState(), jobCounts(r),
						time.Unix(r.GetStartedAtUnix(), 0).Format("01-02 15:04"), jobDuration(r.GetStartedAtUnix(), r.GetEndedAtUnix()))
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	cmd.Flags().Int32VarP(&limit, "limit", "n", 20, "show at most this many runs, 0 for all")
	return cmd
}

func shortCommit(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	return c
}

// jobCounts is the JOBS column: passed/all, with failures.
func jobCounts(r *anvilv1.PipelineRun) string {
	ok, failed := 0, 0
	for _, j := range r.GetJobs() {
		switch j.GetState() {
		case "succeeded":
			ok++
		case "failed":
			failed++
		}
	}
	s := fmt.Sprintf("%d/%d", ok, len(r.GetJobs()))
	if failed > 0 {
		s += fmt.Sprintf(" (%d failed)", failed)
	}
	return s
}

func newPipelineShowCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show a run and its jobs (a unique start of the id is enough)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				r, err := c.Pipeline.GetRun(cmd.Context(), &anvilv1.PipelineGetRunRequest{Id: args[0]})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(os.Stdout, r)
				}
				fmt.Printf("run %s: %s (%s, %s)\n", r.GetId(), r.GetState(), r.GetSource(), r.GetFile())
				if r.GetCommit() != "" {
					fmt.Println("commit", r.GetCommit())
				}
				if r.GetError() != "" {
					fmt.Println("error:", r.GetError())
				}
				if r.GetApplied() != "" {
					fmt.Println("apply:", r.GetApplied())
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "JOB\tDISTRO\tSTATE\tEXIT\tDURATION\tNOTE")
				for _, j := range r.GetJobs() {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", j.GetName(), j.GetDistro(), j.GetState(), j.GetExitCode(),
						(time.Duration(j.GetDurationMs()) * time.Millisecond).Round(10*time.Millisecond), oneLine(j.GetError(), 60))
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newPipelineLogsCommand(flags *globalFlags) *cobra.Command {
	var distro string
	cmd := &cobra.Command{
		Use:   "logs <run-id> [job]",
		Short: "Print the output of a run's jobs, or of one job",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &anvilv1.PipelineLogsRequest{Id: args[0], Distro: distro}
			if len(args) == 2 {
				req.Job = args[1]
			}
			return withClient(flags, func(c *client.Client) error {
				stream, err := c.Pipeline.Logs(cmd.Context(), req)
				if err != nil {
					return err
				}
				for {
					ch, err := stream.Recv()
					if errors.Is(err, io.EOF) {
						return nil
					}
					if err != nil {
						return err
					}
					_, _ = os.Stdout.Write(ch.GetData())
				}
			})
		},
	}
	cmd.Flags().StringVar(&distro, "distro", "", "only the job's run on this distro")
	return cmd
}

func newPipelineArtifactsCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "artifacts <run-id> [dir]",
		Short: "Download a run's collected files into dir (default .), as <job>/<distro>/...",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 2 {
				dir = args[1]
			}
			return withClient(flags, func(c *client.Client) error { return downloadArtifacts(cmd.Context(), c, args[0], dir) })
		},
	}
}

func newPipelineRerunCommand(flags *globalFlags) *cobra.Command {
	var artifacts string
	cmd := &cobra.Command{
		Use:   "rerun <run-id>",
		Short: "Run an earlier run's source again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return withClient(flags, func(c *client.Client) error {
				stream, err := c.Pipeline.Rerun(ctx, &anvilv1.PipelineRerunRequest{Id: args[0]})
				if err != nil {
					return err
				}
				run, err := printPipeline(ctx, stream.Recv)
				if err != nil {
					return err
				}
				return finishPipeline(cmd.Context(), c, run, artifacts)
			})
		},
	}
	cmd.Flags().StringVar(&artifacts, "artifacts", "", "download the run's collected files into this directory afterwards")
	return cmd
}

func newPipelineCancelCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Stop a running run and delete its clones",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				_, err := c.Pipeline.Cancel(cmd.Context(), &anvilv1.PipelineCancelRequest{Id: args[0]})
				return err
			})
		},
	}
}

func newPipelineWatchCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run a repository's pipeline on every new commit of a branch",
		Long: "anvild polls the branch with `git ls-remote` and, on a new commit, clones it and runs its pipeline. " +
			"anvild runs git as its own user, so a private repo needs that user's SSH key or credentials.",
	}
	var w anvilv1.PipelineWatch
	var interval time.Duration
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Watch a branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w.Name, w.IntervalSeconds = args[0], int64(interval/time.Second)
			return withClient(flags, func(c *client.Client) error {
				got, err := c.Pipeline.AddWatch(cmd.Context(), &anvilv1.PipelineWatchAddRequest{Watch: &w})
				if err != nil {
					return err
				}
				fmt.Printf("watching %s %s every %s\n", got.GetRepo(), got.GetBranch(), time.Duration(got.GetIntervalSeconds())*time.Second)
				return nil
			})
		},
	}
	add.Flags().StringVar(&w.Repo, "repo", "", "the repository: an https://, ssh://, git:// or user@host:path URL, or a local path owned by the caller")
	add.Flags().StringVar(&w.Branch, "branch", "", "the branch (default main)")
	add.Flags().StringVarP(&w.File, "file", "f", "", "the pipeline file in the repo (default anvil-ci.yaml)")
	add.Flags().DurationVar(&interval, "interval", 0, "how often to look for a new commit (default 1m, at least 10s)")
	add.Flags().BoolVar(&w.Paused, "paused", false, "add it paused")
	_ = add.MarkFlagRequired("repo")

	var output string
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List watches",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Pipeline.ListWatches(cmd.Context(), &anvilv1.PipelineWatchListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(os.Stdout, reply)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tREPO\tBRANCH\tLAST COMMIT\tLAST RUN\tSTATE")
				for _, w := range reply.GetWatches() {
					state := "watching"
					switch {
					case w.GetPaused():
						state = "paused"
					case w.GetLastError() != "":
						state = "error: " + oneLine(w.GetLastError(), 60)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", w.GetName(), w.GetRepo(), w.GetBranch(), dash(shortCommit(w.GetLastCommit())), dash(w.GetLastRun()), state)
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(list, &output)

	simple := func(use, short string, call func(ctx context.Context, c *client.Client, name string) error) *cobra.Command {
		return &cobra.Command{
			Use: use + " <name>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withClient(flags, func(c *client.Client) error { return call(cmd.Context(), c, args[0]) })
			},
		}
	}
	rm := simple("rm", "Stop watching; its runs stay", func(ctx context.Context, c *client.Client, name string) error {
		_, err := c.Pipeline.RemoveWatch(ctx, &anvilv1.PipelineWatchRemoveRequest{Name: name})
		return err
	})
	rm.Aliases = []string{"remove"}
	pause := simple("pause", "Stop polling the branch", func(ctx context.Context, c *client.Client, name string) error {
		_, err := c.Pipeline.SetWatchPaused(ctx, &anvilv1.PipelineWatchPauseRequest{Name: name, Paused: true})
		return err
	})
	resume := simple("resume", "Poll the branch again", func(ctx context.Context, c *client.Client, name string) error {
		_, err := c.Pipeline.SetWatchPaused(ctx, &anvilv1.PipelineWatchPauseRequest{Name: name})
		return err
	})
	trigger := simple("trigger", "Run the branch's pipeline now, in the background", func(ctx context.Context, c *client.Client, name string) error {
		if _, err := c.Pipeline.TriggerWatch(ctx, &anvilv1.PipelineWatchTriggerRequest{Name: name}); err != nil {
			return err
		}
		fmt.Println("started; see anvil pipeline runs")
		return nil
	})
	cmd.AddCommand(add, list, rm, pause, resume, trigger)
	return cmd
}
