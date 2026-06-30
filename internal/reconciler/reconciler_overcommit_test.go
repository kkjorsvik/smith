package reconciler

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/scheduler"
	"github.com/kkjorsvik/smith/internal/types"
)

// captureLog redirects the standard logger to a buffer for the duration of fn.
func captureLog(fn func()) string {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	fn()
	return buf.String()
}

// logOvercommit must warn (with node ID, committed, and cap) for any node whose
// committed requests exceed its schedulable capacity — e.g. a node that rejoins
// reporting less memory than the load already on it.
func TestLogOvercommitWarnsForOverCappedNode(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 4000}) // cap 3400
	s := scheduler.New(reg)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.Assign(id+"-0", id, types.Resources{MemoryMB: 1000}); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
	}
	// n1 rejoins with less RAM: cap drops to 2550, below the committed 3000.
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 3000})

	r := &Reconciler{scheduler: s}
	out := captureLog(r.logOvercommit)

	for _, want := range []string{"overcommit", "n1", "3000", "2550"} {
		if !strings.Contains(out, want) {
			t.Fatalf("overcommit warning missing %q; got: %s", want, out)
		}
	}
}

func TestLogOvercommitSilentWhenWithinCap(t *testing.T) {
	reg := registry.New()
	reg.Register(types.Node{ID: "n1", CPU: 4, MemoryMB: 4000})
	s := scheduler.New(reg)
	if _, err := s.Assign("a-0", "a", types.Resources{MemoryMB: 1000}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	r := &Reconciler{scheduler: s}
	if out := captureLog(r.logOvercommit); out != "" {
		t.Fatalf("expected no log output within cap, got: %s", out)
	}
}
