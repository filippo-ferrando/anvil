package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

func newMigrateCommand(flags *globalFlags) *cobra.Command {
	var (
		to         string
		copyMode   bool
		destName   string
		dryRun     bool
		bestEffort bool
	)
	cmd := &cobra.Command{
		Use:   "migrate <name|intent> --to <alias|user@host[:port]>",
		Short: "Move a single instance, or a whole intent, to a different anvil host",
		Long: "Move a single instance, or a whole intent as one group, to a different anvil " +
			"host over SSH (see `anvil host`). name is looked up as a single instance first, " +
			"a whole intent second. Default mode is destructive: copies to the target, " +
			"confirms it actually landed, then deletes the source. It never deletes first. " +
			"`--copy` skips that last step. For an intent, default mode is also " +
			"all-or-nothing: the first member that fails to migrate rolls back whatever " +
			"already landed on the target and leaves every source member untouched; " +
			"`--best-effort` keeps whatever succeeds instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return fmt.Errorf("--to is required")
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			// Authorize the target's guest-access key on the source VM before migrating,
			// since a migrated disk skips cloud-init on relaunch.
			if !dryRun {
				if err := injectGuestKeys(cmd.Context(), c, args[0], to); err != nil {
					return fmt.Errorf("preparing guest SSH access on the target: %w", err)
				}
			}

			stream, err := c.Migrate.Migrate(cmd.Context(), &anvilv1.MigrateRequest{
				Name:       args[0],
				To:         to,
				Copy:       copyMode,
				DestName:   destName,
				DryRun:     dryRun,
				BestEffort: bestEffort,
			})
			if err != nil {
				return err
			}

			failed := false
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
					if failed {
						return fmt.Errorf("migration finished with failures, see above")
					}
					return nil
				}
				if err != nil {
					return err
				}
				switch e := ev.GetEvent().(type) {
				case *anvilv1.MigrateProgress_Status:
					fmt.Fprintln(cmd.OutOrStdout(), e.Status)
				case *anvilv1.MigrateProgress_Error:
					return fmt.Errorf("%s", e.Error)
				case *anvilv1.MigrateProgress_Done:
					fmt.Fprintf(cmd.OutOrStdout(), "Migrated: new instance %s on %s\n", e.Done, to)
				case *anvilv1.MigrateProgress_MemberDone:
					mr := e.MemberDone
					if mr.GetError() != "" {
						failed = true
						fmt.Fprintf(cmd.OutOrStdout(), "  %s: FAILED: %s\n", mr.GetRole(), mr.GetError())
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s: migrated as %s\n", mr.GetRole(), mr.GetNewId())
					}
				case *anvilv1.MigrateProgress_IntentDone:
					done := e.IntentDone
					if done.GetRolledBack() {
						failed = true
						fmt.Fprintf(cmd.OutOrStdout(), "Rolled back: %q failed to migrate as a group, target cleaned up, source untouched\n", done.GetIntentName())
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "Migrated: intent %q to %s\n", done.GetIntentName(), to)
					}
				}
			}
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "known host alias, or a literal user@host[:port] (required)")
	cmd.Flags().BoolVar(&copyMode, "copy", false, "leave the source instance in place instead of deleting it after a confirmed successful migration")
	cmd.Flags().StringVar(&destName, "dest-name", "", "rename the instance on the target (single instance only, defaults to its current name)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check connectivity and report the plan, transfer nothing")
	cmd.Flags().BoolVar(&bestEffort, "best-effort", false, "intent only: keep whatever members succeed instead of rolling back the whole group on any failure")
	return cmd
}

// injectGuestKeys authorizes the target host's guest-access key in every
// running VM about to be migrated (name: a single instance, or a whole intent).
func injectGuestKeys(ctx context.Context, c *client.Client, name, to string) error {
	vms, err := resolveMigratingVMs(ctx, c, name)
	if err != nil {
		return err
	}
	if len(vms) == 0 {
		return nil
	}

	reply, err := c.Migrate.GuestKey(ctx, &anvilv1.MigrateGuestKeyRequest{To: to})
	if err != nil {
		return fmt.Errorf("fetching the target's guest-access key: %w", err)
	}
	key := reply.GetPublicKey()

	identity, err := resolveIdentity("")
	if err != nil {
		return err
	}

	// Appends the key (piped via stdin) to authorized_keys unless it's already present.
	const remoteCommand = `mkdir -p ~/.ssh && chmod 700 ~/.ssh && touch ~/.ssh/authorized_keys && ` +
		`key=$(cat) && (grep -qxF "$key" ~/.ssh/authorized_keys || echo "$key" >> ~/.ssh/authorized_keys) && ` +
		`chmod 600 ~/.ssh/authorized_keys`

	for _, inst := range vms {
		if inst.GetState() != anvilv1.State_STATE_RUNNING {
			fmt.Fprintf(os.Stderr, "anvil: %s isn't running, skipping guest-key setup. Add the target's key to it manually if needed\n", inst.GetName())
			continue
		}
		target, err := resolveSSHTarget(inst, "")
		if err != nil {
			return fmt.Errorf("%s: %w", inst.GetName(), err)
		}
		if err := runSSHWithStdin(target, identity, []string{remoteCommand}, strings.NewReader(key)); err != nil {
			return fmt.Errorf("authorizing the target's key on %s: %w", inst.GetName(), err)
		}
	}
	return nil
}

// resolveMigratingVMs looks up name as a single instance first, then as an intent,
// and returns every VM involved. Container members are skipped.
func resolveMigratingVMs(ctx context.Context, c *client.Client, name string) ([]*anvilv1.Instance, error) {
	infoReply, err := c.Info(ctx, &anvilv1.InfoRequest{Names: []string{name}})
	switch {
	case err == nil && len(infoReply.GetInstances()) > 0:
		inst := infoReply.GetInstances()[0]
		if inst.GetVm() == nil {
			return nil, nil
		}
		return []*anvilv1.Instance{inst}, nil
	case err != nil && status.Code(err) != codes.NotFound:
		return nil, fmt.Errorf("migrate: looking up %q: %w", name, err)
	}

	intentReply, err := c.Intent.Info(ctx, &anvilv1.IntentInfoRequest{Name: name})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("no instance or intent named %q", name)
		}
		return nil, fmt.Errorf("migrate: looking up %q as an intent: %w", name, err)
	}
	memberIDs := make(map[string]bool)
	for _, mem := range intentReply.GetIntent().GetMembers() {
		if mem.GetKind() == anvilv1.Kind_KIND_VM {
			memberIDs[mem.GetInstanceId()] = true
		}
	}
	if len(memberIDs) == 0 {
		return nil, nil
	}

	// List all instances and match by ID, since Info only resolves by name.
	listReply, err := c.List(ctx, &anvilv1.ListRequest{})
	if err != nil {
		return nil, err
	}
	var vms []*anvilv1.Instance
	for _, inst := range listReply.GetInstances() {
		if memberIDs[inst.GetId()] {
			vms = append(vms, inst)
		}
	}
	return vms, nil
}
