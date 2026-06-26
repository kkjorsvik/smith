// Package secrets decrypts SOPS-encrypted env overlays by shelling out to the
// sops binary. The apply engine uses it to merge secret env into a workload at
// apply time. sops handles age-key discovery via its own conventions
// (SOPS_AGE_KEY_FILE or ~/.config/sops/age/keys.txt).
package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"sigs.k8s.io/yaml"
)

// SopsDecryptor decrypts overlay files using the `sops` binary on PATH.
type SopsDecryptor struct{}

// overlay is the decrypted shape of a *.sops.yaml file: just an env map.
type overlay struct {
	Env map[string]string `json:"env"`
}

// Decrypt runs `sops --decrypt` on path and returns its env map. It returns a
// clear error if sops is not installed, if decryption fails, or if the
// decrypted content is not an {env: map} document.
func (SopsDecryptor) Decrypt(path string) (map[string]string, error) {
	cmd := exec.Command("sops", "--decrypt", "--output-type", "yaml", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("'sops' is not installed (PATH)")
		}
		return nil, fmt.Errorf("sops decrypt failed: %s", strings.TrimSpace(stderr.String()))
	}
	return parseOverlay(stdout.Bytes())
}

// parseOverlay decodes decrypted sops YAML (an {env: map} document) into a flat
// env map, rejecting any other shape.
func parseOverlay(data []byte) (map[string]string, error) {
	var o overlay
	if err := yaml.UnmarshalStrict(data, &o); err != nil {
		return nil, fmt.Errorf("parse overlay: %w", err)
	}
	return o.Env, nil
}
