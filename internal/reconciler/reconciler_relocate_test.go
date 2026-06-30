package reconciler

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/types"
)

// recordingAgent is a fake agent that records the method+path of each request.
func recordingAgent() (*httptest.Server, *[]string, *sync.Mutex) {
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return srv, &hits, &mu
}

func addr(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "https://") }

// relocate must stop the replica on its old node and start it on the new one,
// so a re-fit move doesn't leave a duplicate container (two writers on a
// volume-backed workload).
func TestRelocateStopsOldNodeAndStartsNew(t *testing.T) {
	oldSrv, oldHits, oldMu := recordingAgent()
	defer oldSrv.Close()
	newSrv, newHits, newMu := recordingAgent()
	defer newSrv.Close()

	reg := registry.New()
	reg.Register(types.Node{ID: "old", Addr: addr(oldSrv)})
	reg.Register(types.Node{ID: "new", Addr: addr(newSrv)})
	newNode, _ := reg.Get("new")

	r := &Reconciler{
		registry:   reg,
		httpClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}

	if err := r.relocate("old", newNode, types.Workload{ID: "app-0"}); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	oldMu.Lock()
	defer oldMu.Unlock()
	newMu.Lock()
	defer newMu.Unlock()
	if want := "DELETE /assign/app-0"; len(*oldHits) != 1 || (*oldHits)[0] != want {
		t.Fatalf("old node hits = %v, want exactly [%q]", *oldHits, want)
	}
	if want := "POST /assign"; len(*newHits) != 1 || (*newHits)[0] != want {
		t.Fatalf("new node hits = %v, want exactly [%q]", *newHits, want)
	}
}

// If the old node is no longer registered (e.g. removed), relocate still starts
// the replica on the new node and reports no error.
func TestRelocateWhenOldNodeGoneStartsNew(t *testing.T) {
	newSrv, newHits, newMu := recordingAgent()
	defer newSrv.Close()

	reg := registry.New()
	reg.Register(types.Node{ID: "new", Addr: addr(newSrv)})
	newNode, _ := reg.Get("new")

	r := &Reconciler{
		registry:   reg,
		httpClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}

	if err := r.relocate("gone", newNode, types.Workload{ID: "app-0"}); err != nil {
		t.Fatalf("relocate with missing old node should not error: %v", err)
	}
	newMu.Lock()
	defer newMu.Unlock()
	if len(*newHits) != 1 || (*newHits)[0] != "POST /assign" {
		t.Fatalf("new node hits = %v, want exactly [POST /assign]", *newHits)
	}
}
