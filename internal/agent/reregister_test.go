package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

// TestHeartbeatReRegistersAfterControlPlaneRestart simulates a control-plane
// restart: the heartbeat endpoint 404s (the in-memory registry forgot the node)
// until the agent re-registers, after which heartbeats succeed again.
func TestHeartbeatReRegistersAfterControlPlaneRestart(t *testing.T) {
	var mu sync.Mutex
	registered := false // flips true once the node (re-)registers
	registerCalls := 0

	mux := http.NewServeMux()
	mux.HandleFunc("POST /nodes/register", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		registered = true
		registerCalls++
		mu.Unlock()
		json.NewEncoder(w).Encode(types.NetworkConfig{Subnet: "10.22.3.0/24", Gateway: "10.22.3.1"})
	})
	mux.HandleFunc("POST /nodes/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := registered
		mu.Unlock()
		if !ok {
			http.Error(w, "unknown node", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	a := &Agent{
		id:         "smith-agent-03",
		serverAddr: strings.TrimPrefix(srv.URL, "https://"),
		httpClient: srv.Client(),
		netCfg:     types.NetworkConfig{Subnet: "10.22.3.0/24", Gateway: "10.22.3.1"},
	}

	// Control plane has restarted and forgotten us: the heartbeat 404s with the
	// errNodeUnknown sentinel.
	err := a.sendHeartbeat()
	if !errors.Is(err, errNodeUnknown) {
		t.Fatalf("expected errNodeUnknown on 404 heartbeat, got %v", err)
	}

	// The loop's recovery path: re-register, then the next heartbeat succeeds.
	a.reRegister()
	if err := a.sendHeartbeat(); err != nil {
		t.Fatalf("heartbeat after re-register should succeed, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if registerCalls != 1 {
		t.Fatalf("expected exactly 1 re-registration, got %d", registerCalls)
	}
}

// TestSendHeartbeatOK confirms a 200 heartbeat is not mistaken for the
// node-unknown case.
func TestSendHeartbeatOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /nodes/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	a := &Agent{
		id:         "smith-agent-03",
		serverAddr: strings.TrimPrefix(srv.URL, "https://"),
		httpClient: srv.Client(),
	}
	if err := a.sendHeartbeat(); err != nil {
		t.Fatalf("healthy heartbeat returned error: %v", err)
	}
}
