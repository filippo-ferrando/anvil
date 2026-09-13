// Package daemon implements anvilv1.InstanceServiceServer against
// internal/instance.Manager, translating between the gRPC wire types
// (api/gen/anvil/v1) and anvil's domain model (internal/instance) — kept
// as an explicit conversion layer, per the plan, so the two can evolve
// independently.
package daemon

import (
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
	return &instance.VMSpec{
		ImageRef:          v.GetImageRef(),
		Arch:              v.GetArch(),
		CPUs:              int(v.GetCpus()),
		MemoryMiB:         v.GetMemoryMib(),
		DiskGiB:           v.GetDiskGib(),
		CloudInitUserData: v.GetCloudInitUserData(),
		CloudInitName:     v.GetCloudInitName(),
		NetworkMode:       v.GetNetworkMode(),
		SSHPublicKeys:     v.GetSshPublicKeys(),
		// SSHPort/DefaultUser/BridgeInterface/StaticIP/Gateway are all
		// daemon-populated outputs, not client input — a LaunchRequest
		// never carries them, so there's nothing to read here; see
		// vmSpecToPB for where they're actually set.
	}
}

func vmSpecToPB(v *instance.VMSpec) *anvilv1.VMSpec {
	if v == nil {
		return nil
	}
	pb := &anvilv1.VMSpec{
		ImageRef:          v.ImageRef,
		Arch:              v.Arch,
		Cpus:              int32(v.CPUs),
		MemoryMib:         v.MemoryMiB,
		DiskGib:           v.DiskGiB,
		CloudInitUserData: v.CloudInitUserData,
		CloudInitName:     v.CloudInitName,
		NetworkMode:       v.NetworkMode,
		SshPublicKeys:     v.SSHPublicKeys,
		SshPort:           int32(v.SSHPort),
		DefaultUser:       v.DefaultUser,
		BridgeInterface:   v.BridgeInterface,
		StaticIp:          v.StaticIP,
		Gateway:           v.Gateway,
	}
	for _, m := range v.Mounts {
		pb.Mounts = append(pb.Mounts, &anvilv1.Mount{
			HostPath:  m.HostPath,
			GuestPath: m.GuestPath,
			Tag:       m.Tag,
			ReadOnly:  m.ReadOnly,
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
		// ContainerID is daemon-populated, not client input — see
		// containerSpecToPB for where it's actually set.
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
		ImageRef:    c.ImageRef,
		Env:         c.Env,
		Entrypoint:  c.Entrypoint,
		Cmd:         c.Cmd,
		NetworkMode: c.NetworkMode,
		Engine:      containerEngineToPB(c.Engine),
		ContainerId: c.ContainerID,
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
	}
}

func specsToPB(specs []*instance.Spec) []*anvilv1.Instance {
	out := make([]*anvilv1.Instance, len(specs))
	for i, s := range specs {
		out[i] = specToPB(s)
	}
	return out
}

func launchParamsFromPB(req *anvilv1.LaunchRequest) instance.LaunchParams {
	return instance.LaunchParams{
		Name:       req.GetName(),
		Kind:       kindFromPB(req.GetKind()),
		VM:         vmSpecFromPB(req.GetVm()),
		Container:  containerSpecFromPB(req.GetContainer()),
		NoStart:    req.GetNoStart(),
		IntentName: req.GetIntentName(),
		Role:       req.GetRole(),
	}
}
