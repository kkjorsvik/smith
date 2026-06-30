package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/scheduler"
	"github.com/kkjorsvik/smith/internal/types"
)

// overcommittedScheduler packs six 1000MB replicas onto n1, then shrinks n1 so
// it is overcommitted and a rebalance has work to do.
func overcommittedScheduler(t *testing.T) *scheduler.Scheduler {
	t.Helper()
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 8000})
	reg.Register(types.Node{ID: "n2", CPU: 4, MemoryMB: 8000})
	reg.Register(types.Node{ID: "n3", CPU: 4, MemoryMB: 8000})
	s := scheduler.New(reg)
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		if _, err := s.Assign(id+"-0", id, types.Resources{MemoryMB: 1000}); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
	}
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 5000})
	return s
}

// GET /rebalance returns the plan as JSON and does not mutate placement.
func TestRebalancePlanHandler(t *testing.T) {
	sched := overcommittedScheduler(t)
	s := &Server{scheduler: sched}

	rec := httptest.NewRecorder()
	s.rebalancePlan(rec, httptest.NewRequest(http.MethodGet, "/rebalance", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var moves []types.Move
	if err := json.NewDecoder(rec.Body).Decode(&moves); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(moves) == 0 {
		t.Fatal("expected a non-empty plan for an overcommitted cluster")
	}
	// Pure: placement unchanged after a plan request.
	if over := sched.Overcommitted(); len(over) == 0 {
		t.Fatal("GET /rebalance must not enact moves (node should still be overcommitted)")
	}
}

// POST /rebalance enacts the moves and returns them.
func TestRebalanceHandler(t *testing.T) {
	sched := overcommittedScheduler(t)
	s := &Server{scheduler: sched}

	rec := httptest.NewRecorder()
	s.rebalance(rec, httptest.NewRequest(http.MethodPost, "/rebalance", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var moves []types.Move
	if err := json.NewDecoder(rec.Body).Decode(&moves); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(moves) == 0 {
		t.Fatal("expected enacted moves")
	}
	// Committed: the node is no longer overcommitted.
	if over := sched.Overcommitted(); len(over) != 0 {
		t.Fatalf("POST /rebalance should have drained the node, still over: %+v", over)
	}
}
