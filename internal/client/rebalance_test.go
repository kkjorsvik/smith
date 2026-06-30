package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRebalancePlan(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"replica_id":"postgres-0","from_node":"n1","to_node":"n2"}]`))
	}))
	defer srv.Close()

	c := New(Config{Server: srv.URL, Token: "tok"})
	moves, err := c.RebalancePlan()
	if err != nil {
		t.Fatalf("RebalancePlan: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/rebalance" {
		t.Errorf("got %s %s, want GET /rebalance", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if len(moves) != 1 || moves[0].ReplicaID != "postgres-0" || moves[0].FromNode != "n1" || moves[0].ToNode != "n2" {
		t.Errorf("moves = %+v", moves)
	}
}

func TestRebalance(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"replica_id":"grafana-0","from_node":"n1","to_node":"n3"}]`))
	}))
	defer srv.Close()

	c := New(Config{Server: srv.URL, Token: "t"})
	moves, err := c.Rebalance()
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/rebalance" {
		t.Errorf("got %s %s, want POST /rebalance", gotMethod, gotPath)
	}
	if len(moves) != 1 || moves[0].ReplicaID != "grafana-0" || moves[0].ToNode != "n3" {
		t.Errorf("moves = %+v", moves)
	}
}

func TestRebalanceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	c := New(Config{Server: srv.URL, Token: "t"})
	if _, err := c.Rebalance(); err == nil {
		t.Fatal("expected error on 500")
	}
}
