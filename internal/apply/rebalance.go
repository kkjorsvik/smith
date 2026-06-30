package apply

import (
	"fmt"
	"io"

	"github.com/kkjorsvik/smith/internal/types"
)

// Rebalancer is the control-plane surface Rebalance needs: preview the moves a
// rebalance would make, or enact them. *client.Client satisfies it.
type Rebalancer interface {
	RebalancePlan() ([]types.Move, error)
	Rebalance() ([]types.Move, error)
}

// Rebalance previews (doApply=false) or enacts (doApply=true) a scheduler
// rebalance, printing the moves to out. Preview hints how to enact; an empty
// plan reports a balanced cluster.
func Rebalance(r Rebalancer, doApply bool, out io.Writer) error {
	var (
		moves []types.Move
		err   error
	)
	if doApply {
		moves, err = r.Rebalance()
	} else {
		moves, err = r.RebalancePlan()
	}
	if err != nil {
		return err
	}

	if len(moves) == 0 {
		fmt.Fprintln(out, "Cluster is balanced; no moves needed.")
		return nil
	}

	if doApply {
		fmt.Fprintf(out, "Rebalanced (%d move(s)):\n", len(moves))
	} else {
		fmt.Fprintf(out, "Rebalance plan (%d move(s)):\n", len(moves))
	}
	for _, m := range moves {
		fmt.Fprintf(out, "  %s: %s -> %s\n", m.ReplicaID, m.FromNode, m.ToNode)
	}
	if !doApply {
		fmt.Fprintln(out, "\nRun 'smithctl rebalance --apply' to enact.")
	}
	return nil
}
