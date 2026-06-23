package reconciler

import (
	"database/sql"
	"fmt"
	"net"
	"sync"

	"github.com/kkjorsvik/smith/internal/types"
	_ "github.com/mattn/go-sqlite3"
)

// SubnetAllocator hands out and persists per-node container subnets carved
// from a cluster /16 into per-node /24 blocks (node index i -> X.Y.i.0/24,
// gateway X.Y.i.1).
//
// Allocation is keyed by node ID and persists in SQLite, so a node that
// re-registers — including after a control-plane restart — always gets the
// same subnet back. Subnets are reclaimed only by an explicit Release
// (operator decommission), never on heartbeat timeout, so a rebooting or
// briefly-partitioned node never has its subnet reassigned out from under it.
type SubnetAllocator struct {
	db   *sql.DB
	mu   sync.Mutex
	base net.IP // cluster network base (first two octets used), e.g. 10.22.0.0
}

// NewSubnetAllocator opens (or creates) the allocation table in the SQLite
// database at path. clusterCIDR must be a /16; it is carved into /24 blocks.
func NewSubnetAllocator(path, clusterCIDR string) (*SubnetAllocator, error) {
	_, ipnet, err := net.ParseCIDR(clusterCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse cluster CIDR %s: %w", clusterCIDR, err)
	}
	ones, _ := ipnet.Mask.Size()
	if ones != 16 {
		return nil, fmt.Errorf("cluster CIDR must be a /16 (got /%d); per-node /24 allocation assumes a /16 pool", ones)
	}
	base := ipnet.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("cluster CIDR %s is not IPv4", clusterCIDR)
	}

	// busy_timeout lets the allocator wait out brief write locks from the
	// workload store, which shares this database file. MaxOpenConns(1)
	// serializes our own statements so an open read cursor never deadlocks
	// a following write on the single connection.
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	a := &SubnetAllocator{db: db, base: base}
	if err := a.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return a, nil
}

func (a *SubnetAllocator) migrate() error {
	if _, err := a.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_subnets (
			node_id TEXT PRIMARY KEY,
			idx     INTEGER NOT NULL UNIQUE,
			subnet  TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create node_subnets table: %w", err)
	}
	return nil
}

// config returns the NetworkConfig for a given node index.
func (a *SubnetAllocator) config(idx int) types.NetworkConfig {
	return types.NetworkConfig{
		Subnet:  fmt.Sprintf("%d.%d.%d.0/24", a.base[0], a.base[1], idx),
		Gateway: fmt.Sprintf("%d.%d.%d.1", a.base[0], a.base[1], idx),
	}
}

// Allocate returns the subnet for nodeID, allocating a fresh one if none
// exists. It is idempotent: an existing allocation is returned unchanged
// (the persistence guarantee). Returns an error if the /24 pool is exhausted.
func (a *SubnetAllocator) Allocate(nodeID string) (types.NetworkConfig, error) {
	if nodeID == "" {
		return types.NetworkConfig{}, fmt.Errorf("node ID cannot be empty")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Existing allocation — return unchanged.
	var idx int
	err := a.db.QueryRow(`SELECT idx FROM node_subnets WHERE node_id = ?`, nodeID).Scan(&idx)
	if err == nil {
		return a.config(idx), nil
	}
	if err != sql.ErrNoRows {
		return types.NetworkConfig{}, fmt.Errorf("lookup subnet for %s: %w", nodeID, err)
	}

	// Read all used indexes, fully draining and closing the cursor before
	// the INSERT below (MaxOpenConns(1) means an open cursor would block it).
	used := make(map[int]bool)
	rows, err := a.db.Query(`SELECT idx FROM node_subnets`)
	if err != nil {
		return types.NetworkConfig{}, fmt.Errorf("list allocated indexes: %w", err)
	}
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			rows.Close()
			return types.NetworkConfig{}, fmt.Errorf("scan index: %w", err)
		}
		used[i] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return types.NetworkConfig{}, fmt.Errorf("iterate indexes: %w", err)
	}

	// Lowest free index in [0, 255].
	free := -1
	for i := 0; i <= 255; i++ {
		if !used[i] {
			free = i
			break
		}
	}
	if free == -1 {
		return types.NetworkConfig{}, fmt.Errorf("subnet pool exhausted: all 256 /24 blocks in use")
	}

	cfg := a.config(free)
	if _, err := a.db.Exec(
		`INSERT INTO node_subnets (node_id, idx, subnet) VALUES (?, ?, ?)`,
		nodeID, free, cfg.Subnet,
	); err != nil {
		return types.NetworkConfig{}, fmt.Errorf("persist subnet for %s: %w", nodeID, err)
	}
	return cfg, nil
}

// Release frees a node's subnet allocation. Call only on explicit
// decommission — never on heartbeat timeout.
func (a *SubnetAllocator) Release(nodeID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.db.Exec(`DELETE FROM node_subnets WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("release subnet for %s: %w", nodeID, err)
	}
	return nil
}

// All returns a map of node ID -> subnet for every current allocation,
// used to build cross-node routing tables.
func (a *SubnetAllocator) All() (map[string]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	rows, err := a.db.Query(`SELECT node_id, subnet FROM node_subnets`)
	if err != nil {
		return nil, fmt.Errorf("list subnets: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var id, subnet string
		if err := rows.Scan(&id, &subnet); err != nil {
			return nil, fmt.Errorf("scan subnet row: %w", err)
		}
		out[id] = subnet
	}
	return out, rows.Err()
}

// Close releases the allocator's database handle.
func (a *SubnetAllocator) Close() error {
	return a.db.Close()
}
