package runtime

import "testing"

func TestClientIPTracking(t *testing.T) {
	c := &Client{ips: make(map[string]string)}

	// Unknown container has no IP.
	if got := c.getIP("a"); got != "" {
		t.Fatalf("getIP(a) = %q, want empty", got)
	}

	// setIP records, getIP returns it.
	c.setIP("a", "10.22.1.7")
	if got := c.getIP("a"); got != "10.22.1.7" {
		t.Fatalf("getIP(a) = %q, want 10.22.1.7", got)
	}

	// Empty IP is a no-op (CNI-disabled containers don't pollute the map).
	c.setIP("b", "")
	if got := c.getIP("b"); got != "" {
		t.Fatalf("getIP(b) after empty set = %q, want empty", got)
	}

	// clearIP forgets it.
	c.clearIP("a")
	if got := c.getIP("a"); got != "" {
		t.Fatalf("getIP(a) after clear = %q, want empty", got)
	}

	// clearIP on an unknown container is harmless.
	c.clearIP("nonexistent")
}
