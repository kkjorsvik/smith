package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/kkjorsvik/smith/internal/reconciler"
	"github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/types"
)

// Server exposes smith's desired and observed state over HTTP.
type Server struct {
	store  reconciler.Storer
	client *runtime.Client
	addr   string
}

// New returns a Server bound to addr.
func New(store reconciler.Storer, client *runtime.Client, addr string) *Server {
	return &Server{
		store:  store,
		client: client,
		addr:   addr,
	}
}

// Start registers routes and begins listening in a goroutine.
func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workloads", s.listWorkloads)
	mux.HandleFunc("POST /workloads", s.addWorkload)
	mux.HandleFunc("DELETE /workloads/{id}", s.removeWorkload)
	mux.HandleFunc("GET /status", s.status)

	go func() {
		log.Printf("api: listening on %s", s.addr)
		if err := http.ListenAndServe(s.addr, mux); err != nil {
			log.Fatalf("api: %v", err)
		}
	}()
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
	observed, err := s.client.ListRunning()
	if err != nil {
		httpError(w, fmt.Errorf("list running: %w", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, observed)
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
