package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestApplyWorkloadPostsWithAuth(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBody types.Workload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(Config{Server: srv.URL, Token: "tok123"})
	if err := c.ApplyWorkload(types.Workload{ID: "alpha", Image: "nginx"}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/workloads" {
		t.Errorf("path = %q, want /workloads", gotPath)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("auth = %q, want Bearer tok123", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if gotBody.ID != "alpha" || gotBody.Image != "nginx" {
		t.Errorf("body = %+v, want id=alpha image=nginx", gotBody)
	}
}

func TestApplyServiceAndIngressPaths(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	// Trailing slash must be trimmed.
	c := New(Config{Server: srv.URL + "/", Token: "tok123"})
	if err := c.ApplyService(types.Service{Name: "alpha", Port: 80}); err != nil {
		t.Fatalf("ApplyService: %v", err)
	}
	if err := c.ApplyIngress(types.Ingress{Host: "alpha.example.com", Service: "alpha"}); err != nil {
		t.Fatalf("ApplyIngress: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/services" || paths[1] != "/ingresses" {
		t.Errorf("paths = %v, want [/services /ingresses]", paths)
	}
}

func TestApplyErrorIncludesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad workload"}`))
	}))
	defer srv.Close()

	c := New(Config{Server: srv.URL, Token: "tok123"})
	err := c.ApplyWorkload(types.Workload{ID: "alpha"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad workload") {
		t.Errorf("error %q does not contain body", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q does not contain status", err)
	}
}

func TestApplyUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer srv.Close()

	c := New(Config{Server: srv.URL, Token: "bad"})
	err := c.ApplyWorkload(types.Workload{ID: "alpha"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("error %q does not mention unauthorized", err)
	}
}
