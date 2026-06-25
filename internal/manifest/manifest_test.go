package manifest

import "testing"

func TestParseMinimalBundle(t *testing.T) {
	data := []byte(`
name: deployops
workload:
  image: git.kkjorsvik.com/kydovik/deployops:2026.06.24
  replicas: 2
service:
  port: 8080
ingress:
  host: deployops.kkjorsvik.com
`)
	app, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if app.Name != "deployops" {
		t.Errorf("Name = %q, want deployops", app.Name)
	}
	if app.Workload.Image != "git.kkjorsvik.com/kydovik/deployops:2026.06.24" {
		t.Errorf("Image = %q", app.Workload.Image)
	}
	if app.Workload.Replicas != 2 {
		t.Errorf("Replicas = %d, want 2", app.Workload.Replicas)
	}
	if app.Service == nil || app.Service.Port != 8080 {
		t.Errorf("Service = %+v, want port 8080", app.Service)
	}
	if app.Ingress == nil || app.Ingress.Host != "deployops.kkjorsvik.com" {
		t.Errorf("Ingress = %+v", app.Ingress)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	data := []byte(`
name: deployops
workload:
  image: nginx
  replicaz: 2
`)
	if _, err := Parse(data); err == nil {
		t.Fatal("expected error for unknown field replicaz, got nil")
	}
}
