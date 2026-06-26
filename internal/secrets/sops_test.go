package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOverlayValid(t *testing.T) {
	env, err := parseOverlay([]byte("env:\n  POSTGRES_PASSWORD: secret\n  API_KEY: k\n"))
	if err != nil {
		t.Fatalf("parseOverlay: %v", err)
	}
	if env["POSTGRES_PASSWORD"] != "secret" || env["API_KEY"] != "k" {
		t.Errorf("env = %v", env)
	}
}

func TestParseOverlayRejectsUnknownKey(t *testing.T) {
	// The decrypted overlay must be an {env: map} document; anything else is a
	// mistake we want surfaced.
	_, err := parseOverlay([]byte("secrets:\n  X: y\n"))
	if err == nil {
		t.Fatal("expected error for non-env document, got nil")
	}
}

func TestDecryptSopsNotInstalled(t *testing.T) {
	// Empty PATH so the sops binary cannot be found; Decrypt must say so clearly.
	t.Setenv("PATH", "")
	f := filepath.Join(t.TempDir(), "x.sops.yaml")
	if err := os.WriteFile(f, []byte("env: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := SopsDecryptor{}.Decrypt(f)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want 'not installed'", err)
	}
}
