package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/kkjorsvik/smith/internal/types"
)

const (
	natTable      = "nat"
	filterTable   = "filter"
	servicesChain = "SMITH-SERVICES"
	markChain     = "SMITH-MARK-MASQ"
	// masqMark is the fwmark set on packets that must be SNAT'd on egress
	// (NodePort-from-external and hairpin), masqueraded in POSTROUTING.
	masqMark = "0x4000/0x4000"
)

// ServiceProxy programs the iptables nat rules that load-balance a service's
// ClusterIP and NodePort across its backend replica IPs (kube-proxy iptables
// mode, simplified). It is reconciled from the control plane's endpoint list
// on every sync and owns the SMITH-* chains.
type ServiceProxy struct {
	ipt     *iptables.IPTables
	podCIDR string
	// nodePorts currently opened in filter/INPUT, port -> protocol, so stale
	// ones can be closed when a service goes away.
	nodePorts map[int]string
}

// NewServiceProxy returns a ServiceProxy. podCIDR is the pod network used to
// decide which traffic needs masquerading (anything not from a pod, plus
// hairpin from a backend).
func NewServiceProxy(podCIDR string) (*ServiceProxy, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("init iptables: %w", err)
	}
	return &ServiceProxy{ipt: ipt, podCIDR: podCIDR, nodePorts: make(map[int]string)}, nil
}

// serviceChain returns the nat chain name for a service (<=28 chars).
func serviceChain(name string) string {
	h := sha256.Sum256([]byte(name))
	return "SMITH-SVC-" + hex.EncodeToString(h[:])[:12]
}

// Sync reconciles all service nat rules to match the given endpoint set. It is
// idempotent: per-service chains are rebuilt from scratch each call, and
// chains/NodePorts for services no longer present are removed.
func (p *ServiceProxy) Sync(services []types.ServiceEndpoints) error {
	if err := p.ensureBaseChains(); err != nil {
		return fmt.Errorf("ensure base chains: %w", err)
	}

	// Build/populate each service's own chain first, so the parent chain can
	// reference them.
	desired := make(map[string]bool, len(services))
	for _, svc := range services {
		chain := serviceChain(svc.Name)
		desired[chain] = true
		if err := p.ensureChain(chain); err != nil {
			return fmt.Errorf("ensure chain %s: %w", chain, err)
		}
		if err := p.populateServiceChain(chain, svc); err != nil {
			return fmt.Errorf("populate chain %s: %w", chain, err)
		}
	}

	// Rebuild the parent dispatch chain (ClusterIP + NodePort jumps).
	if err := p.populateServicesChain(services); err != nil {
		return fmt.Errorf("populate %s: %w", servicesChain, err)
	}

	// Remove chains for services that no longer exist.
	if err := p.cleanupStaleChains(desired); err != nil {
		return fmt.Errorf("cleanup stale chains: %w", err)
	}

	// Reconcile NodePort INPUT-accept rules.
	if err := p.syncNodePorts(services); err != nil {
		return fmt.Errorf("sync node ports: %w", err)
	}

	return nil
}

// ensureBaseChains creates the mark and dispatch chains and the jumps into
// them (PREROUTING/OUTPUT) plus the masquerade rule (POSTROUTING). Idempotent.
func (p *ServiceProxy) ensureBaseChains() error {
	if err := p.ensureChain(markChain); err != nil {
		return err
	}
	if err := p.ipt.AppendUnique(natTable, markChain, "-j", "MARK", "--set-xmark", masqMark); err != nil {
		return err
	}
	if err := p.ensureChain(servicesChain); err != nil {
		return err
	}
	if err := p.ipt.AppendUnique(natTable, "PREROUTING", "-j", servicesChain); err != nil {
		return err
	}
	if err := p.ipt.AppendUnique(natTable, "OUTPUT", "-j", servicesChain); err != nil {
		return err
	}
	return p.ipt.AppendUnique(natTable, "POSTROUTING", "-m", "mark", "--mark", masqMark, "-j", "MASQUERADE")
}

func (p *ServiceProxy) ensureChain(chain string) error {
	ok, err := p.ipt.ChainExists(natTable, chain)
	if err != nil {
		return err
	}
	if !ok {
		return p.ipt.NewChain(natTable, chain)
	}
	return nil
}

// populateServiceChain rebuilds a single service's chain: mark rules for
// traffic that needs SNAT, then random-weighted DNAT to each backend.
func (p *ServiceProxy) populateServiceChain(chain string, svc types.ServiceEndpoints) error {
	if err := p.ipt.ClearChain(natTable, chain); err != nil {
		return err
	}

	proto := svc.Protocol
	if proto == "" {
		proto = "tcp"
	}

	// SNAT external/host traffic (NodePort from outside, host -> ClusterIP).
	if err := p.ipt.Append(natTable, chain, "!", "-s", p.podCIDR, "-j", markChain); err != nil {
		return err
	}
	// SNAT hairpin (a backend reaching its own service).
	for _, ep := range svc.Endpoints {
		if err := p.ipt.Append(natTable, chain, "-s", ep+"/32", "-j", markChain); err != nil {
			return err
		}
	}

	// Random uniform selection: rule i fires with probability 1/(N-i); the
	// last endpoint is unconditional. DNAT is conntracked, so balancing is
	// per-connection.
	n := len(svc.Endpoints)
	for i, ep := range svc.Endpoints {
		dest := fmt.Sprintf("%s:%d", ep, svc.TargetPort)
		if i < n-1 {
			prob := 1.0 / float64(n-i)
			if err := p.ipt.Append(natTable, chain,
				"-p", proto, "-m", "statistic", "--mode", "random",
				"--probability", fmt.Sprintf("%.5f", prob),
				"-j", "DNAT", "--to-destination", dest); err != nil {
				return err
			}
		} else {
			if err := p.ipt.Append(natTable, chain,
				"-p", proto, "-j", "DNAT", "--to-destination", dest); err != nil {
				return err
			}
		}
	}
	return nil
}

// populateServicesChain rebuilds the parent dispatch chain with a ClusterIP
// and NodePort match per service jumping to that service's chain.
func (p *ServiceProxy) populateServicesChain(services []types.ServiceEndpoints) error {
	if err := p.ipt.ClearChain(natTable, servicesChain); err != nil {
		return err
	}
	for _, svc := range services {
		chain := serviceChain(svc.Name)
		proto := svc.Protocol
		if proto == "" {
			proto = "tcp"
		}
		if svc.ClusterIP != "" && svc.Port > 0 {
			if err := p.ipt.Append(natTable, servicesChain,
				"-d", svc.ClusterIP+"/32", "-p", proto, "--dport", strconv.Itoa(svc.Port),
				"-j", chain); err != nil {
				return err
			}
		}
		if svc.NodePort > 0 {
			if err := p.ipt.Append(natTable, servicesChain,
				"-p", proto, "-m", "addrtype", "--dst-type", "LOCAL",
				"--dport", strconv.Itoa(svc.NodePort), "-j", chain); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanupStaleChains flushes and deletes SMITH-SVC-* chains not in desired.
func (p *ServiceProxy) cleanupStaleChains(desired map[string]bool) error {
	chains, err := p.ipt.ListChains(natTable)
	if err != nil {
		return err
	}
	for _, c := range chains {
		if !strings.HasPrefix(c, "SMITH-SVC-") || desired[c] {
			continue
		}
		if err := p.ipt.ClearChain(natTable, c); err != nil {
			return err
		}
		if err := p.ipt.DeleteChain(natTable, c); err != nil {
			return err
		}
	}
	return nil
}

// syncNodePorts opens an INPUT-accept rule for each service's NodePort and
// closes ones no longer in use.
func (p *ServiceProxy) syncNodePorts(services []types.ServiceEndpoints) error {
	want := make(map[int]string)
	for _, svc := range services {
		if svc.NodePort <= 0 {
			continue
		}
		proto := svc.Protocol
		if proto == "" {
			proto = "tcp"
		}
		want[svc.NodePort] = proto
	}

	for port, proto := range want {
		if _, open := p.nodePorts[port]; open {
			continue
		}
		if err := p.ipt.AppendUnique(filterTable, "INPUT",
			"-p", proto, "--dport", strconv.Itoa(port), "-j", "ACCEPT"); err != nil {
			return err
		}
		p.nodePorts[port] = proto
	}

	for port, proto := range p.nodePorts {
		if _, keep := want[port]; keep {
			continue
		}
		if err := p.ipt.DeleteIfExists(filterTable, "INPUT",
			"-p", proto, "--dport", strconv.Itoa(port), "-j", "ACCEPT"); err != nil {
			return err
		}
		delete(p.nodePorts, port)
	}
	return nil
}
