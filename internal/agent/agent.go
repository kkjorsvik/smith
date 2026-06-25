package agent

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
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
	svcProxy   *smithruntime.ServiceProxy
	ingress    *ingressProxy
	netCfg     types.NetworkConfig // assigned at registration
	nfsSource  string              // cluster NFS share for volumes (SMITH_NFS_SOURCE), or ""
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
		nfsSource:  os.Getenv("SMITH_NFS_SOURCE"),
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

		// Service proxy programs load-balancing rules for services.
		sp, err := smithruntime.NewServiceProxy()
		if err != nil {
			log.Printf("agent: service proxy init failed, services disabled: %v", err)
		} else {
			a.svcProxy = sp
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

	// Mount the cluster NFS share once, if configured, so stateful workloads
	// can bind-mount their volume directories from it. Best-effort: without it,
	// only stateless workloads run on this node.
	if a.nfsSource != "" {
		if err := smithruntime.MountNFS(a.nfsSource); err != nil {
			log.Printf("agent: mount NFS %s failed, stateful workloads disabled: %v", a.nfsSource, err)
		} else {
			log.Printf("agent: NFS share %s mounted at %s", a.nfsSource, smithruntime.NFSMountRoot)
		}
	}

	// Clear ghost containers left by a previous unclean shutdown, releasing
	// their CNI IP allocations, before serving assignments. Uses the
	// subnet-derived CNI built above.
	if err := a.client.CleanupAll(a.cni); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}

	// Remove any stale bridge so CNI setup isn't blocked by a leftover smith0
	// whose IP doesn't match this run's assigned subnet. Containers are gone
	// after CleanupAll, so the bridge has no attached veths.
	if a.cni != nil {
		if err := smithruntime.RemoveBridge(); err != nil {
			log.Printf("agent: remove stale bridge: %v", err)
		}
	}

	go a.heartbeatLoop()
	go a.serveHTTP(a.serverTLS)
	go a.routeSyncLoop()
	go a.serviceSyncLoop()

	// Ingress reverse proxy: needs cluster networking (service ClusterIPs) to
	// reach backends, so only when a subnet was assigned.
	if netCfg.Subnet != "" {
		a.startIngress()
	}

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

// errNodeUnknown means the control plane returned 404 for our heartbeat: it no
// longer has us in its node registry. The registry is in-memory, so this is the
// signature of a control-plane restart — the cure is to re-register.
var errNodeUnknown = errors.New("control plane does not recognize this node")

// heartbeatLoop sends a heartbeat to the control plane every heartbeatInterval.
// A 404 means the control plane lost our registration (it restarted — the node
// registry is in-memory), so we re-announce ourselves; the cluster then
// re-converges within one interval without restarting the agent.
func (a *Agent) heartbeatLoop() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := a.sendHeartbeat(); err != nil {
				log.Printf("agent: heartbeat failed: %v", err)
				if errors.Is(err, errNodeUnknown) {
					a.reRegister()
				}
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

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("heartbeat: %w", errNodeUnknown)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat failed: status %d", resp.StatusCode)
	}

	return nil
}

// reRegister re-announces this node after a heartbeat revealed the control
// plane no longer knows it (typically a control-plane restart). The container
// subnet is allocated idempotently and persisted server-side, so the node keeps
// its existing /24 and the already-configured CNI/routes/firewall need no
// change. A failure here just retries on the next heartbeat tick.
func (a *Agent) reRegister() {
	netCfg, err := a.register()
	if err != nil {
		log.Printf("agent: re-registration failed: %v (will retry next heartbeat)", err)
		return
	}
	if netCfg.Subnet != "" && a.netCfg.Subnet != "" && netCfg.Subnet != a.netCfg.Subnet {
		log.Printf("agent: WARNING re-registration returned subnet %s but this node is configured for %s; restart the agent to reconfigure networking", netCfg.Subnet, a.netCfg.Subnet)
		return
	}
	log.Printf("agent: re-registered with control plane after it lost our registration")
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

// serviceSyncLoop periodically pulls the service load-balancing table from the
// control plane and reconciles iptables rules, so new/changed services and
// replica churn take effect within one interval.
func (a *Agent) serviceSyncLoop() {
	if a.svcProxy == nil {
		return
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	a.syncServices() // sync once immediately on start
	for {
		select {
		case <-ticker.C:
			a.syncServices()
		case <-a.stop:
			return
		}
	}
}

// syncServices fetches and applies the service table once.
func (a *Agent) syncServices() {
	services, err := a.fetchServices()
	if err != nil {
		log.Printf("agent: fetch services: %v", err)
		return
	}
	if err := a.svcProxy.Sync(services); err != nil {
		log.Printf("agent: sync services: %v", err)
	}
}

// fetchServices pulls the service load-balancing table from the control plane.
func (a *Agent) fetchServices() ([]types.ServiceEndpoints, error) {
	url := fmt.Sprintf("https://%s/nodes/%s/services", a.serverAddr, a.id)
	resp, err := a.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("services failed: status %d", resp.StatusCode)
	}

	var services []types.ServiceEndpoints
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		return nil, fmt.Errorf("decode services: %w", err)
	}
	return services, nil
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

	// Resolve persistent volumes to host bind mounts under the NFS share. The
	// volume dir is keyed by the parent workload ID so it is stable across the
	// replica's recreation, rolling updates, and node failover.
	mounts, err := a.resolveVolumes(wl)
	if err != nil {
		log.Printf("agent: volumes for %s: %v", wl.ID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
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
			Mounts:    mounts,
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

// replicaSuffixRe matches the trailing "-<index>" a replica ID carries, so the
// parent workload ID can be recovered for stable per-workload volume paths.
var replicaSuffixRe = regexp.MustCompile(`-\d+$`)

// resolveVolumes turns a workload's persistent volumes into host bind mounts
// under the NFS share. Returns an error (failing the assign) if the workload
// declares volumes but this node has no NFS share mounted.
func (a *Agent) resolveVolumes(wl types.Workload) ([]smithruntime.Mount, error) {
	if len(wl.Volumes) == 0 {
		return nil, nil
	}
	if a.nfsSource == "" {
		return nil, fmt.Errorf("workload %s has volumes but this node has no NFS share (SMITH_NFS_SOURCE unset)", wl.ID)
	}

	baseID := replicaSuffixRe.ReplaceAllString(wl.ID, "")
	mounts := make([]smithruntime.Mount, 0, len(wl.Volumes))
	for _, v := range wl.Volumes {
		hostPath, err := smithruntime.EnsureVolumeDir(baseID, v.Name)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, smithruntime.Mount{Source: hostPath, Dest: v.Path})
	}
	return mounts, nil
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
