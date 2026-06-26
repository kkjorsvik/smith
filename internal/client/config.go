// Package client provides the smithctl operator's view of the smith control
// plane: a kubeconfig-style config loader and a bearer-auth HTTP client with
// Apply methods for workloads, services, and ingresses.
package client

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Config is the smithctl client configuration: the control-plane base URL and
// the bearer token used to authenticate API requests.
type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// DefaultConfigPath returns the default config location, ~/.config/smith/config.
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "smith", "config"), nil
}

// LoadConfig reads and validates the config at path. A missing file yields a
// friendly, actionable error; both server and token are required.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("no config at %s; set server and token", path)
		}
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if cfg.Server == "" {
		return Config{}, fmt.Errorf("config %s: server is required", path)
	}
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("config %s: token is required", path)
	}
	return cfg, nil
}
