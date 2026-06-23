package runtime

import (
	"encoding/json"
	"testing"
)

func TestRenderBridgeConflist(t *testing.T) {
	raw, err := renderBridgeConflist("10.22.3.0/24", "10.22.3.1")
	if err != nil {
		t.Fatalf("renderBridgeConflist: %v", err)
	}

	var conflist struct {
		CNIVersion string `json:"cniVersion"`
		Name       string `json:"name"`
		Plugins    []struct {
			Type   string `json:"type"`
			Bridge string `json:"bridge"`
			IPMasq bool   `json:"ipMasq"`
			IPAM   struct {
				Type   string          `json:"type"`
				Ranges [][]struct {
					Subnet  string `json:"subnet"`
					Gateway string `json:"gateway"`
				} `json:"ranges"`
			} `json:"ipam"`
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &conflist); err != nil {
		t.Fatalf("generated conflist is not valid JSON: %v\n%s", err, raw)
	}

	if conflist.Name != "smith" || conflist.CNIVersion != cniVersion {
		t.Fatalf("unexpected name/version: %+v", conflist)
	}
	if len(conflist.Plugins) != 2 {
		t.Fatalf("want 2 plugins (bridge, portmap), got %d", len(conflist.Plugins))
	}

	bridge := conflist.Plugins[0]
	if bridge.Type != "bridge" || bridge.Bridge != bridgeName {
		t.Fatalf("bridge plugin wrong: %+v", bridge)
	}
	if bridge.IPMasq {
		t.Fatal("ipMasq must be false so the firewall handles egress masquerade selectively")
	}
	if bridge.IPAM.Type != "host-local" {
		t.Fatalf("ipam type = %q, want host-local", bridge.IPAM.Type)
	}
	if len(bridge.IPAM.Ranges) != 1 || len(bridge.IPAM.Ranges[0]) != 1 {
		t.Fatalf("unexpected ipam ranges shape: %+v", bridge.IPAM.Ranges)
	}
	r := bridge.IPAM.Ranges[0][0]
	if r.Subnet != "10.22.3.0/24" || r.Gateway != "10.22.3.1" {
		t.Fatalf("ipam range = %+v, want subnet 10.22.3.0/24 gateway 10.22.3.1", r)
	}

	portmap := conflist.Plugins[1]
	if portmap.Type != "portmap" || !portmap.Capabilities["portMappings"] {
		t.Fatalf("portmap plugin wrong: %+v", portmap)
	}
}
