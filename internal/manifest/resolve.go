package manifest

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kkjorsvik/smith/internal/types"
)

// nameRe is the identifier pattern shared with the HTTP API for workload and
// service names.
var nameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// Resolved is an App lowered into the concrete API resources the control plane
// accepts: exactly one workload, plus zero or more services and ingresses with
// implicit names, defaults, and cross-references filled in.
type Resolved struct {
	Workload  types.Workload
	Services  []types.Service
	Ingresses []types.Ingress
}

// Resolve validates the bundle and lowers it to concrete types.* values.
func (a *App) Resolve() (*Resolved, error) {
	if a.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if !nameRe.MatchString(a.Name) {
		return nil, fmt.Errorf("name %q must match [a-z0-9-]+", a.Name)
	}
	if a.Service != nil && len(a.Services) > 0 {
		return nil, fmt.Errorf("set either service or services, not both")
	}
	if a.Ingress != nil && len(a.Ingresses) > 0 {
		return nil, fmt.Errorf("set either ingress or ingresses, not both")
	}

	wl, err := a.resolveWorkload()
	if err != nil {
		return nil, err
	}
	svcs, err := a.resolveServices()
	if err != nil {
		return nil, err
	}
	ings, err := a.resolveIngresses(svcs)
	if err != nil {
		return nil, err
	}
	return &Resolved{Workload: wl, Services: svcs, Ingresses: ings}, nil
}

func (a *App) resolveWorkload() (types.Workload, error) {
	w := a.Workload
	if w.Image == "" {
		return types.Workload{}, fmt.Errorf("workload.image is required")
	}
	if w.Replicas < 0 {
		return types.Workload{}, fmt.Errorf("workload.replicas must be >= 0 (got %d)", w.Replicas)
	}
	if w.MaxUnavailable < 0 {
		return types.Workload{}, fmt.Errorf("workload.max_unavailable must be >= 0 (got %d)", w.MaxUnavailable)
	}
	replicas := w.Replicas
	if replicas == 0 {
		replicas = 1
	}
	if len(w.Volumes) > 0 && replicas > 1 {
		return types.Workload{}, fmt.Errorf("workload with volumes must have replicas: 1 (single writer)")
	}
	if err := validateVolumes(w.Volumes); err != nil {
		return types.Workload{}, err
	}
	maxUnavail := w.MaxUnavailable
	if maxUnavail == 0 {
		maxUnavail = 1
	}
	return types.Workload{
		ID:             a.Name,
		Image:          w.Image,
		Args:           w.Args,
		Env:            w.Env,
		Resources:      w.Resources,
		Replicas:       replicas,
		MaxUnavailable: maxUnavail,
		Volumes:        w.Volumes,
		Ports:          w.Ports,
		HealthCheck:    w.HealthCheck,
	}, nil
}

// validateVolumes mirrors the control plane's volume invariants (see
// internal/api validateWorkload) so a bad volume name or path fails at resolve
// time rather than only at apply time: names must be a safe NFS subdirectory
// component, be unique, and mount paths must be absolute.
func validateVolumes(vols []types.Volume) error {
	seen := make(map[string]bool, len(vols))
	for _, v := range vols {
		if !nameRe.MatchString(v.Name) {
			return fmt.Errorf("volume name %q must match [a-z0-9-]+", v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("duplicate volume name %q", v.Name)
		}
		seen[v.Name] = true
		if !strings.HasPrefix(v.Path, "/") {
			return fmt.Errorf("volume %q path %q must be absolute", v.Name, v.Path)
		}
	}
	return nil
}

func (a *App) resolveServices() ([]types.Service, error) {
	var specs []ServiceSpec
	singular := false
	if a.Service != nil {
		specs = []ServiceSpec{*a.Service}
		singular = true
	} else {
		specs = a.Services
	}

	seen := map[string]bool{}
	var out []types.Service
	for i, s := range specs {
		name := s.Name
		if name == "" {
			if singular {
				name = a.Name
			} else {
				return nil, fmt.Errorf("services[%d].name is required in list mode", i)
			}
		}
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("service name %q must match [a-z0-9-]+", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate service name %q", name)
		}
		seen[name] = true
		if s.Port <= 0 {
			return nil, fmt.Errorf("service %q: port is required", name)
		}
		target := s.TargetPort
		if target == 0 {
			target = s.Port
		}
		proto := s.Protocol
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			return nil, fmt.Errorf("service %q: protocol %q must be tcp or udp", name, proto)
		}
		out = append(out, types.Service{
			Name:       name,
			WorkloadID: a.Name,
			Port:       s.Port,
			TargetPort: target,
			Protocol:   proto,
			NodePort:   s.NodePort,
		})
	}
	return out, nil
}

func (a *App) resolveIngresses(svcs []types.Service) ([]types.Ingress, error) {
	var specs []IngressSpec
	if a.Ingress != nil {
		specs = []IngressSpec{*a.Ingress}
	} else {
		specs = a.Ingresses
	}
	if len(specs) == 0 {
		return nil, nil
	}

	svcNames := map[string]bool{}
	for _, s := range svcs {
		svcNames[s.Name] = true
	}

	var out []types.Ingress
	for i, in := range specs {
		if in.Host == "" {
			return nil, fmt.Errorf("ingresses[%d].host is required", i)
		}
		svc := in.Service
		if svc == "" {
			if len(svcs) != 1 {
				return nil, fmt.Errorf("ingress %q: service is required unless the app declares exactly one service (has %d)", in.Host, len(svcs))
			}
			svc = svcs[0].Name
		} else if !svcNames[svc] {
			return nil, fmt.Errorf("ingress %q: service %q is not declared in this app", in.Host, svc)
		}
		out = append(out, types.Ingress{Host: in.Host, Service: svc})
	}
	return out, nil
}
