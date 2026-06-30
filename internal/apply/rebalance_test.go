package apply

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

type fakeRebalancer struct {
	plan            []types.Move
	enacted         []types.Move
	planCalled      bool
	rebalanceCalled bool
}

func (f *fakeRebalancer) RebalancePlan() ([]types.Move, error) {
	f.planCalled = true
	return f.plan, nil
}

func (f *fakeRebalancer) Rebalance() ([]types.Move, error) {
	f.rebalanceCalled = true
	return f.enacted, nil
}

// Without --apply, Rebalance previews the plan: it queries the plan (not the
// enacting endpoint), prints the moves, and hints how to enact.
func TestRebalanceDryRunPrintsPlan(t *testing.T) {
	f := &fakeRebalancer{plan: []types.Move{{ReplicaID: "postgres-0", FromNode: "n1", ToNode: "n2"}}}
	var out bytes.Buffer
	if err := Rebalance(f, false, &out); err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if !f.planCalled || f.rebalanceCalled {
		t.Fatalf("dry-run should call RebalancePlan only (plan=%v enact=%v)", f.planCalled, f.rebalanceCalled)
	}
	s := out.String()
	for _, want := range []string{"postgres-0", "n1", "n2", "--apply"} {
		if !strings.Contains(s, want) {
			t.Fatalf("plan output missing %q; got:\n%s", want, s)
		}
	}
}

// With --apply, Rebalance enacts: it calls the enacting endpoint (not the plan)
// and does not print the dry-run hint.
func TestRebalanceApplyEnacts(t *testing.T) {
	f := &fakeRebalancer{enacted: []types.Move{{ReplicaID: "grafana-0", FromNode: "n1", ToNode: "n3"}}}
	var out bytes.Buffer
	if err := Rebalance(f, true, &out); err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if !f.rebalanceCalled || f.planCalled {
		t.Fatalf("apply should call Rebalance only (plan=%v enact=%v)", f.planCalled, f.rebalanceCalled)
	}
	s := out.String()
	if !strings.Contains(s, "grafana-0") {
		t.Fatalf("apply output missing moved replica; got:\n%s", s)
	}
	if strings.Contains(s, "--apply") {
		t.Fatalf("apply output should not print the dry-run hint; got:\n%s", s)
	}
}

// An empty plan reports a balanced cluster and is not an error.
func TestRebalanceNoMoves(t *testing.T) {
	f := &fakeRebalancer{}
	var out bytes.Buffer
	if err := Rebalance(f, false, &out); err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "balanced") {
		t.Fatalf("empty plan should report balanced; got:\n%s", out.String())
	}
}
