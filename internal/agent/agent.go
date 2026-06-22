package agent

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	goruntime "runtime"
	"sync"
	"time"

	smithruntime "github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/types"
)

const (
	heartbeatInterval = 10 * time.Second
	serverTimeout     = 5 * time.Second
)

// Agent runs on a worker node, registers with the control plane,
// and manages containers assigned to this node.
type Agent struct {
	id         string
	addr       string
	serverAddr string
	client     *smithruntime.Client
	stop       chan struct{}
	httpClient *http.Client
	serverTLS  *tls.Config
	cni        *smithruntime.CNI
	firewall   *smithruntime.Firewall
	mu         sync.Mutex
	ports      map[string][]types.PortMapping // workloadID -> ports
}

// New returns an Agent.
// id is this node's unique name (e.g. "smith-agent-01").
// addr is this agent's HTTP address the control plane will call back to (e.g. "192.168.1.55:9000").
// serverAddr is the control plane's internal mTLS address (e.g. "smith-server-01.kkjorsvik.com:9443").
// clientTLS authenticates this agent's outbound calls to the control plane;
// serverTLS requires and verifies client certs on this agent's inbound server.
func New(id, addr, serverAddr string, client *smithruntime.Client, clientTLS, serverTLS *tls.Config) *Agent {
	httpClient := &http.Client{
		Timeout: serverTimeout,
		Transport: &http.Transport{
			TLSClientConfig: clientTLS,
		},
	}

	// CNI is best-effort: if it can't initialize (no config in
	// /etc/cni/net.d, missing plugins), the agent still runs and serves
	// workloads without port mappings.
	cni, err := smithruntime.NewCNI()
	if err != nil {
		log.Printf("agent: CNI init failed, port mapping disabled: %v", err)
		cni = nil
	}

	// Firewall is best-effort, like CNI: if iptables is unavailable the
	// agent still runs, but the operator must manage FORWARD/INPUT rules.
	fw, err := smithruntime.NewFirewall(smithruntime.BridgeSubnet)
	if err != nil {
		log.Printf("agent: firewall init failed, port management disabled: %v", err)
		fw = nil
	}

	return &Agent{
		id:         id,
		addr:       addr,
		serverAddr: serverAddr,
		client:     client,
		stop:       make(chan struct{}),
		httpClient: httpClient,
		serverTLS:  serverTLS,
		cni:        cni,
		firewall:   fw,
		ports:      make(map[string][]types.PortMapping),
	}
}

// Start registers with the control plane, starts the heartbeat loop,
// and starts the agent's HTTP server in goroutines.
func (a *Agent) Start() error {
	if err := a.register(); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	if a.firewall != nil {
		if err := a.firewall.EnsureForwarding(); err != nil {
			log.Printf("agent: ensure forwarding: %v", err)
		}
	}

	go a.heartbeatLoop()
	go a.serveHTTP(a.serverTLS)

	return nil
}

// Stop signals the agent to shut down.
func (a *Agent) Stop() {
	close(a.stop)
}

// register sends a registration request to the control plane.
func (a *Agent) register() error {
	hostname, _ := os.Hostname()
	node := types.Node{
		ID:       a.id,
		Addr:     a.addr,
		CPU:      getCPUCount(),
		MemoryMB: getMemoryMB(),
	}
	_ = hostname

	body, err := json.Marshal(node)
	if err != nil {
		return fmt.Errorf("marshal node: %w", err)
	}

	url := fmt.Sprintf("https://%s/nodes/register", a.serverAddr)
	resp, err := a.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("post register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register failed: status %d", resp.StatusCode)
	}

	log.Printf("agent: registered with control plane at %s", a.serverAddr)
	return nil
}

// heartbeatLoop sends a heartbeat to the control plane every heartbeatInterval.
func (a *Agent) heartbeatLoop() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := a.sendHeartbeat(); err != nil {
				log.Printf("agent: heartbeat failed: %v", err)
			}
		case <-a.stop:
			return
		}
	}
}

// sendHeartbeat notifies the control plane this node is still alive.
func (a *Agent) sendHeartbeat() error {
	url := fmt.Sprintf("https://%s/nodes/%s/heartbeat", a.serverAddr, a.id)
	resp, err := a.httpClient.Post(url, "application/json", nil)
	if err != nil {
		return fmt.Errorf("post heartbeat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat failed: status %d", resp.StatusCode)
	}

	return nil
}

// serveHTTP starts the agent's HTTP server so the control plane can
// send assignments and query observed state.
func (a *Agent) serveHTTP(tlsCfg *tls.Config) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /assign", a.handleAssign)
	mux.HandleFunc("DELETE /assign/{id}", a.handleUnassign)
	mux.HandleFunc("GET /status", a.handleStatus)

	server := &http.Server{
		Addr:      a.addr,
		Handler:   mux,
		TLSConfig: tlsCfg,
	}

	log.Printf("agent: listening on %s", a.addr)
	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("agent: http server: %v", err)
	}
}

// handleAssign receives a workload assignment from the control plane
// and starts the container locally.
func (a *Agent) handleAssign(w http.ResponseWriter, r *http.Request) {
	var wl types.Workload
	if err := json.NewDecoder(r.Body).Decode(&wl); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("agent: received assignment for %s", wl.ID)

	a.mu.Lock()
	a.ports[wl.ID] = wl.Ports
	a.mu.Unlock()

	if a.firewall != nil && len(wl.Ports) > 0 {
		if err := a.firewall.OpenPorts(wl.Ports); err != nil {
			log.Printf("agent: open ports for %s: %v", wl.ID, err)
		}
	}

	go func() {
		image, err := a.client.GetImage(wl.Image)
		if err != nil {
			image, err = a.client.PullImage(wl.Image)
			if err != nil {
				log.Printf("agent: pull %s: %v", wl.ID, err)
				return
			}
		}

		code, err := a.client.RunContainer(smithruntime.RunOptions{
			ID:    wl.ID,
			Image: image,
			Args:  wl.Args,
			Ports: wl.Ports,
			Env:   wl.Env,
			CNI:   a.cni,
		})
		if err != nil {
			if smithruntime.ErrAlreadyExists(err) {
				return
			}
			log.Printf("agent: run %s: %v", wl.ID, err)
			return
		}

		log.Printf("agent: %s exited (code %d)", wl.ID, code)
	}()

	w.WriteHeader(http.StatusOK)
}

// handleUnassign receives a removal request from the control plane
// and stops the container locally.
func (a *Agent) handleUnassign(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	log.Printf("agent: received unassign for %s", id)

	a.mu.Lock()
	ports := a.ports[id]
	delete(a.ports, id)
	a.mu.Unlock()

	if err := a.client.StopContainer(id, a.cni, ports); err != nil {
		log.Printf("agent: stop %s: %v", id, err)
	}

	if a.firewall != nil && len(ports) > 0 {
		a.firewall.ClosePorts(ports)
	}

	w.WriteHeader(http.StatusOK)
}

// handleStatus returns the observed state of all containers on this node.
func (a *Agent) handleStatus(w http.ResponseWriter, r *http.Request) {
	observed, err := a.client.ListRunning()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(observed)
}

// getCPUCount returns the number of logical CPUs available.
func getCPUCount() int {
	return goruntime.NumCPU()
}

// getMemoryMB returns total system memory in megabytes.
func getMemoryMB() int {
	// Read from /proc/meminfo
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}

	var total uint64
	fmt.Sscanf(string(data), "MemTotal: %d kB", &total)
	return int(total / 1024)
}
