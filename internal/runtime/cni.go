package runtime

import (
	"context"
	"fmt"
	"log"

	gocni "github.com/containerd/go-cni"
	"github.com/kkjorsvik/smith/internal/types"
)

// CNI wraps the go-cni library configured for smith's network.
type CNI struct {
	cni gocni.CNI
}

// NewCNI initializes CNI from the config files in /etc/cni/net.d.
// It loads the smith network config and prepares the plugin chain.
func NewCNI() (*CNI, error) {
	c, err := gocni.New(
		gocni.WithMinNetworkCount(2),
		gocni.WithPluginConfDir("/etc/cni/net.d"),
		gocni.WithPluginDir([]string{"/opt/cni/bin"}),
		gocni.WithInterfacePrefix("eth"),
	)
	if err != nil {
		return nil, fmt.Errorf("create cni: %w", err)
	}

	// Load the network config from disk.
	if err := c.Load(gocni.WithLoNetwork, gocni.WithDefaultConf); err != nil {
		return nil, fmt.Errorf("load cni config: %w", err)
	}

	return &CNI{cni: c}, nil
}

// portMappings converts smith port mappings into go-cni port mappings,
// defaulting an empty protocol to "tcp".
func portMappings(ports []types.PortMapping) []gocni.PortMapping {
	mappings := make([]gocni.PortMapping, 0, len(ports))
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		mappings = append(mappings, gocni.PortMapping{
			HostPort:      int32(p.HostPort),
			ContainerPort: int32(p.ContainerPort),
			Protocol:      proto,
		})
	}
	return mappings
}

// Setup configures networking for a container's network namespace.
// id is the container ID, netnsPath is the path to the container's
// network namespace (from the task), and ports are the host->container
// port mappings to publish via the portmap plugin.
// Returns the assigned container IP.
func (c *CNI) Setup(ctx context.Context, id, netnsPath string, ports []types.PortMapping) (string, error) {
	opts := []gocni.NamespaceOpts{}

	// Add port mappings as a CNI capability for the portmap plugin.
	if len(ports) > 0 {
		opts = append(opts, gocni.WithCapabilityPortMap(portMappings(ports)))
	}

	result, err := c.cni.Setup(ctx, id, netnsPath, opts...)
	if err != nil {
		return "", fmt.Errorf("cni setup for %s: %w", id, err)
	}

	// Extract the container IP from the result.
	var ip string
	for _, iface := range result.Interfaces {
		for _, ipconf := range iface.IPConfigs {
			ip = ipconf.IP.String()
			break
		}
		if ip != "" {
			break
		}
	}

	log.Printf("cni: container %s networked with IP %s", id, ip)
	return ip, nil
}

// Teardown removes networking for a container's network namespace.
// Must be called with the same ports that were used in Setup so the
// portmap plugin can remove the correct DNAT rules.
func (c *CNI) Teardown(ctx context.Context, id, netnsPath string, ports []types.PortMapping) error {
	opts := []gocni.NamespaceOpts{}

	if len(ports) > 0 {
		opts = append(opts, gocni.WithCapabilityPortMap(portMappings(ports)))
	}

	if err := c.cni.Remove(ctx, id, netnsPath, opts...); err != nil {
		return fmt.Errorf("cni remove for %s: %w", id, err)
	}

	log.Printf("cni: container %s networking torn down", id)
	return nil
}
