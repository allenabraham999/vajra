// Package vmm wraps the Cloud Hypervisor REST API and manages the lifetime
// of cloud-hypervisor processes. The structs in this file mirror the JSON
// payloads documented at:
//   https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/api.md
//
// Only the subset of fields Vajra currently uses is modelled. omitempty is
// applied liberally so that minimal configs round-trip without leaking nulls
// to the VMM.
package vmm

// VmConfig is the body of PUT /api/v1/vm.create.
type VmConfig struct {
	Cpus    *CpusConfig    `json:"cpus,omitempty"`
	Memory  *MemoryConfig  `json:"memory,omitempty"`
	Payload *PayloadConfig `json:"payload,omitempty"`
	Disks   []DiskConfig   `json:"disks,omitempty"`
	Net     []NetConfig    `json:"net,omitempty"`
	Vsock   *VsockConfig   `json:"vsock,omitempty"`
	Console *ConsoleConfig `json:"console,omitempty"`
	Serial  *ConsoleConfig `json:"serial,omitempty"`
	Rng     *RngConfig     `json:"rng,omitempty"`
}

// CpusConfig describes the vCPU topology.
type CpusConfig struct {
	BootVcpus int `json:"boot_vcpus"`
	MaxVcpus  int `json:"max_vcpus"`
}

// MemoryConfig describes guest memory. Size is in bytes.
type MemoryConfig struct {
	Size      int64 `json:"size"`
	Shared    bool  `json:"shared,omitempty"`
	Hugepages bool  `json:"hugepages,omitempty"`
}

// PayloadConfig points at the kernel/initramfs and supplies the boot cmdline.
type PayloadConfig struct {
	Kernel    string `json:"kernel,omitempty"`
	Initramfs string `json:"initramfs,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
	Firmware  string `json:"firmware,omitempty"`
}

// DiskConfig describes a virtio-blk disk.
type DiskConfig struct {
	Path      string `json:"path"`
	Readonly  bool   `json:"readonly,omitempty"`
	Direct    bool   `json:"direct,omitempty"`
	NumQueues int    `json:"num_queues,omitempty"`
	QueueSize int    `json:"queue_size,omitempty"`
}

// NetConfig describes a virtio-net device.
type NetConfig struct {
	Tap  string `json:"tap,omitempty"`
	Mac  string `json:"mac,omitempty"`
	IP   string `json:"ip,omitempty"`
	Mask string `json:"mask,omitempty"`
}

// VsockConfig wires a virtio-vsock device. Cid is the guest CID; Socket is
// the host-side Unix socket path.
type VsockConfig struct {
	Cid    uint32 `json:"cid"`
	Socket string `json:"socket"`
}

// ConsoleConfig configures a console or serial device. Mode is one of
// "Off", "Pty", "Tty", "File", "Null", "Socket".
type ConsoleConfig struct {
	Mode string `json:"mode"`
	File string `json:"file,omitempty"`
}

// RngConfig configures a virtio-rng device. Src is typically /dev/urandom.
type RngConfig struct {
	Src string `json:"src"`
}

// SnapshotConfig is the body of PUT /api/v1/vm.snapshot. DestinationURL is a
// "file://..." URL pointing at an existing directory.
type SnapshotConfig struct {
	DestinationURL string `json:"destination_url"`
}

// RestoreConfig is the body of PUT /api/v1/vm.restore. SourceURL is a
// "file://..." URL pointing at a snapshot directory previously produced by
// vm.snapshot.
type RestoreConfig struct {
	SourceURL string `json:"source_url"`
	Prefault  bool   `json:"prefault,omitempty"`
}

// VmInfo is the response from GET /api/v1/vm.info.
type VmInfo struct {
	Config           VmConfig `json:"config"`
	State            string   `json:"state"`
	MemoryActualSize int64    `json:"memory_actual_size,omitempty"`
}
