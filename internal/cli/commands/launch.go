package commands

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// launchOptions holds the launch command's flags.
type launchOptions struct {
	kind          string
	name          string
	cpus          int32
	memoryMiB     int64
	diskGiB       int64
	cloudInitFile string
	cloudInitName string
	sshKeys       []string
	noStart       bool
	engine        string
	hypervisor    string
	envVars       []string
	volumes       []string
	publish       []string
	entrypoint    string
	intentName    string
	role          string
	fromDisk      string
	defaultUser   string
	wait          bool
	waitTimeout   time.Duration
	noGuestAgent  bool
	autostart     bool
	restart       string
	devices       []string
	ephemeral     bool
	eph           ephemeralFlags
}

func newLaunchCommand(flags *globalFlags) *cobra.Command {
	var o launchOptions
	cmd := &cobra.Command{
		Use:   "launch <image> [-- <command> [args...]]",
		Short: "Launch a new VM or container instance",
		Long: "Launch a new VM or container instance. For a container, anything after " +
			"`--` overrides the image's own command, same as `docker run image -- cmd args`.\n\n" +
			"With --ephemeral, `launch --ephemeral <distro> [path] -- <command>` restores a throwaway " +
			"clone of the distro's warm snapshot instead, uploads path into /work, runs the command, " +
			"deletes the clone and exits with the command's exit code (see anvil matrix, docs/ephemeral.md).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.ephemeral {
				return o.runEphemeral(cmd, flags, args)
			}
			req, err := o.request(args[0], args[1:])
			if err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				stream, err := c.Launch(cmd.Context(), req)
				if err != nil {
					return err
				}
				return streamLaunchProgress(cmd.OutOrStdout(), stream)
			})
		},
	}
	o.addFlags(cmd)
	return cmd
}

func (o *launchOptions) addFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&o.kind, "kind", "", `instance kind: "vm" or "container" (required)`)
	f.StringVar(&o.name, "name", "", "instance name (defaults to one derived from the image, e.g. ubuntu-24.04)")
	f.Int32Var(&o.cpus, "cpus", 1, "number of vCPUs (VM only)")
	f.Int64Var(&o.memoryMiB, "memory", 1024, "memory in MiB (VM only)")
	f.Int64Var(&o.diskGiB, "disk", 0, "disk size in GiB (VM only; 0 = catalog minimum)")
	f.StringVar(&o.cloudInitFile, "cloud-init", "", `path to a cloud-init user-data file, or "-" for stdin (VM only, ad hoc, not saved to the library)`)
	f.StringVar(&o.cloudInitName, "cloud-init-name", "", "name of a saved cloud-init config from the library (VM only, see anvil cloud-init)")
	f.StringArrayVar(&o.sshKeys, "ssh-key", nil, "additional SSH public key to authorize, on top of anvil's own managed key "+
		"(always authorized by default): a literal key, or a path to a .pub file; repeatable (VM only)")
	f.StringVar(&o.engine, "engine", "", `container engine: "docker" or "podman" (container only; defaults to docker, the only one implemented so far)`)
	f.StringVar(&o.hypervisor, "hypervisor", "", `VM hypervisor: "qemu" (default) or "firecracker" (VM only)`)
	f.StringArrayVarP(&o.envVars, "env", "e", nil, "environment variable KEY=VALUE (container only, repeatable)")
	f.StringArrayVarP(&o.volumes, "volume", "v", nil, "bind mount <host-path>:<container-path>[:ro] (container only, repeatable)")
	f.StringArrayVarP(&o.publish, "publish", "p", nil, "publish <host-port>:<guest-port>[/tcp|udp] (repeatable; a bridged --intent VM already has its own reachable address, so this only applies to a standalone VM or a container)")
	f.StringVar(&o.entrypoint, "entrypoint", "", "override the image's entrypoint (container only)")
	f.BoolVar(&o.noStart, "no-start", false, "create but don't start the instance")
	f.StringVar(&o.intentName, "intent", "", "join this instance to an intent (created automatically if it doesn't exist yet), see anvil intent")
	f.StringVar(&o.role, "role", "", "label this instance's role within --intent (defaults to its own name)")
	f.StringVar(&o.fromDisk, "from-disk", "", "use this already-prepared qcow2 file as the VM's own disk directly, skipping the image catalog and cloud-init entirely (kept for migrations from older anvil versions)")
	f.BoolVar(&o.wait, "wait", false, "block until the instance finished its first-boot setup (cloud-init for a VM); fails if cloud-init reports errors")
	f.DurationVar(&o.waitTimeout, "wait-timeout", 15*time.Minute, "give up on --wait after this long")
	f.BoolVar(&o.autostart, "autostart", false, "start the instance whenever anvild starts, unless it was stopped on purpose")
	f.StringVar(&o.restart, "restart", "", "restart policy when it stops on its own: no (default), on-failure[:N] or always")
	f.StringArrayVar(&o.devices, "device", nil, "pass a host device to the VM: usb:<vendor>:<product>, usb:<bus>-<port> or "+
		"pci:<domain>:<bus>:<slot>.<fn>, see anvil host devices (repeatable; qemu VMs only, which then can't be migrated or forked)")
	f.BoolVar(&o.noGuestAgent, "no-guest-agent", false, "don't install qemu-guest-agent through cloud-init (VM only); disables guest IP reporting and agent shutdown")
	f.BoolVar(&o.ephemeral, "ephemeral", false, "run a command on a throwaway clone of the distro's warm snapshot: <distro> [path] -- <command>")
	f.BoolVarP(&o.eph.tty, "tty", "t", false, "with --ephemeral: run the command on a terminal, with stdin")
	f.StringVar(&o.eph.arch, "arch", "", "with --ephemeral: image architecture (default: this host's)")
	o.eph.addRunFlags(cmd)
	o.eph.addSetupFlags(cmd)
	f.StringVar(&o.defaultUser, "default-user", "", "override the SSH login user normally read from the image catalog (VM only, kept for migrations from older anvil versions)")
	// Older migrate sources still pass these, newer ones use migrate-import.
	_ = f.MarkHidden("from-disk")
	_ = f.MarkHidden("default-user")
}

// runEphemeral runs `launch --ephemeral <distro> [path] -- <command>`.
func (o *launchOptions) runEphemeral(cmd *cobra.Command, flags *globalFlags, args []string) error {
	distro, path, command, err := splitRunArgs(cmd, args)
	if err != nil {
		return err
	}
	// The launch defaults are a regular VM's; a clone's come from its warm snapshot.
	if cmd.Flags().Changed("cpus") {
		o.eph.cpus = o.cpus
	}
	if cmd.Flags().Changed("memory") {
		o.eph.memoryMiB = o.memoryMiB
	}
	o.eph.env = o.envVars
	return runEphemeral(cmd, flags, []string{distro}, path, command, o.eph)
}

// request validates the flags and builds the LaunchRequest for imageRef.
func (o *launchOptions) request(imageRef string, command []string) (*anvilv1.LaunchRequest, error) {
	kind, err := o.validate(command)
	if err != nil {
		return nil, err
	}
	req := &anvilv1.LaunchRequest{
		Name:               o.name,
		Kind:               kind,
		NoStart:            o.noStart,
		IntentName:         o.intentName,
		Role:               o.role,
		Wait:               o.wait,
		WaitTimeoutSeconds: client.CeilSeconds(o.waitTimeout),
		Autostart:          o.autostart,
	}
	if o.restart != "" {
		req.RestartPolicy = &anvilv1.RestartPolicy{Mode: o.restart}
	}
	switch kind {
	case anvilv1.Kind_KIND_VM:
		req.Vm, err = o.vmSpec(imageRef)
	case anvilv1.Kind_KIND_CONTAINER:
		req.Container, err = o.containerSpec(imageRef, command)
	}
	if err != nil {
		return nil, err
	}
	return req, nil
}

// validate rejects flag combinations that don't fit the chosen kind.
func (o *launchOptions) validate(command []string) (anvilv1.Kind, error) {
	if o.eph.tty || o.eph.keepFailed || o.eph.logs != "" || o.eph.arch != "" || len(o.eph.collect) > 0 || o.eph.setup != "" || len(o.eph.keyFiles) > 0 {
		return 0, fmt.Errorf("--tty, --keep-failed, --logs, --collect, --setup, --key-files and --arch only apply with --ephemeral")
	}
	var kind anvilv1.Kind
	switch o.kind {
	case "vm":
		kind = anvilv1.Kind_KIND_VM
	case "container":
		kind = anvilv1.Kind_KIND_CONTAINER
	default:
		return 0, fmt.Errorf("--kind must be \"vm\" or \"container\" (or use --ephemeral)")
	}
	isVM := kind == anvilv1.Kind_KIND_VM
	switch {
	case isVM && len(command) > 0:
		return 0, fmt.Errorf("a trailing command only makes sense for --kind container")
	case o.role != "" && o.intentName == "":
		return 0, fmt.Errorf("--role only makes sense with --intent")
	case isVM && o.intentName != "" && len(o.publish) > 0:
		return 0, fmt.Errorf("--publish doesn't apply to a VM joining --intent: it gets its own directly-reachable address on the shared network instead of a SLIRP-forwarded port")
	case o.fromDisk != "" && !isVM:
		return 0, fmt.Errorf("--from-disk only applies to --kind vm")
	case o.fromDisk != "" && (o.cloudInitFile != "" || o.cloudInitName != ""):
		return 0, fmt.Errorf("--from-disk skips cloud-init entirely (the disk already has everything from its original first boot), --cloud-init/--cloud-init-name don't apply")
	case o.defaultUser != "" && !isVM:
		return 0, fmt.Errorf("--default-user only applies to --kind vm")
	case o.hypervisor != "" && !isVM:
		return 0, fmt.Errorf("--hypervisor only applies to --kind vm")
	case len(o.devices) > 0 && (!isVM || o.hypervisor == "firecracker"):
		return 0, fmt.Errorf("--device only applies to --kind vm with the qemu hypervisor")
	case o.noGuestAgent && !isVM:
		return 0, fmt.Errorf("--no-guest-agent only applies to --kind vm")
	case o.wait && o.noStart:
		return 0, fmt.Errorf("--wait and --no-start are mutually exclusive")
	case o.cloudInitFile != "" && o.cloudInitName != "":
		return 0, fmt.Errorf("--cloud-init and --cloud-init-name are mutually exclusive")
	}
	return kind, nil
}

func (o *launchOptions) vmSpec(imageRef string) (*anvilv1.VMSpec, error) {
	hypervisor, err := parseVMHypervisor(o.hypervisor)
	if err != nil {
		return nil, err
	}
	var cloudInitData string
	if o.cloudInitFile != "" {
		if cloudInitData, err = readCloudInitFile(o.cloudInitFile); err != nil {
			return nil, err
		}
	}
	keys, err := client.ResolveSSHKeys(o.sshKeys)
	if err != nil {
		return nil, err
	}
	ports, err := client.ParsePorts(o.publish)
	if err != nil {
		return nil, err
	}
	devs, err := client.ParseDevices(o.devices)
	if err != nil {
		return nil, err
	}
	return &anvilv1.VMSpec{
		ImageRef:          imageRef,
		Cpus:              o.cpus,
		MemoryMib:         o.memoryMiB,
		DiskGib:           o.diskGiB,
		CloudInitUserData: cloudInitData,
		CloudInitName:     o.cloudInitName,
		SshPublicKeys:     keys,
		Ports:             ports,
		SourceDiskPath:    o.fromDisk,
		DefaultUser:       o.defaultUser,
		NoGuestAgent:      o.noGuestAgent,
		Hypervisor:        hypervisor,
		Devices:           devs,
	}, nil
}

func (o *launchOptions) containerSpec(imageRef string, command []string) (*anvilv1.ContainerSpec, error) {
	engine, err := parseContainerEngine(o.engine)
	if err != nil {
		return nil, err
	}
	env, err := client.ParseEnv(o.envVars)
	if err != nil {
		return nil, err
	}
	vols, err := client.ParseVolumes(o.volumes)
	if err != nil {
		return nil, err
	}
	ports, err := client.ParsePorts(o.publish)
	if err != nil {
		return nil, err
	}
	var entrypoint []string
	if o.entrypoint != "" {
		entrypoint = []string{o.entrypoint}
	}
	return &anvilv1.ContainerSpec{
		ImageRef:   imageRef,
		Env:        env,
		Entrypoint: entrypoint,
		Cmd:        command,
		Volumes:    vols,
		Ports:      ports,
		Engine:     engine,
	}, nil
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

func parseVMHypervisor(s string) (anvilv1.VmHypervisor, error) {
	switch s {
	case "":
		return anvilv1.VmHypervisor_VM_HYPERVISOR_UNSPECIFIED, nil
	case "qemu":
		return anvilv1.VmHypervisor_VM_HYPERVISOR_QEMU, nil
	case "firecracker":
		return anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER, nil
	default:
		return 0, fmt.Errorf(`--hypervisor must be "qemu" or "firecracker"`)
	}
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
