package api

import "testing"

func TestPublicDomain(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"unset falls back to default", "", DefaultPublicDomain},
		{"whitespace falls back to default", "   ", DefaultPublicDomain},
		{"override is honored and trimmed", "  smith.example.org  ", "smith.example.org"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SMITH_PUBLIC_DOMAIN", tc.env)
			if got := PublicDomain(); got != tc.want {
				t.Errorf("PublicDomain() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIngressZone(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"unset falls back to default", "", defaultIngressZone},
		{"whitespace falls back to default", "  ", defaultIngressZone},
		{"override is honored and trimmed", " example.org ", "example.org"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SMITH_INGRESS_DOMAIN", tc.env)
			if got := ingressZone(); got != tc.want {
				t.Errorf("ingressZone() = %q, want %q", got, tc.want)
			}
		})
	}
}
