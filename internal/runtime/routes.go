package runtime

import (
	"fmt"
	"log"
	"net"

	"github.com/kkjorsvik/smith/internal/types"
	"github.com/vishvananda/netlink"
)

// RouteManager installs and reconciles the static routes that make other
// nodes' container subnets reachable from this node. It only ever touches
// routes whose destination is inside the cluster CIDR and is not this node's
// own subnet — host, default, and unrelated routes are never modified.
type RouteManager struct {
	cluster   *net.IPNet // the cluster pool, e.g. 10.22.0.0/16
	ownSubnet string     // this node's /24 in CIDR string form
}

// NewRouteManager returns a RouteManager for the given cluster CIDR and this
// node's own container subnet (which is left untouched during reconciliation).
func NewRouteManager(clusterCIDR, ownSubnet string) (*RouteManager, error) {
	_, cluster, err := net.ParseCIDR(clusterCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse cluster CIDR %s: %w", clusterCIDR, err)
	}
	_, own, err := net.ParseCIDR(ownSubnet)
	if err != nil {
		return nil, fmt.Errorf("parse own subnet %s: %w", ownSubnet, err)
	}
	return &RouteManager{cluster: cluster, ownSubnet: own.String()}, nil
}

// Sync reconciles kernel routes to match the desired set: it adds/updates a
// route per entry and removes smith-managed routes no longer present (e.g.
// for a decommissioned node). It is idempotent and safe to call on a ticker.
func (rm *RouteManager) Sync(routes []types.Route) error {
	// Desired: peer subnet (CIDR string) -> gateway IP.
	desired := make(map[string]net.IP, len(routes))
	for _, r := range routes {
		_, dst, err := net.ParseCIDR(r.Subnet)
		if err != nil {
			log.Printf("warn: route sync: bad subnet %q: %v", r.Subnet, err)
			continue
		}
		gw := net.ParseIP(r.Via)
		if gw == nil {
			log.Printf("warn: route sync: bad via %q for %s", r.Via, r.Subnet)
			continue
		}
		desired[dst.String()] = gw
	}

	// Install/update desired routes. RouteReplace is idempotent — it adds if
	// missing and updates the gateway if changed, so unchanged routes are a
	// no-op each tick.
	for dstStr, gw := range desired {
		_, dst, _ := net.ParseCIDR(dstStr)
		if err := netlink.RouteReplace(&netlink.Route{Dst: dst, Gw: gw}); err != nil {
			log.Printf("warn: route replace %s via %s: %v", dstStr, gw, err)
		}
	}

	// Remove smith-managed routes that are no longer desired.
	existing, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list routes: %w", err)
	}
	for i := range existing {
		route := existing[i]
		if !rm.managed(route) {
			continue
		}
		if _, ok := desired[route.Dst.String()]; ok {
			continue
		}
		if err := netlink.RouteDel(&route); err != nil {
			log.Printf("warn: route del %s: %v", route.Dst.String(), err)
		} else {
			log.Printf("routes: removed stale route to %s", route.Dst.String())
		}
	}

	return nil
}

// managed reports whether a route is one smith owns: destination inside the
// cluster pool and not this node's own (bridge-connected) subnet.
func (rm *RouteManager) managed(route netlink.Route) bool {
	if route.Dst == nil {
		return false
	}
	if !rm.cluster.Contains(route.Dst.IP) {
		return false
	}
	return route.Dst.String() != rm.ownSubnet
}
