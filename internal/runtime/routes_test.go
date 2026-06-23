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

	gw := net.ParseIP("192.168.1.50")

	cases := []struct {
		name string
		dst  *net.IPNet
		gw   net.IP
		want bool
	}{
		{"default route (nil dst)", nil, gw, false},
		{"unrelated host LAN route", mustCIDR(t, "192.168.1.0/24"), gw, false},
		{"own bridge subnet", mustCIDR(t, "10.22.3.0/24"), gw, false},
		{"cluster /16 aggregate", mustCIDR(t, "10.22.0.0/16"), gw, false},
		{"peer /24 without gateway (connected)", mustCIDR(t, "10.22.4.0/24"), nil, false},
		{"peer subnet inside cluster", mustCIDR(t, "10.22.4.0/24"), gw, true},
		{"another peer subnet", mustCIDR(t, "10.22.0.0/24"), gw, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rm.managed(netlink.Route{Dst: c.dst, Gw: c.gw})
			if got != c.want {
				t.Fatalf("managed(%v, gw=%v) = %v, want %v", c.dst, c.gw, got, c.want)
			}
		})
	}
}
