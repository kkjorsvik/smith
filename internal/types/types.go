package types

import (
	"encoding/json"
	"time"
)

// Workload describes a container smith should keep running.
type Workload struct {
	ID          string            `json:"id"`
	Image       string            `json:"image"`
	Args        []string          `json:"args"`
	HealthCheck *HealthCheck      `json:"health_check,omitempty"`
	Ports       []PortMapping     `json:"ports,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Resources   *Resources        `json:"resources,omitempty"`
	// Replicas is how many instances of this workload to run, spread across
	// nodes. 0 or omitted means 1.
	Replicas int `json:"replicas,omitempty"`
}

// Resources defines CPU and memory limits for a workload.
type Resources struct {
	// CPUMillicores is the CPU limit in millicores. 1000 = one full
	// core, 500 = half a core. Zero means no CPU limit.
	CPUMillicores int `json:"cpu_millicores,omitempty"`
	// MemoryMB is the memory limit in megabytes. Zero means no memory
	// limit. A container exceeding this will be OOM-killed.
	MemoryMB int `json:"memory_mb,omitempty"`
}

// NetworkConfig is the per-node container network assignment the control
// plane returns to an agent at registration. The agent builds its CNI bridge
// from this so container IPs are unique and routable across the cluster.
type NetworkConfig struct {
	// Subnet is this node's container CIDR, e.g. "10.22.3.0/24".
	Subnet string `json:"subnet"`
	// Gateway is the bridge IP on this node, e.g. "10.22.3.1".
	Gateway string `json:"gateway"`
}

// Route is one entry in a node's cross-node container routing table: reach
// Subnet (a peer's container CIDR) via Via (that peer's underlay host IP).
type Route struct {
	Subnet string `json:"subnet"` // e.g. "10.22.4.0/24"
	Via    string `json:"via"`    // e.g. "192.168.1.56"
}

// PortMapping maps a port on the host node to a port inside the container.
type PortMapping struct {
	// HostPort is the port exposed on the agent node's host network.
	HostPort int `json:"host_port"`
	// ContainerPort is the port the container process listens on.
	ContainerPort int `json:"container_port"`
	// Protocol is "tcp" or "udp". Defaults to "tcp" if empty.
	Protocol string `json:"protocol,omitempty"`
}

// HealthCheck defines how smith should probe a running container.
type HealthCheck struct {
	Type         string   `json:"type"`
	Command      []string `json:"command,omitempty"`
	URL          string   `json:"url,omitempty"`
	InitialDelay Duration `json:"initial_delay"`
	Interval     Duration `json:"interval"`
	Threshold    int      `json:"threshold"`
}

// Duration is a time.Duration that marshals to/from a human-readable
// string in JSON (e.g. "5s", "1m30s") instead of nanoseconds.
type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// Node represents a worker node registered with the control plane.
type Node struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
	// HostIP is the underlay IP other nodes route container traffic through.
	// It may differ from Addr's host (the API bind address); used as the
	// Via for cross-node routes.
	HostIP        string    `json:"host_ip"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	CPU           int       `json:"cpu"`
	MemoryMB      int       `json:"memory_mb"`
}

// Assignment represents a workload replica assigned to a specific node.
// WorkloadID is the replica instance ID (e.g. "smith-nginx-0"); ParentID is
// the workload it belongs to.
type Assignment struct {
	WorkloadID string `json:"workload_id"`
	NodeID     string `json:"node_id"`
	ParentID   string `json:"parent_id,omitempty"`
}
