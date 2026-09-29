// Package daemon implements the anvil gRPC services against their
// underlying managers and stores.
package daemon

import (
	"fmt"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
)

func kindFromPB(k anvilv1.Kind) instance.Kind {
	switch k {
	case anvilv1.Kind_KIND_VM:
		return instance.KindVM
	case anvilv1.Kind_KIND_CONTAINER:
		return instance.KindContainer
	default:
		return ""
	}
}

func kindToPB(k instance.Kind) anvilv1.Kind {
	switch k {
	case instance.KindVM:
		return anvilv1.Kind_KIND_VM
	case instance.KindContainer:
		return anvilv1.Kind_KIND_CONTAINER
	default:
		return anvilv1.Kind_KIND_UNSPECIFIED
	}
}

func stateToPB(s instance.State) anvilv1.State {
	switch s {
	case instance.StateStopped:
		return anvilv1.State_STATE_STOPPED
	case instance.StateStarting:
		return anvilv1.State_STATE_STARTING
	case instance.StateRunning:
		return anvilv1.State_STATE_RUNNING
	case instance.StateStopping:
		return anvilv1.State_STATE_STOPPING
	case instance.StateDeleting:
		return anvilv1.State_STATE_DELETING
	case instance.StateDeleted:
		return anvilv1.State_STATE_DELETED
	case instance.StateError:
		return anvilv1.State_STATE_ERROR
	default:
		return anvilv1.State_STATE_UNSPECIFIED
	}
}

func vmSpecFromPB(v *anvilv1.VMSpec) *instance.VMSpec {
	if v == nil {
		return nil
	}
	spec := &instance.VMSpec{
		ImageRef:             v.GetImageRef(),
		Arch:                 v.GetArch(),
		CPUs:                 int(v.GetCpus()),
		MemoryMiB:            v.GetMemoryMib(),
		DiskGiB:              v.GetDiskGib(),
		CloudInitUserData:    v.GetCloudInitUserData(),
		CloudInitName:        v.GetCloudInitName(),
		NetworkMode:          v.GetNetworkMode(),
		SSHPublicKeys:        v.GetSshPublicKeys(),
		DefaultUser:          v.GetDefaultUser(),
		SourceDiskPath:       v.GetSourceDiskPath(),
		SourceDiskBaseSHA256: v.GetSourceDiskBaseSha256(),
		NoGuestAgent:         v.GetNoGuestAgent(),
	}
	for _, p := range v.GetPorts() {
		spec.Ports = append(spec.Ports, instance.PortMapping{
			HostPort:  int(p.GetHostPort()),
			GuestPort: int(p.GetGuestPort()),
			Protocol:  p.GetProtocol(),
		})
	}
	return spec
}

func vmSpecToPB(v *instance.VMSpec) *anvilv1.VMSpec {
	if v == nil {
		return nil
	}
	pb := &anvilv1.VMSpec{
		ImageRef:             v.ImageRef,
		Arch:                 v.Arch,
		Cpus:                 int32(v.CPUs),
		MemoryMib:            v.MemoryMiB,
		DiskGib:              v.DiskGiB,
		CloudInitUserData:    v.CloudInitUserData,
		CloudInitName:        v.CloudInitName,
		NetworkMode:          v.NetworkMode,
		SshPublicKeys:        v.SSHPublicKeys,
		SshPort:              int32(v.SSHPort),
		DefaultUser:          v.DefaultUser,
		BridgeInterface:      v.BridgeInterface,
		StaticIp:             v.StaticIP,
		Gateway:              v.Gateway,
		ExtraHosts:           v.ExtraHosts,
		SourceDiskPath:       v.SourceDiskPath,
		SourceDiskBaseSha256: v.SourceDiskBaseSHA256,
		NoGuestAgent:         v.NoGuestAgent,
		SnapshotSchedule:     snapshotScheduleToPB(v.SnapshotSchedule),
	}
	for _, m := range v.Mounts {
		pb.Mounts = append(pb.Mounts, &anvilv1.Mount{
			HostPath:  m.HostPath,
			GuestPath: m.GuestPath,
			Tag:       m.Tag,
			ReadOnly:  m.ReadOnly,
		})
	}
	for _, p := range v.Ports {
		pb.Ports = append(pb.Ports, &anvilv1.PortMapping{
			HostPort:  int32(p.HostPort),
			GuestPort: int32(p.GuestPort),
			Protocol:  p.Protocol,
		})
	}
	return pb
}

func containerEngineFromPB(e anvilv1.ContainerEngine) instance.ContainerEngine {
	switch e {
	case anvilv1.ContainerEngine_CONTAINER_ENGINE_DOCKER:
		return instance.ContainerEngineDocker
	case anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN:
		return instance.ContainerEnginePodman
	default:
		return ""
	}
}

func containerEngineToPB(e instance.ContainerEngine) anvilv1.ContainerEngine {
	switch e {
	case instance.ContainerEngineDocker:
		return anvilv1.ContainerEngine_CONTAINER_ENGINE_DOCKER
	case instance.ContainerEnginePodman:
		return anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN
	default:
		return anvilv1.ContainerEngine_CONTAINER_ENGINE_UNSPECIFIED
	}
}

func containerSpecFromPB(c *anvilv1.ContainerSpec) *instance.ContainerSpec {
	if c == nil {
		return nil
	}
	spec := &instance.ContainerSpec{
		ImageRef:    c.GetImageRef(),
		Env:         c.GetEnv(),
		Entrypoint:  c.GetEntrypoint(),
		Cmd:         c.GetCmd(),
		NetworkMode: c.GetNetworkMode(),
		Engine:      containerEngineFromPB(c.GetEngine()),
	}
	for _, vol := range c.GetVolumes() {
		spec.Volumes = append(spec.Volumes, instance.VolumeMount{
			HostPath:      vol.GetHostPath(),
			ContainerPath: vol.GetContainerPath(),
			ReadOnly:      vol.GetReadOnly(),
		})
	}
	for _, p := range c.GetPorts() {
		spec.Ports = append(spec.Ports, instance.PortMapping{
			HostPort:  int(p.GetHostPort()),
			GuestPort: int(p.GetGuestPort()),
			Protocol:  p.GetProtocol(),
		})
	}
	return spec
}

func containerSpecToPB(c *instance.ContainerSpec) *anvilv1.ContainerSpec {
	if c == nil {
		return nil
	}
	spec := &anvilv1.ContainerSpec{
		ImageRef:     c.ImageRef,
		Env:          c.Env,
		Entrypoint:   c.Entrypoint,
		Cmd:          c.Cmd,
		NetworkMode:  c.NetworkMode,
		Engine:       containerEngineToPB(c.Engine),
		ContainerId:  c.ContainerID,
		NetworkAlias: c.NetworkAlias,
		ExtraHosts:   c.ExtraHosts,
	}
	for _, vol := range c.Volumes {
		spec.Volumes = append(spec.Volumes, &anvilv1.VolumeMount{
			HostPath:      vol.HostPath,
			ContainerPath: vol.ContainerPath,
			ReadOnly:      vol.ReadOnly,
		})
	}
	for _, p := range c.Ports {
		spec.Ports = append(spec.Ports, &anvilv1.PortMapping{
			HostPort:  int32(p.HostPort),
			GuestPort: int32(p.GuestPort),
			Protocol:  p.Protocol,
		})
	}
	return spec
}

func specToPB(s *instance.Spec) *anvilv1.Instance {
	return &anvilv1.Instance{
		Id:            s.ID,
		Name:          s.Name,
		Kind:          kindToPB(s.Kind),
		State:         stateToPB(s.State),
		CreatedAtUnix: s.CreatedAt.Unix(),
		Labels:        s.Labels,
		Vm:            vmSpecToPB(s.VM),
		Container:     containerSpecToPB(s.Container),
		Guest:         guestInfoToPB(s.Guest),
		Autostart:     s.Autostart,
		RestartPolicy: restartPolicyToPB(s.RestartPolicy),
	}
}

func restartPolicyToPB(p instance.RestartPolicy) *anvilv1.RestartPolicy {
	mode := p.Mode
	if mode == "" {
		mode = instance.RestartNo
	}
	return &anvilv1.RestartPolicy{Mode: mode, MaxRetries: int32(p.MaxRetries)}
}

// restartPolicyFromPB validates p; nil means "not set".
func restartPolicyFromPB(p *anvilv1.RestartPolicy) (*instance.RestartPolicy, error) {
	if p == nil {
		return nil, nil
	}
	parsed, err := instance.ParseRestartPolicy(p.GetMode())
	if err != nil {
		return nil, err
	}
	if p.GetMaxRetries() != 0 {
		if parsed.Mode != instance.RestartOnFailure {
			return nil, fmt.Errorf("instance: max_retries only applies to on-failure")
		}
		parsed.MaxRetries = int(p.GetMaxRetries())
	}
	return &parsed, nil
}

func snapshotScheduleToPB(s *instance.SnapshotSchedule) *anvilv1.SnapshotSchedule {
	if s == nil {
		return nil
	}
	pb := &anvilv1.SnapshotSchedule{EverySeconds: int64(s.Every / time.Second), Keep: int32(s.Keep)}
	if !s.LastRun.IsZero() {
		pb.LastRunUnix = s.LastRun.Unix()
	}
	return pb
}

func guestInfoToPB(g *instance.GuestInfo) *anvilv1.GuestInfo {
	if g == nil {
		return nil
	}
	return &anvilv1.GuestInfo{
		AgentConnected: g.AgentConnected,
		IpAddresses:    g.IPAddresses,
		CloudInit:      cloudInitStatusToPB(g.CloudInit),
	}
}

func cloudInitStatusToPB(s instance.CloudInitStatus) anvilv1.CloudInitStatus {
	switch s {
	case instance.CloudInitRunning:
		return anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_RUNNING
	case instance.CloudInitDone:
		return anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_DONE
	case instance.CloudInitError:
		return anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_ERROR
	case instance.CloudInitDisabled:
		return anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_DISABLED
	default:
		return anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_UNSPECIFIED
	}
}

func specsToPB(specs []*instance.Spec) []*anvilv1.Instance {
	out := make([]*anvilv1.Instance, len(specs))
	for i, s := range specs {
		out[i] = specToPB(s)
	}
	return out
}

func statsToPB(s instance.Stats) *anvilv1.InstanceStats {
	return &anvilv1.InstanceStats{
		CpuPercent:           s.CPUPercent,
		MemUsedBytes:         s.MemUsedBytes,
		MemLimitBytes:        s.MemLimitBytes,
		DiskUsedBytes:        s.DiskUsedBytes,
		DiskTotalBytes:       s.DiskTotalBytes,
		DiskReadBytesPerSec:  s.DiskReadBytesPerSec,
		DiskWriteBytesPerSec: s.DiskWriteBytesPerSec,
		NetAvailable:         s.NetAvailable,
		NetRxBytesPerSec:     s.NetRxBytesPerSec,
		NetTxBytesPerSec:     s.NetTxBytesPerSec,
		UptimeSeconds:        s.UptimeSeconds,
		Address:              s.Address,
	}
}

func launchParamsFromPB(req *anvilv1.LaunchRequest) (instance.LaunchParams, error) {
	policy, err := restartPolicyFromPB(req.GetRestartPolicy())
	if err != nil {
		return instance.LaunchParams{}, err
	}
	params := instance.LaunchParams{
		Autostart:      req.GetAutostart(),
		Name:           req.GetName(),
		Kind:           kindFromPB(req.GetKind()),
		VM:             vmSpecFromPB(req.GetVm()),
		Container:      containerSpecFromPB(req.GetContainer()),
		NoStart:        req.GetNoStart(),
		IntentName:     req.GetIntentName(),
		Role:           req.GetRole(),
		PinnedStaticIP: req.GetPinnedStaticIp(),
	}
	if req.GetPinnedSubnet() != "" {
		params.PinnedNetwork = &instance.PinnedNetwork{
			Subnet:        req.GetPinnedSubnet(),
			Gateway:       req.GetPinnedGateway(),
			DockerIPRange: req.GetPinnedDockerIpRange(),
		}
	}
	if policy != nil {
		params.RestartPolicy = *policy
	}
	return params, nil
}
