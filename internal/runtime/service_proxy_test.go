package runtime

import (
	"strings"
	"testing"
)

func TestServiceChain(t *testing.T) {
	a := serviceChain("web")
	b := serviceChain("web")
	c := serviceChain("api")

	if a != b {
		t.Fatalf("serviceChain not deterministic: %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("serviceChain collided for distinct names: %s", a)
	}
	if !strings.HasPrefix(a, "SMITH-SVC-") {
		t.Fatalf("chain %s missing SMITH-SVC- prefix", a)
	}
	// iptables chain names must be <= 28 characters.
	if len(a) > 28 {
		t.Fatalf("chain name %q is %d chars, exceeds iptables 28-char limit", a, len(a))
	}
}
