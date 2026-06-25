// Package manifest defines the GitOps app-bundle file format and lowers a
// parsed bundle into the concrete types.* resources the control plane accepts.
package manifest

import (
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/kkjorsvik/smith/internal/types"
)

// App is one application bundle: exactly one workload plus optional service(s)
// and ingress(es). It is the on-disk GitOps manifest format. Field names use
// json tags so sigs.k8s.io/yaml (which converts YAML to JSON) honors the same
// snake_case keys as the HTTP API and reuses types.* sub-structs verbatim,
// including types.Duration's custom JSON (un)marshaling.
type App struct {
	Name      string        `json:"name"`
	Workload  WorkloadSpec  `json:"workload"`
	Service   *ServiceSpec  `json:"service,omitempty"`
	Services  []ServiceSpec `json:"services,omitempty"`
	Ingress   *IngressSpec  `json:"ingress,omitempty"`
	Ingresses []IngressSpec `json:"ingresses,omitempty"`
}

// WorkloadSpec is the workload section. It omits Workload.ID (implicit: the
// app Name) and carries only fields a manifest author sets.
type WorkloadSpec struct {
	Image          string              `json:"image"`
	Args           []string            `json:"args,omitempty"`
	Replicas       int                 `json:"replicas,omitempty"`
	MaxUnavailable int                 `json:"max_unavailable,omitempty"`
	Env            map[string]string   `json:"env,omitempty"`
	Resources      *types.Resources    `json:"resources,omitempty"`
	Volumes        []types.Volume      `json:"volumes,omitempty"`
	Ports          []types.PortMapping `json:"ports,omitempty"`
	HealthCheck    *types.HealthCheck  `json:"health_check,omitempty"`
}

// ServiceSpec is a service section. It omits WorkloadID (implicit: the app
// Name) and the control-plane-assigned ClusterIP. NodePort may be set only as
// an explicit pin.
type ServiceSpec struct {
	Name       string `json:"name,omitempty"`
	Port       int    `json:"port"`
	TargetPort int    `json:"target_port,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	NodePort   int    `json:"node_port,omitempty"`
}

// IngressSpec is an ingress section. Service may be empty in singular mode
// (defaults to the app's sole service).
type IngressSpec struct {
	Host    string `json:"host"`
	Service string `json:"service,omitempty"`
}

// Parse decodes one YAML app bundle. Unknown fields are rejected so typos in
// keys fail loudly instead of being silently dropped.
func Parse(data []byte) (*App, error) {
	var app App
	if err := yaml.UnmarshalStrict(data, &app); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &app, nil
}
