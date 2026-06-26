package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a config file into a fresh temp dir and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigValid(t *testing.T) {
	path := writeConfig(t, "server: https://cp.example.com\ntoken: tok123\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "https://cp.example.com" {
		t.Errorf("Server = %q, want https://cp.example.com", cfg.Server)
	}
	if cfg.Token != "tok123" {
		t.Errorf("Token = %q, want tok123", cfg.Token)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "no config at") {
		t.Errorf("error %q does not mention missing config", err)
	}
}

func TestLoadConfigMissingServer(t *testing.T) {
	path := writeConfig(t, "token: tok123\n")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing server")
	}
	if !strings.Contains(err.Error(), "server is required") {
		t.Errorf("error %q does not mention server", err)
	}
}

func TestLoadConfigMissingToken(t *testing.T) {
	path := writeConfig(t, "server: https://cp.example.com\n")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if !strings.Contains(err.Error(), "token is required") {
		t.Errorf("error %q does not mention token", err)
	}
}
