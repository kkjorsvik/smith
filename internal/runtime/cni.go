package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	gocni "github.com/containerd/go-cni"
	"github.com/kkjorsvik/smith/internal/types"
)

// BridgeSubnet is the cluster-wide CIDR pool the control plane carves into
// per-node /24 blocks. It is the firewall's forwarding/masquerade scope and
// must match the SubnetAllocator's pool.
const BridgeSubnet = "10.22.0.0/16"

const (
	// bridgeName is the Linux bridge smith creates on each node.
	bridgeName = "smith0"
	// cniVersion must be supported by the CNI plugins installed in
	// /opt/cni/bin. 1.0.0 is supported by containernetworking/plugins v1.x;
	// drop to "0.4.0" if older plugins reject it.
	cniVersion = "1.0.0"
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

// NewCNIForSubnet initializes CNI from an in-process bridge config generated
// for this node's assigned subnet, rather than a static /etc/cni/net.d file.
// This is how each node gets a unique, cluster-routable container subnet.
func NewCNIForSubnet(subnet, gateway string) (*CNI, error) {
	confList, err := renderBridgeConflist(subnet, gateway)
	if err != nil {
		return nil, fmt.Errorf("render bridge conflist: %w", err)
	}

	c, err := gocni.New(
		gocni.WithMinNetworkCount(2),
		gocni.WithPluginConfDir("/etc/cni/net.d"),
		gocni.WithPluginDir([]string{"/opt/cni/bin"}),
		gocni.WithInterfacePrefix("eth"),
	)
	if err != nil {
		return nil, fmt.Errorf("create cni: %w", err)
	}

	if err := c.Load(gocni.WithLoNetwork, gocni.WithConfListBytes(confList)); err != nil {
		return nil, fmt.Errorf("load cni config: %w", err)
	}

	log.Printf("cni: bridge %s configured for subnet %s (gateway %s)", bridgeName, subnet, gateway)
	return &CNI{cni: c}, nil
}

// renderBridgeConflist builds a bridge + host-local + portmap CNI conflist
// for the given node subnet. ipMasq is false: masquerading is handled
// selectively by the firewall (egress only) so inter-node container traffic
// keeps its real source IP.
func renderBridgeConflist(subnet, gateway string) ([]byte, error) {
	conflist := map[string]any{
		"cniVersion": cniVersion,
		"name":       "smith",
		"plugins": []any{
			map[string]any{
				"type":        "bridge",
				"bridge":      bridgeName,
				"isGateway":   true,
				"ipMasq":      false,
				"hairpinMode": true,
				"ipam": map[string]any{
					"type": "host-local",
					"ranges": []any{
						[]any{
							map[string]any{"subnet": subnet, "gateway": gateway},
						},
					},
					"routes": []any{
						map[string]any{"dst": "0.0.0.0/0"},
					},
				},
			},
			map[string]any{
				"type":         "portmap",
				"capabilities": map[string]any{"portMappings": true},
			},
		},
	}
	return json.Marshal(conflist)
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
		// When StopContainer and the RunContainer exit path both tear down
		// the same container, the second portmap delete hits an already-gone
		// chain. That is harmless — treat it as success.
		msg := err.Error()
		if strings.Contains(msg, "No chain/target/match by that name") ||
			strings.Contains(msg, "no chain") {
			return nil
		}
		return fmt.Errorf("cni remove for %s: %w", id, err)
	}

	log.Printf("cni: container %s networking torn down", id)
	return nil
}
