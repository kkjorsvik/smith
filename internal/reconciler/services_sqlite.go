package reconciler

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/kkjorsvik/smith/internal/types"
	_ "github.com/mattn/go-sqlite3"
)

const (
	// ServiceCIDR is the pool ClusterIPs (VIPs) are allocated from. It must be
	// distinct from the pod BridgeSubnet (10.22.0.0/16).
	ServiceCIDR = "10.23.0.0/16"
	// NodePort range, kube-style.
	nodePortMin = 30000
	nodePortMax = 32767
)

// ServiceStore persists services and allocates each one a ClusterIP (from
// ServiceCIDR) and a NodePort (from the 30000-32767 range). Allocation is
// keyed by service name and persisted, so a service keeps the same VIP/port
// across control-plane restarts. Both are freed when the service is removed.
type ServiceStore struct {
	db      *sql.DB
	mu      sync.Mutex
	svcBase uint32 // ServiceCIDR network base as a uint32
}

// NewServiceStore opens (or creates) the services table in the SQLite database
// at path.
func NewServiceStore(path string) (*ServiceStore, error) {
	_, ipnet, err := net.ParseCIDR(ServiceCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse service CIDR %s: %w", ServiceCIDR, err)
	}
	base := ipnet.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("service CIDR %s is not IPv4", ServiceCIDR)
	}

	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	s := &ServiceStore{db: db, svcBase: binary.BigEndian.Uint32(base)}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *ServiceStore) migrate() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS services (
			name        TEXT PRIMARY KEY,
			workload_id TEXT NOT NULL,
			port        INTEGER NOT NULL,
			target_port INTEGER NOT NULL,
			protocol    TEXT NOT NULL,
			cluster_ip  TEXT NOT NULL UNIQUE,
			node_port   INTEGER NOT NULL UNIQUE
		);
	`); err != nil {
		return fmt.Errorf("create services table: %w", err)
	}
	return nil
}

// Add creates or updates a service. On create it allocates a ClusterIP and
// NodePort; on update it preserves the existing allocation (idempotent by
// name). It returns the stored service with ClusterIP and NodePort filled in.
func (s *ServiceStore) Add(svc types.Service) (types.Service, error) {
	if svc.Name == "" {
		return types.Service{}, fmt.Errorf("service name cannot be empty")
	}
	if svc.WorkloadID == "" {
		return types.Service{}, fmt.Errorf("service %s: workload_id cannot be empty", svc.Name)
	}
	if svc.Port <= 0 || svc.TargetPort <= 0 {
		return types.Service{}, fmt.Errorf("service %s: port and target_port must be positive", svc.Name)
	}
	if svc.Protocol == "" {
		svc.Protocol = "tcp"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Preserve an existing allocation (persistence guarantee).
	var ip string
	var port int
	err := s.db.QueryRow(`SELECT cluster_ip, node_port FROM services WHERE name = ?`, svc.Name).Scan(&ip, &port)
	switch {
	case err == nil:
		svc.ClusterIP = ip
		svc.NodePort = port
	case err == sql.ErrNoRows:
		svc.ClusterIP, svc.NodePort, err = s.allocate()
		if err != nil {
			return types.Service{}, err
		}
	default:
		return types.Service{}, fmt.Errorf("lookup service %s: %w", svc.Name, err)
	}

	if _, err := s.db.Exec(`
		INSERT INTO services (name, workload_id, port, target_port, protocol, cluster_ip, node_port)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			workload_id = excluded.workload_id,
			port        = excluded.port,
			target_port = excluded.target_port,
			protocol    = excluded.protocol;
	`, svc.Name, svc.WorkloadID, svc.Port, svc.TargetPort, svc.Protocol, svc.ClusterIP, svc.NodePort); err != nil {
		return types.Service{}, fmt.Errorf("upsert service %s: %w", svc.Name, err)
	}

	return svc, nil
}

// allocate picks the lowest free ClusterIP and NodePort. Caller holds s.mu.
func (s *ServiceStore) allocate() (string, int, error) {
	usedIPs := make(map[uint32]bool)
	usedPorts := make(map[int]bool)

	rows, err := s.db.Query(`SELECT cluster_ip, node_port FROM services`)
	if err != nil {
		return "", 0, fmt.Errorf("list allocations: %w", err)
	}
	for rows.Next() {
		var ip string
		var port int
		if err := rows.Scan(&ip, &port); err != nil {
			rows.Close()
			return "", 0, fmt.Errorf("scan allocation: %w", err)
		}
		if parsed := net.ParseIP(ip).To4(); parsed != nil {
			usedIPs[binary.BigEndian.Uint32(parsed)] = true
		}
		usedPorts[port] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("iterate allocations: %w", err)
	}

	// Lowest free IP, starting at base+1 (skip the network address), within
	// the /16 (base+1 .. base+65534).
	var ip string
	for i := uint32(1); i <= 65534; i++ {
		cand := s.svcBase + i
		if !usedIPs[cand] {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], cand)
			ip = net.IP(b[:]).String()
			break
		}
	}
	if ip == "" {
		return "", 0, fmt.Errorf("ClusterIP pool exhausted")
	}

	// Lowest free NodePort.
	port := 0
	for p := nodePortMin; p <= nodePortMax; p++ {
		if !usedPorts[p] {
			port = p
			break
		}
	}
	if port == 0 {
		return "", 0, fmt.Errorf("NodePort range exhausted")
	}

	return ip, port, nil
}

// List returns all services.
func (s *ServiceStore) List() ([]types.Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT name, workload_id, port, target_port, protocol, cluster_ip, node_port FROM services`)
	if err != nil {
		return nil, fmt.Errorf("query services: %w", err)
	}
	defer rows.Close()

	var out []types.Service
	for rows.Next() {
		var svc types.Service
		if err := rows.Scan(&svc.Name, &svc.WorkloadID, &svc.Port, &svc.TargetPort, &svc.Protocol, &svc.ClusterIP, &svc.NodePort); err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// Remove deletes a service, freeing its ClusterIP and NodePort.
func (s *ServiceStore) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(`DELETE FROM services WHERE name = ?`, name); err != nil {
		return fmt.Errorf("remove service %s: %w", name, err)
	}
	return nil
}

// Close releases the database handle.
func (s *ServiceStore) Close() error {
	return s.db.Close()
}
