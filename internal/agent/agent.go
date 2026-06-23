package agent

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
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
	hostIP     string
	serverAddr string
	client     *smithruntime.Client
	stop       chan struct{}
	httpClient *http.Client
	serverTLS  *tls.Config
	cni        *smithruntime.CNI
	firewall   *smithruntime.Firewall
	routeMgr   *smithruntime.RouteManager
	netCfg     types.NetworkConfig // assigned at registration
	mu         sync.Mutex
	ports      map[string][]types.PortMapping // workloadID -> ports
}

// New returns an Agent.
// id is this node's unique name (e.g. "smith-agent-01").
// addr is this agent's HTTP address the control plane will call back to (e.g. "192.168.1.55:9000").
// hostIP is the underlay IP other nodes route container traffic through.
// serverAddr is the control plane's internal mTLS address (e.g. "smith-server-01.kkjorsvik.com:9443").
// clientTLS authenticates this agent's outbound calls to the control plane;
// serverTLS requires and verifies client certs on this agent's inbound server.
func New(id, addr, hostIP, serverAddr string, client *smithruntime.Client, clientTLS, serverTLS *tls.Config) *Agent {
	httpClient := &http.Client{
		Timeout: serverTimeout,
		Transport: &http.Transport{
			TLSClientConfig: clientTLS,
		},
	}

	// Firewall is best-effort: if iptables is unavailable the agent still
	// runs, but the operator must manage FORWARD/INPUT/nat rules. CNI is
	// initialized later in Start(), once registration assigns this node's
	// subnet to build the bridge from.
	fw, err := smithruntime.NewFirewall(smithruntime.BridgeSubnet)
	if err != nil {
		log.Printf("agent: firewall init failed, port management disabled: %v", err)
		fw = nil
	}

	return &Agent{
		id:         id,
		addr:       addr,
		hostIP:     hostIP,
		serverAddr: serverAddr,
		client:     client,
		stop:       make(chan struct{}),
		httpClient: httpClient,
		serverTLS:  serverTLS,
		firewall:   fw,
		ports:      make(map[string][]types.PortMapping),
	}
}

// Start registers with the control plane, starts the heartbeat loop,
// and starts the agent's HTTP server in goroutines.
func (a *Agent) Start() error {
	// Register first (retrying until the control plane is reachable):
	// registration assigns this node's container subnet, which the CNI
	// bridge is built from. A control-plane restart must not wedge agents,
	// so we retry with backoff rather than failing.
	netCfg, err := a.registerWithRetry()
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	a.netCfg = netCfg

	// Build the CNI bridge from the assigned subnet. If the control plane
	// returned no subnet (older server), fall back to the static
	// /etc/cni/net.d config so the agent still functions.
	if netCfg.Subnet != "" {
		cni, err := smithruntime.NewCNIForSubnet(netCfg.Subnet, netCfg.Gateway)
		if err != nil {
			log.Printf("agent: CNI init for subnet %s failed, port mapping disabled: %v", netCfg.Subnet, err)
			cni = nil
		}
		a.cni = cni

		// Route manager installs routes to peer subnets (Commit 3).
		rm, err := smithruntime.NewRouteManager(smithruntime.BridgeSubnet, netCfg.Subnet)
		if err != nil {
			log.Printf("agent: route manager init failed, cross-node routing disabled: %v", err)
		} else {
			a.routeMgr = rm
		}
	} else {
		log.Printf("agent: control plane assigned no subnet; falling back to static CNI config")
		cni, err := smithruntime.NewCNI()
		if err != nil {
			log.Printf("agent: static CNI init failed, port mapping disabled: %v", err)
			cni = nil
		}
		a.cni = cni
	}

	// Host network setup: forwarding rules, egress-only masquerade, and IP
	// forwarding so cross-node container traffic is routed.
	if a.firewall != nil {
		if err := a.firewall.EnsureForwarding(); err != nil {
			log.Printf("agent: ensure forwarding: %v", err)
		}
		if err := a.firewall.EnsureMasquerade(smithruntime.BridgeSubnet); err != nil {
			log.Printf("agent: ensure masquerade: %v", err)
		}
	}
	if err := smithruntime.EnableIPForwarding(); err != nil {
		log.Printf("agent: enable ip_forward: %v", err)
	}

	// Clear ghost containers left by a previous unclean shutdown, releasing
	// their CNI IP allocations, before serving assignments. Uses the
	// subnet-derived CNI built above.
	if err := a.client.CleanupAll(a.cni); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}

	go a.heartbeatLoop()
	go a.serveHTTP(a.serverTLS)
	go a.routeSyncLoop()

	return nil
}

// Stop signals the agent to shut down.
func (a *Agent) Stop() {
	close(a.stop)
}

// registerWithRetry calls register, retrying with exponential backoff until
// it succeeds or the agent is stopped. Registration assigns this node's
// subnet, so a transient control-plane outage at boot must not permanently
// wedge the agent.
func (a *Agent) registerWithRetry() (types.NetworkConfig, error) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for attempt := 1; ; attempt++ {
		netCfg, err := a.register()
		if err == nil {
			return netCfg, nil
		}

		log.Printf("agent: registration attempt %d failed: %v; retrying in %s", attempt, err, backoff)
		select {
		case <-time.After(backoff):
		case <-a.stop:
			return types.NetworkConfig{}, fmt.Errorf("registration cancelled during shutdown")
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// register sends a registration request to the control plane and returns the
// network config (subnet/gateway) the control plane assigned this node.
func (a *Agent) register() (types.NetworkConfig, error) {
	node := types.Node{
		ID:       a.id,
		Addr:     a.addr,
		HostIP:   a.hostIP,
		CPU:      getCPUCount(),
		MemoryMB: getMemoryMB(),
	}

	body, err := json.Marshal(node)
	if err != nil {
		return types.NetworkConfig{}, fmt.Errorf("marshal node: %w", err)
	}

	url := fmt.Sprintf("https://%s/nodes/register", a.serverAddr)
	resp, err := a.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return types.NetworkConfig{}, fmt.Errorf("post register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return types.NetworkConfig{}, fmt.Errorf("register failed: status %d", resp.StatusCode)
	}

	// Decode the assigned network config. Tolerate an empty body from an
	// older control plane that doesn't allocate subnets yet.
	var netCfg types.NetworkConfig
	if err := json.NewDecoder(resp.Body).Decode(&netCfg); err != nil && err != io.EOF {
		return types.NetworkConfig{}, fmt.Errorf("decode network config: %w", err)
	}

	log.Printf("agent: registered with control plane at %s (subnet %q)", a.serverAddr, netCfg.Subnet)
	return netCfg, nil
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

// routeSyncLoop periodically pulls this node's cross-node routing table from
// the control plane and reconciles kernel routes, so a newly-added node's
// subnet becomes reachable automatically within one interval.
func (a *Agent) routeSyncLoop() {
	if a.routeMgr == nil {
		return
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	a.syncRoutes() // sync once immediately on start
	for {
		select {
		case <-ticker.C:
			a.syncRoutes()
		case <-a.stop:
			return
		}
	}
}

// syncRoutes fetches and applies the routing table once.
func (a *Agent) syncRoutes() {
	routes, err := a.fetchRoutes()
	if err != nil {
		log.Printf("agent: fetch routes: %v", err)
		return
	}
	if err := a.routeMgr.Sync(routes); err != nil {
		log.Printf("agent: sync routes: %v", err)
	}
}

// fetchRoutes pulls this node's routing table from the control plane.
func (a *Agent) fetchRoutes() ([]types.Route, error) {
	url := fmt.Sprintf("https://%s/nodes/%s/routes", a.serverAddr, a.id)
	resp, err := a.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("get routes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("routes failed: status %d", resp.StatusCode)
	}

	var routes []types.Route
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return nil, fmt.Errorf("decode routes: %w", err)
	}
	return routes, nil
}

// serveHTTP starts the agent's HTTP server so the control plane can
// send assignments and query observed state.
func (a *Agent) serveHTTP(tlsCfg *tls.Config) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /assign", a.handleAssign)
	mux.HandleFunc("DELETE /assign/{id}", a.handleUnassign)
	mux.HandleFunc("GET /status", a.handleStatus)
	mux.HandleFunc("GET /logs/{id}", a.handleLogs)

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
			ID:        wl.ID,
			Image:     image,
			Args:      wl.Args,
			Ports:     wl.Ports,
			Env:       wl.Env,
			Resources: wl.Resources,
			CNI:       a.cni,
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

// handleLogs streams a container's log file to the client. With
// ?follow=true it keeps the connection open and streams new content as
// it is written (like tail -f), until the client disconnects.
func (a *Agent) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	logPath := smithruntime.LogPath(id)
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "no logs for "+id, http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)

	follow := r.URL.Query().Get("follow") == "true"

	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			flusher.Flush()
		}
		if err == io.EOF {
			if !follow {
				return
			}
			// In follow mode, wait for more data or client disconnect.
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		if err != nil && err != io.EOF {
			return
		}
	}
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
