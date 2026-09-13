package commands

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newLaunchCommand(flags *globalFlags) *cobra.Command {
	var (
		kindStr       string
		name          string
		cpus          int32
		memoryMiB     int64
		diskGiB       int64
		cloudInitFile string
		cloudInitName string
		sshKeys       []string
		noStart       bool
		engineStr     string
		envVars       []string
		volumes       []string
		publish       []string
		entrypoint    string
		intentName    string
		role          string
		fromDisk      string
		defaultUser   string
	)

	cmd := &cobra.Command{
		Use:   "launch <image> [-- <command> [args...]]",
		Short: "Launch a new VM or container instance",
		Long: "Launch a new VM or container instance. For a container, anything after " +
			"`--` overrides the image's own command, same as `docker run image -- cmd args`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			imageRef := args[0]
			command := args[1:]

			var kind anvilv1.Kind
			switch kindStr {
			case "vm":
				kind = anvilv1.Kind_KIND_VM
			case "container":
				kind = anvilv1.Kind_KIND_CONTAINER
			default:
				return fmt.Errorf("--kind must be \"vm\" or \"container\"")
			}
			if kind == anvilv1.Kind_KIND_VM && len(command) > 0 {
				return fmt.Errorf("a trailing command only makes sense for --kind container")
			}

			if name == "" {
				name = imageRef
			}

			if role != "" && intentName == "" {
				return fmt.Errorf("--role only makes sense with --intent")
			}
			if kind == anvilv1.Kind_KIND_VM && intentName != "" && len(publish) > 0 {
				return fmt.Errorf("--publish doesn't apply to a VM joining --intent: it gets its own directly-reachable address on the shared network instead of a SLIRP-forwarded port")
			}
			if fromDisk != "" {
				if kind != anvilv1.Kind_KIND_VM {
					return fmt.Errorf("--from-disk only applies to --kind vm")
				}
				if cloudInitFile != "" || cloudInitName != "" {
					return fmt.Errorf("--from-disk skips cloud-init entirely (the disk already has everything from its original first boot), --cloud-init/--cloud-init-name don't apply")
				}
			}
			if defaultUser != "" && kind != anvilv1.Kind_KIND_VM {
				return fmt.Errorf("--default-user only applies to --kind vm")
			}

			req := &anvilv1.LaunchRequest{
				Name:       name,
				Kind:       kind,
				NoStart:    noStart,
				IntentName: intentName,
				Role:       role,
			}

			switch kind {
			case anvilv1.Kind_KIND_VM:
				if cloudInitFile != "" && cloudInitName != "" {
					return fmt.Errorf("--cloud-init and --cloud-init-name are mutually exclusive")
				}
				var cloudInitData string
				if cloudInitFile != "" {
					data, err := readCloudInitFile(cloudInitFile)
					if err != nil {
						return err
					}
					cloudInitData = data
				}
				resolvedKeys, err := resolveSSHKeys(sshKeys)
				if err != nil {
					return err
				}
				ports, err := parsePublish(publish)
				if err != nil {
					return err
				}
				req.Vm = &anvilv1.VMSpec{
					ImageRef:          imageRef,
					Cpus:              cpus,
					MemoryMib:         memoryMiB,
					DiskGib:           diskGiB,
					CloudInitUserData: cloudInitData,
					CloudInitName:     cloudInitName,
					SshPublicKeys:     resolvedKeys,
					Ports:             ports,
					SourceDiskPath:    fromDisk,
					DefaultUser:       defaultUser,
				}

			case anvilv1.Kind_KIND_CONTAINER:
				engine, err := parseContainerEngine(engineStr)
				if err != nil {
					return err
				}
				env, err := parseEnvVars(envVars)
				if err != nil {
					return err
				}
				vols, err := parseVolumes(volumes)
				if err != nil {
					return err
				}
				ports, err := parsePublish(publish)
				if err != nil {
					return err
				}
				var entrypointSlice []string
				if entrypoint != "" {
					entrypointSlice = []string{entrypoint}
				}
				req.Container = &anvilv1.ContainerSpec{
					ImageRef:   imageRef,
					Env:        env,
					Entrypoint: entrypointSlice,
					Cmd:        command,
					Volumes:    vols,
					Ports:      ports,
					Engine:     engine,
				}
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			stream, err := c.Launch(cmd.Context(), req)
			if err != nil {
				return err
			}
			return streamLaunchProgress(cmd.OutOrStdout(), stream)
		},
	}

	cmd.Flags().StringVar(&kindStr, "kind", "", `instance kind: "vm" or "container" (required)`)
	_ = cmd.MarkFlagRequired("kind")
	cmd.Flags().StringVar(&name, "name", "", "instance name (defaults to the image reference)")
	cmd.Flags().Int32Var(&cpus, "cpus", 1, "number of vCPUs (VM only)")
	cmd.Flags().Int64Var(&memoryMiB, "memory", 1024, "memory in MiB (VM only)")
	cmd.Flags().Int64Var(&diskGiB, "disk", 0, "disk size in GiB (VM only; 0 = catalog minimum)")
	cmd.Flags().StringVar(&cloudInitFile, "cloud-init", "", `path to a cloud-init user-data file, or "-" for stdin (VM only, ad hoc, not saved to the library)`)
	cmd.Flags().StringVar(&cloudInitName, "cloud-init-name", "", "name of a saved cloud-init config from the library (VM only, see `anvil cloud-init`)")
	cmd.Flags().StringArrayVar(&sshKeys, "ssh-key", nil, "additional SSH public key to authorize, on top of anvil's own managed key "+
		"(always authorized by default): a literal key, or a path to a .pub file; repeatable (VM only)")
	cmd.Flags().StringVar(&engineStr, "engine", "", `container engine: "docker" or "podman" (container only; defaults to docker, the only one implemented so far)`)
	cmd.Flags().StringArrayVarP(&envVars, "env", "e", nil, "environment variable KEY=VALUE (container only, repeatable)")
	cmd.Flags().StringArrayVarP(&volumes, "volume", "v", nil, "bind mount <host-path>:<container-path>[:ro] (container only, repeatable)")
	cmd.Flags().StringArrayVarP(&publish, "publish", "p", nil, "publish <host-port>:<guest-port>[/tcp|udp] (repeatable; a bridged --intent VM already has its own reachable address, so this only applies to a standalone VM or a container)")
	cmd.Flags().StringVar(&entrypoint, "entrypoint", "", "override the image's entrypoint (container only)")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "create but don't start the instance")
	cmd.Flags().StringVar(&intentName, "intent", "", "join this instance to an intent (created automatically if it doesn't exist yet), see `anvil intent`")
	cmd.Flags().StringVar(&role, "role", "", "label this instance's role within --intent (defaults to its own name)")
	cmd.Flags().StringVar(&fromDisk, "from-disk", "", "use this already-prepared qcow2 file as the VM's own disk directly, skipping the image catalog and cloud-init entirely (internal, used by `anvil migrate`)")
	cmd.Flags().StringVar(&defaultUser, "default-user", "", "override the SSH login user normally read from the image catalog (VM only, mainly for internal use by `anvil migrate`)")
	return cmd
}

// launchProgressStream is the subset of InstanceService_LaunchClient
// streamLaunchProgress needs — just Recv, so `anvil intent create`/`add`
// (internal/cli/commands/intent.go) can reuse this against the same real
// stream type without this function needing to know about intents at all.
type launchProgressStream interface {
	Recv() (*anvilv1.LaunchProgress, error)
}

// streamLaunchProgress prints each LaunchProgress event as it arrives —
// shared by `anvil launch` and `anvil intent create`/`add`, since both
// ultimately drive the same streaming Launch RPC and want identical
// output.
//
// When out is a real terminal, consecutive status updates that share the
// same "key" (see progressKey) redraw in place with a carriage return
// instead of each becoming its own line — a real download/pull produces a
// status update every ~500ms (see the backends' own throttling), and one
// line per tick was, in filippo's own words, awful to watch. Piped/
// redirected output (out isn't a terminal — e.g. into a file or `less`)
// falls back to one line per update, since carriage-return redraws only
// make sense on a real screen.
//
// This isn't a full multi-line progress renderer: a Docker pull can have
// several layers downloading concurrently, and their status lines
// legitimately interleave (different keys arriving back to back). Each
// key change ends the previous in-place line and starts a new one, so a
// multi-layer pull still prints more than one line, just far fewer than
// today's one-line-per-tick — a real multi-line redraw (tracking N
// concurrent lines, moving the cursor up to update each in place) would
// need real terminal-size/cursor handling this doesn't attempt.
func streamLaunchProgress(out io.Writer, stream launchProgressStream) error {
	term := isTerminalWriter(out)
	var lastKey string
	haveLine := false

	printStatus := func(status string) {
		if !term {
			fmt.Fprintln(out, status)
			return
		}
		key := progressKey(status)
		if haveLine && key == lastKey {
			fmt.Fprintf(out, "\r\033[K%s", status) // redraw this same line in place
		} else {
			if haveLine {
				fmt.Fprintln(out) // finalize the previous in-place line
			}
			fmt.Fprint(out, status) // no trailing newline yet, may still be overwritten
		}
		lastKey = key
		haveLine = true
	}
	finalizeLine := func() {
		if term && haveLine {
			fmt.Fprintln(out)
			haveLine = false
		}
	}

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			finalizeLine()
			return nil
		}
		if err != nil {
			finalizeLine()
			return err
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.LaunchProgress_Status:
			printStatus(e.Status)
		case *anvilv1.LaunchProgress_Error:
			finalizeLine()
			return fmt.Errorf("%s", e.Error)
		case *anvilv1.LaunchProgress_Instance:
			finalizeLine()
			fmt.Fprintf(out, "Launched %q (%s)\n", e.Instance.GetName(), e.Instance.GetId())
		}
	}
}

// progressKey groups status updates that should redraw the same line
// rather than each getting their own: everything before the first ": ",
// or the whole string if there isn't one. Matches both this project's own
// status shapes: "downloading ubuntu-24.04: 43% (...)" groups by image
// name, "abc123: Downloading [...]" (a Docker pull's per-layer status)
// groups by layer ID.
func progressKey(status string) string {
	if idx := strings.Index(status, ": "); idx != -1 {
		return status[:idx]
	}
	return status
}

// isTerminalWriter reports whether w is a real terminal rather than a
// pipe/file redirect — same os.ModeCharDevice trick as
// container_exec.go's isStdinTerminal, just checking stdout-shaped output
// instead of stdin. Deliberately checks the concrete *os.File rather than
// trusting an io.Writer's type alone: cobra's cmd.OutOrStdout() returns
// os.Stdout by default but can be swapped for a plain buffer (tests,
// programmatic use), which should never trigger escape-code redraws.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func parseContainerEngine(s string) (anvilv1.ContainerEngine, error) {
	switch s {
	case "":
		return anvilv1.ContainerEngine_CONTAINER_ENGINE_UNSPECIFIED, nil
	case "docker":
		return anvilv1.ContainerEngine_CONTAINER_ENGINE_DOCKER, nil
	case "podman":
		return anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN, nil
	default:
		return 0, fmt.Errorf(`--engine must be "docker" or "podman"`)
	}
}

func parseEnvVars(vars []string) (map[string]string, error) {
	if len(vars) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(vars))
	for _, v := range vars {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("--env %q must be in KEY=VALUE form", v)
		}
		env[k] = val
	}
	return env, nil
}

func parseVolumes(vols []string) ([]*anvilv1.VolumeMount, error) {
	var out []*anvilv1.VolumeMount
	for _, v := range vols {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf(`--volume %q must be "<host-path>:<container-path>[:ro]"`, v)
		}
		readOnly := false
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return nil, fmt.Errorf(`--volume %q: third part must be "ro"`, v)
			}
			readOnly = true
		}
		out = append(out, &anvilv1.VolumeMount{
			HostPath:      parts[0],
			ContainerPath: parts[1],
			ReadOnly:      readOnly,
		})
	}
	return out, nil
}

func parsePublish(pubs []string) ([]*anvilv1.PortMapping, error) {
	var out []*anvilv1.PortMapping
	for _, p := range pubs {
		spec := p
		protocol := "tcp"
		if host, proto, ok := strings.Cut(p, "/"); ok {
			spec = host
			protocol = proto
		}
		hostStr, guestStr, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf(`--publish %q must be "<host-port>:<guest-port>[/tcp|udp]"`, p)
		}
		hostPort, err := strconv.Atoi(hostStr)
		if err != nil {
			return nil, fmt.Errorf("--publish %q: invalid host port: %w", p, err)
		}
		guestPort, err := strconv.Atoi(guestStr)
		if err != nil {
			return nil, fmt.Errorf("--publish %q: invalid guest port: %w", p, err)
		}
		out = append(out, &anvilv1.PortMapping{
			HostPort:  int32(hostPort),
			GuestPort: int32(guestPort),
			Protocol:  protocol,
		})
	}
	return out, nil
}

func readCloudInitFile(path string) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		return string(data), err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading cloud-init file %s: %w", path, err)
	}
	return string(data), nil
}

// resolveSSHKeys always includes anvil's own managed public key (generated
// on first use if it doesn't exist yet, see ensureDefaultAnvilKey in
// ssh.go) — this is what makes `anvil shell`/`exec`/`transfer` work by
// default with zero flags, on any instance, without depending on whatever
// personal keys a given user happens to have in ~/.ssh. --ssh-key values
// (literal keys or paths to .pub files) are additive on top of that, for
// anyone who also wants to authorize their own personal key.
func resolveSSHKeys(explicit []string) ([]string, error) {
	anvilKeyPath, err := ensureDefaultAnvilKey()
	if err != nil {
		return nil, err
	}
	anvilPub, err := os.ReadFile(anvilKeyPath + ".pub")
	if err != nil {
		return nil, fmt.Errorf("reading anvil's default public key: %w", err)
	}
	keys := []string{strings.TrimSpace(string(anvilPub))}

	for _, k := range explicit {
		resolved, err := resolveSSHKey(k)
		if err != nil {
			return nil, err
		}
		keys = append(keys, resolved)
	}
	return keys, nil
}

func resolveSSHKey(k string) (string, error) {
	// A literal public key, e.g. "ssh-ed25519 AAAA... comment", vs. a path
	// to a .pub file — distinguished by the well-known key-type prefixes,
	// since a real file path never starts with one of these.
	for _, prefix := range []string{"ssh-", "ecdsa-", "sk-"} {
		if strings.HasPrefix(k, prefix) {
			return k, nil
		}
	}
	data, err := os.ReadFile(k)
	if err != nil {
		return "", fmt.Errorf("reading SSH key file %s: %w", k, err)
	}
	return strings.TrimSpace(string(data)), nil
}
