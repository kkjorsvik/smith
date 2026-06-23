package runtime

import (
	"fmt"
	"log"
	"os"

	"github.com/coreos/go-iptables/iptables"
	"github.com/kkjorsvik/smith/internal/types"
)

// Firewall manages iptables rules for smith container networking.
// It is safe to call its methods repeatedly — all rule insertions are
// idempotent (checked before adding).
type Firewall struct {
	ipt    *iptables.IPTables
	subnet string
}

// NewFirewall returns a Firewall manager for the given bridge subnet
// (e.g. "10.22.0.0/16").
func NewFirewall(subnet string) (*Firewall, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("init iptables: %w", err)
	}
	return &Firewall{ipt: ipt, subnet: subnet}, nil
}

// EnsureForwarding sets up the FORWARD chain rules that allow traffic
// to and from the container bridge subnet. This is the equivalent of
// "ufw route allow to 10.22.0.0/16" and must be called once on agent
// startup. Idempotent.
func (f *Firewall) EnsureForwarding() error {
	// Allow forwarding TO the container subnet (inbound to containers).
	if err := f.ensureRule("filter", "FORWARD",
		"-d", f.subnet, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("forward to subnet: %w", err)
	}

	// Allow forwarding FROM the container subnet (container egress).
	if err := f.ensureRule("filter", "FORWARD",
		"-s", f.subnet, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("forward from subnet: %w", err)
	}

	log.Printf("firewall: forwarding enabled for %s", f.subnet)
	return nil
}

// EnsureMasquerade adds a nat POSTROUTING rule that masquerades container
// traffic leaving the cluster (egress to the internet) while NOT touching
// inter-node container traffic (destination still inside the cluster CIDR),
// so cross-node packets keep their real container source IP. The bridge is
// configured with ipMasq:false precisely so this is the only masquerade rule.
// Idempotent.
func (f *Firewall) EnsureMasquerade(clusterCIDR string) error {
	if err := f.ensureRule("nat", "POSTROUTING",
		"-s", clusterCIDR, "!", "-d", clusterCIDR, "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("masquerade for %s: %w", clusterCIDR, err)
	}
	log.Printf("firewall: egress masquerade enabled for %s", clusterCIDR)
	return nil
}

// EnableIPForwarding turns on IPv4 forwarding so the host routes traffic
// between container subnets and on to other nodes. Without it, inter-node
// container traffic is dropped. Best-effort; callers log failures.
func EnableIPForwarding() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	log.Printf("firewall: net.ipv4.ip_forward enabled")
	return nil
}

// OpenPort allows inbound traffic to a published host port. Called when
// a container with a port mapping starts. Idempotent.
func (f *Firewall) OpenPort(port int, protocol string) error {
	if protocol == "" {
		protocol = "tcp"
	}
	if err := f.ensureRule("filter", "INPUT",
		"-p", protocol, "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("open port %d/%s: %w", port, protocol, err)
	}
	log.Printf("firewall: opened port %d/%s", port, protocol)
	return nil
}

// ClosePort removes the inbound allow rule for a host port. Called when
// a container with a port mapping is stopped. Tolerant of the rule not
// existing.
func (f *Firewall) ClosePort(port int, protocol string) error {
	if protocol == "" {
		protocol = "tcp"
	}
	exists, err := f.ipt.Exists("filter", "INPUT",
		"-p", protocol, "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT")
	if err != nil {
		return fmt.Errorf("check port %d/%s: %w", port, protocol, err)
	}
	if !exists {
		return nil
	}
	if err := f.ipt.Delete("filter", "INPUT",
		"-p", protocol, "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("close port %d/%s: %w", port, protocol, err)
	}
	log.Printf("firewall: closed port %d/%s", port, protocol)
	return nil
}

// OpenPorts opens all host ports in a set of port mappings.
func (f *Firewall) OpenPorts(ports []types.PortMapping) error {
	for _, p := range ports {
		if err := f.OpenPort(p.HostPort, p.Protocol); err != nil {
			return err
		}
	}
	return nil
}

// ClosePorts closes all host ports in a set of port mappings.
func (f *Firewall) ClosePorts(ports []types.PortMapping) error {
	for _, p := range ports {
		if err := f.ClosePort(p.HostPort, p.Protocol); err != nil {
			log.Printf("firewall: close port %d: %v", p.HostPort, err)
		}
	}
	return nil
}

// ensureRule inserts a rule only if it does not already exist.
func (f *Firewall) ensureRule(table, chain string, spec ...string) error {
	exists, err := f.ipt.Exists(table, chain, spec...)
	if err != nil {
		return fmt.Errorf("check rule: %w", err)
	}
	if exists {
		return nil
	}
	// Insert at position 1 so our ACCEPT rules come before any
	// restrictive rules already in the chain.
	if err := f.ipt.Insert(table, chain, 1, spec...); err != nil {
		return fmt.Errorf("insert rule: %w", err)
	}
	return nil
}
