package commands

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/migrate/payload"
)

// newMigrateImportCommand is `anvil migrate-import`: plumbing, not meant
// to be run by hand. This is the "target's own local anvil CLI" the
// plan's Migration section describes the source daemon driving over SSH
// (see internal/migrate.Manager.Migrate) — it reads a payload.Payload as
// JSON from stdin (never as command-line arguments: a fixed,
// argument-free remote command sidesteps needing to shell-quote arbitrary
// spec content), relaunches it via a normal Launch call against this
// host's own local anvild, and prints a final MIGRATE_OK/MIGRATE_FAIL
// line the source daemon parses to know whether it succeeded — every
// other line is just forwarded progress, safe to ignore.
func newMigrateImportCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:    "migrate-import",
		Short:  "Internal: relaunch a migrated instance from a JSON payload on stdin",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				fmt.Fprintf(out, "MIGRATE_FAIL reading payload: %v\n", err)
				return err
			}
			var pl payload.Payload
			if err := json.Unmarshal(data, &pl); err != nil {
				fmt.Fprintf(out, "MIGRATE_FAIL invalid payload: %v\n", err)
				return err
			}

			req, err := launchRequestFromPayload(pl)
			if err != nil {
				fmt.Fprintf(out, "MIGRATE_FAIL %v\n", err)
				return err
			}

			c, err := dial(flags)
			if err != nil {
				fmt.Fprintf(out, "MIGRATE_FAIL dialing local anvild: %v\n", err)
				return err
			}
			defer c.Close()

			stream, err := c.Launch(cmd.Context(), req)
			if err != nil {
				fmt.Fprintf(out, "MIGRATE_FAIL %v\n", err)
				return err
			}
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
					fmt.Fprintln(out, "MIGRATE_FAIL launch stream ended without a result")
					return fmt.Errorf("launch stream ended without a result")
				}
				if err != nil {
					fmt.Fprintf(out, "MIGRATE_FAIL %v\n", err)
					return err
				}
				switch e := ev.GetEvent().(type) {
				case *anvilv1.LaunchProgress_Status:
					fmt.Fprintln(out, e.Status)
				case *anvilv1.LaunchProgress_Error:
					fmt.Fprintf(out, "MIGRATE_FAIL %s\n", e.Error)
					return fmt.Errorf("%s", e.Error)
				case *anvilv1.LaunchProgress_Instance:
					fmt.Fprintf(out, "MIGRATE_OK %s\n", e.Instance.GetId())
					return nil
				}
			}
		},
	}
}

// launchRequestFromPayload translates pl into the LaunchRequest that
// relaunches it on this host.
func launchRequestFromPayload(pl payload.Payload) (*anvilv1.LaunchRequest, error) {
	req := &anvilv1.LaunchRequest{Name: pl.Name}

	switch pl.Kind {
	case "vm":
		if pl.VM == nil {
			return nil, fmt.Errorf("payload has no VM spec")
		}
		req.Kind = anvilv1.Kind_KIND_VM
		req.Vm = &anvilv1.VMSpec{
			ImageRef:       pl.VM.ImageRef,
			Arch:           pl.VM.Arch,
			Cpus:           pl.VM.CPUs,
			MemoryMib:      pl.VM.MemoryMiB,
			DefaultUser:    pl.VM.DefaultUser,
			SourceDiskPath: pl.VM.RemoteDiskPath,
		}

	case "container":
		if pl.Container == nil {
			return nil, fmt.Errorf("payload has no container spec")
		}
		engine, err := parseContainerEngine(pl.Container.Engine)
		if err != nil {
			return nil, err
		}
		req.Kind = anvilv1.Kind_KIND_CONTAINER
		cs := &anvilv1.ContainerSpec{
			ImageRef:    pl.Container.ImageRef,
			Env:         pl.Container.Env,
			Entrypoint:  pl.Container.Entrypoint,
			Cmd:         pl.Container.Cmd,
			NetworkMode: pl.Container.NetworkMode,
			Engine:      engine,
		}
		for _, v := range pl.Container.Volumes {
			cs.Volumes = append(cs.Volumes, &anvilv1.VolumeMount{
				HostPath: v.HostPath, ContainerPath: v.ContainerPath, ReadOnly: v.ReadOnly,
			})
		}
		for _, p := range pl.Container.Ports {
			cs.Ports = append(cs.Ports, &anvilv1.PortMapping{
				HostPort: int32(p.HostPort), GuestPort: int32(p.GuestPort), Protocol: p.Protocol,
			})
		}
		req.Container = cs

	default:
		return nil, fmt.Errorf("unknown instance kind %q in payload", pl.Kind)
	}

	return req, nil
}
