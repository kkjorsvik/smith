package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	acmedns "github.com/kkjorsvik/smith/internal/acme"
	"github.com/kkjorsvik/smith/internal/reconciler"
	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/scheduler"
	"github.com/kkjorsvik/smith/internal/types"
	"golang.org/x/crypto/acme"
)

// Server exposes smith's desired and observed state over HTTP.
type Server struct {
	store        reconciler.Storer
	client       *runtime.Client
	registry     *registry.Registry
	scheduler    *scheduler.Scheduler
	internalAddr string
	tlsCfg       *tls.Config
	statusFunc   func() map[string]map[string]runtime.ContainerStatus
	token        string
	agentClient  *http.Client
}

// New returns a Server with a public API (hardcoded on :443 via autocert)
// and an internal mTLS API on internalAddr.
func New(store reconciler.Storer, client *runtime.Client, reg *registry.Registry, sched *scheduler.Scheduler, internalAddr string, tlsCfg *tls.Config) *Server {
	return &Server{
		store:        store,
		client:       client,
		registry:     reg,
		scheduler:    sched,
		internalAddr: internalAddr,
		tlsCfg:       tlsCfg,
	}
}

// SetStatusFunc wires in the function used to aggregate cluster-wide
// container status (provided by the reconciler).
func (s *Server) SetStatusFunc(f func() map[string]map[string]runtime.ContainerStatus) {
	s.statusFunc = f
}

// SetAgentClient wires in the mTLS HTTP client used to reach agents
// (for log proxying). It must not have a Timeout set, since follow-mode
// log streaming is a long-lived connection.
func (s *Server) SetAgentClient(c *http.Client) {
	s.agentClient = c
}

// LoadToken reads the API bearer token from the given file path.
// The token is trimmed of surrounding whitespace. If the file is
// missing or empty, returns an error — the server should refuse to
// start the public API without a token.
func (s *Server) LoadToken(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return fmt.Errorf("token file %s is empty", path)
	}
	s.token = token
	return nil
}

// requireAuth wraps a handler with bearer token authentication.
// Expects an "Authorization: Bearer <token>" header matching the
// configured token.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			httpError(w, fmt.Errorf("missing or malformed Authorization header"), http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(auth, prefix)
		// Constant-time comparison to avoid timing attacks.
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			httpError(w, fmt.Errorf("invalid token"), http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// Start registers routes and begins listening in goroutines.
func (s *Server) Start() {
	// Public mux — workload API over HTTPS via autocert.
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /workloads", s.requireAuth(s.listWorkloads))
	publicMux.HandleFunc("POST /workloads", s.requireAuth(s.addWorkload))
	publicMux.HandleFunc("DELETE /workloads/{id}", s.requireAuth(s.removeWorkload))
	publicMux.HandleFunc("GET /status", s.requireAuth(s.status))
	publicMux.HandleFunc("GET /nodes", s.requireAuth(s.listNodes))
	publicMux.HandleFunc("GET /assignments", s.requireAuth(s.listAssignments))
	publicMux.HandleFunc("GET /workloads/{id}/logs", s.requireAuth(s.workloadLogs))

	// Internal mux — node registration and heartbeat over mTLS.
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("POST /nodes/register", s.registerNode)
	internalMux.HandleFunc("POST /nodes/{id}/heartbeat", s.heartbeat)

	// Internal mTLS server for agent communication.
	internalServer := &http.Server{
		Addr:      s.internalAddr,
		Handler:   internalMux,
		TLSConfig: s.tlsCfg,
	}

	// Public HTTPS server with a DNS-01 (Route 53) provisioned cert. The
	// server is behind NAT and port 80 is unreachable from the internet, so
	// HTTP-01 cannot be used.
	go func() {
		tlsCfg, err := s.provisionCert()
		if err != nil {
			log.Fatalf("api: provision cert: %v", err)
		}

		publicServer := &http.Server{
			Addr:      ":443",
			Handler:   publicMux,
			TLSConfig: tlsCfg,
		}

		log.Printf("api: public HTTPS listening on :443")
		if err := publicServer.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("api: public server: %v", err)
		}
	}()

	go func() {
		log.Printf("api: internal mTLS listening on %s", s.internalAddr)
		if err := internalServer.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("api: internal server: %v", err)
		}
	}()
}

// provisionCert obtains or loads a cached TLS cert for the server's
// public hostname using ACME DNS-01 via Route 53.
func (s *Server) provisionCert() (*tls.Config, error) {
	const domain = "smith-server-01.kkjorsvik.com"
	certFile := "/var/lib/smith/autocert/server.crt"
	keyFile := "/var/lib/smith/autocert/server.key"

	// Use cached cert if it exists.
	if _, err := os.Stat(certFile); err == nil {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err == nil {
			log.Printf("api: loaded cached cert for %s", domain)
			return &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS13,
			}, nil
		}
	}

	log.Printf("api: provisioning cert for %s via DNS-01", domain)

	ctx := context.Background()

	// Look up the Route 53 zone ID automatically.
	zoneID, err := acmedns.GetZoneID(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("get zone ID: %w", err)
	}
	log.Printf("api: found Route 53 zone ID: %s", zoneID)

	solver, err := acmedns.NewRoute53Solver(ctx, zoneID)
	if err != nil {
		return nil, fmt.Errorf("create solver: %w", err)
	}

	// Generate a key for the ACME account.
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate account key: %w", err)
	}

	client := &acme.Client{
		Key:          accountKey,
		DirectoryURL: acme.LetsEncryptURL,
	}

	// Register with Let's Encrypt.
	_, err = client.Register(ctx, &acme.Account{}, acme.AcceptTOS)
	if err != nil && err != acme.ErrAccountAlreadyExists {
		return nil, fmt.Errorf("register ACME account: %w", err)
	}

	// Create an order for the domain.
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return nil, fmt.Errorf("authorize order: %w", err)
	}

	// Process each authorization in the order.
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return nil, fmt.Errorf("get authorization: %w", err)
		}

		// Skip already valid authorizations.
		if authz.Status == acme.StatusValid {
			continue
		}

		// Find the DNS-01 challenge.
		var chal *acme.Challenge
		for _, c := range authz.Challenges {
			if c.Type == "dns-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return nil, fmt.Errorf("no dns-01 challenge for %s", authz.Identifier.Value)
		}

		// Get the TXT record value.
		keyAuth, err := client.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return nil, fmt.Errorf("dns01 key auth: %w", err)
		}

		// Present the TXT record in Route 53.
		if err := solver.Present(ctx, chal, authz.Identifier.Value, chal.Token, keyAuth); err != nil {
			return nil, fmt.Errorf("present challenge: %w", err)
		}
		defer solver.CleanUp(ctx, chal, authz.Identifier.Value, chal.Token, keyAuth)

		// Tell Let's Encrypt to validate.
		if _, err := client.Accept(ctx, chal); err != nil {
			return nil, fmt.Errorf("accept challenge: %w", err)
		}

		// Wait for this authorization to complete.
		if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
			return nil, fmt.Errorf("wait authorization for %s: %w", authz.Identifier.Value, err)
		}
	}

	// Generate the server key and CSR.
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate cert key: %w", err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{DNSNames: []string{domain}},
		certKey,
	)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}

	// Wait for the order to be ready then issue the cert.
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("wait order: %w", err)
	}

	derChain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("create order cert: %w", err)
	}

	// Cache the cert and key to disk.
	if err := os.MkdirAll("/var/lib/smith/autocert", 0700); err != nil {
		return nil, fmt.Errorf("mkdir autocert: %w", err)
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return nil, fmt.Errorf("create cert file: %w", err)
	}
	for _, der := range derChain {
		pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	certOut.Close()

	keyDER, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return nil, fmt.Errorf("marshal cert key: %w", err)
	}
	keyOut, err := os.Create(keyFile)
	if err != nil {
		return nil, fmt.Errorf("create key file: %w", err)
	}
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyOut.Close()

	log.Printf("api: cert provisioned and cached for %s", domain)

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load provisioned cert: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// listWorkloads returns all workloads in the desired state store.
func (s *Server) listWorkloads(w http.ResponseWriter, r *http.Request) {
	workloads, err := s.store.List()
	if err != nil {
		httpError(w, fmt.Errorf("list workloads: %w", err), http.StatusInternalServerError)
		return
	}

	// Convert map to slice for a cleaner JSON response.
	out := make([]types.Workload, 0, len(workloads))
	for _, wl := range workloads {
		out = append(out, wl)
	}

	writeJSON(w, http.StatusOK, out)
}

// addWorkload inserts a workload into the desired state store.
func (s *Server) addWorkload(w http.ResponseWriter, r *http.Request) {
	var wl types.Workload
	if err := json.NewDecoder(r.Body).Decode(&wl); err != nil {
		httpError(w, fmt.Errorf("decode body: %w", err), http.StatusBadRequest)
		return
	}

	if err := s.store.Add(wl); err != nil {
		httpError(w, fmt.Errorf("add workload: %w", err), http.StatusBadRequest)
		return
	}

	log.Printf("api: added workload %s", wl.ID)
	writeJSON(w, http.StatusCreated, wl)
}

// removeWorkload deletes a workload from the desired state store.
// The reconciler will stop the container on the next tick.
func (s *Server) removeWorkload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httpError(w, fmt.Errorf("missing id"), http.StatusBadRequest)
		return
	}

	if err := s.store.Remove(id); err != nil {
		httpError(w, fmt.Errorf("remove workload: %w", err), http.StatusInternalServerError)
		return
	}

	log.Printf("api: removed workload %s", id)
	w.WriteHeader(http.StatusNoContent)
}

// status returns the observed state — what containerd reports as running.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if s.statusFunc == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, s.statusFunc())
}

// registerNode handles agent registration requests.
func (s *Server) registerNode(w http.ResponseWriter, r *http.Request) {
	var node types.Node
	if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
		httpError(w, fmt.Errorf("decode body: %w", err), http.StatusBadRequest)
		return
	}

	s.registry.Register(node)
	log.Printf("api: node registered: %s at %s", node.ID, node.Addr)
	w.WriteHeader(http.StatusOK)
}

// heartbeat handles agent heartbeat requests.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httpError(w, fmt.Errorf("missing id"), http.StatusBadRequest)
		return
	}

	if err := s.registry.Heartbeat(id); err != nil {
		httpError(w, err, http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// listNodes returns all registered nodes and their status.
func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes := s.registry.List()
	writeJSON(w, http.StatusOK, nodes)
}

// listAssignments returns all current workload->node assignments.
func (s *Server) listAssignments(w http.ResponseWriter, r *http.Request) {
	assignments := s.scheduler.ListAssignments()
	writeJSON(w, http.StatusOK, assignments)
}

// workloadLogs proxies a workload's container logs from the agent it is
// assigned to, streaming bytes back to the client. It propagates the
// follow query parameter and tears down the upstream connection when the
// client disconnects (via the request context).
func (s *Server) workloadLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httpError(w, fmt.Errorf("missing id"), http.StatusBadRequest)
		return
	}

	assignment, ok := s.scheduler.GetAssignment(id)
	if !ok {
		httpError(w, fmt.Errorf("workload %s not assigned to any node", id), http.StatusNotFound)
		return
	}

	node, ok := s.registry.Get(assignment.NodeID)
	if !ok {
		httpError(w, fmt.Errorf("node %s not found", assignment.NodeID), http.StatusNotFound)
		return
	}

	if s.agentClient == nil {
		httpError(w, fmt.Errorf("agent client not configured"), http.StatusInternalServerError)
		return
	}

	follow := r.URL.Query().Get("follow")
	url := fmt.Sprintf("https://%s/logs/%s", node.Addr, id)
	if follow == "true" {
		url += "?follow=true"
	}

	// Build an upstream request tied to the client's context so that
	// when the client disconnects, the upstream connection is cancelled.
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		httpError(w, fmt.Errorf("build upstream request: %w", err), http.StatusInternalServerError)
		return
	}

	resp, err := s.agentClient.Do(upReq)
	if err != nil {
		httpError(w, fmt.Errorf("connect to agent %s: %w", node.ID, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, fmt.Errorf("streaming unsupported"), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// writeJSON encodes v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

// httpError writes a JSON error response.
func httpError(w http.ResponseWriter, err error, status int) {
	log.Printf("api: error %d: %v", status, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
