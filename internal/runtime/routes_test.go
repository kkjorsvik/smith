package runtime

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return n
}

func TestRouteManagerManaged(t *testing.T) {
	rm, err := NewRouteManager("10.22.0.0/16", "10.22.3.0/24")
	if err != nil {
		t.Fatalf("NewRouteManager: %v", err)
	}

	cases := []struct {
		name string
		dst  *net.IPNet
		want bool
	}{
		{"default route (nil dst)", nil, false},
		{"unrelated host LAN route", mustCIDR(t, "192.168.1.0/24"), false},
		{"own bridge subnet", mustCIDR(t, "10.22.3.0/24"), false},
		{"peer subnet inside cluster", mustCIDR(t, "10.22.4.0/24"), true},
		{"another peer subnet", mustCIDR(t, "10.22.0.0/24"), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rm.managed(netlink.Route{Dst: c.dst})
			if got != c.want {
				t.Fatalf("managed(%v) = %v, want %v", c.dst, got, c.want)
			}
		})
	}
}
