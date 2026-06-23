// Package provision builds the self-contained tarball an operator scp's to a
// fresh agent box: the agent's certs, an env file, the setup script, the
// systemd unit, and the smith-agent binary. The setup script and unit are
// embedded here so smith-server always emits a bundle consistent with itself.
package provision

import (
	"archive/tar"
	"compress/gzip"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	_ "embed"
)

// Fixed mtime for tar entries: bundles should be byte-reproducible for a given
// input, and nothing on the agent box depends on the file timestamps.
var epoch = time.Unix(0, 0)

//go:embed assets/setup-agent.sh
var setupAgentScript []byte

//go:embed assets/smith-agent.service
var agentUnit []byte

// AgentBundle is everything needed to build one agent's deploy tarball.
type AgentBundle struct {
	ID         string // node id, e.g. "smith-agent-03" (also the cert file basename)
	Addr       string // mTLS callback addr, e.g. "smith-agent-03.kkjorsvik.com:9000"
	Server     string // control-plane internal addr, e.g. "smith-server-01.kkjorsvik.com:9443"
	CACertPEM  []byte // ca.crt (trust anchor — never the key)
	CertPEM    []byte // this agent's leaf cert
	KeyPEM     []byte // this agent's leaf key (written 0600 in the tar)
	BinaryPath string // path to the smith-agent binary to embed
}

// agentEnv renders the EnvironmentFile that drives the systemd unit. The cert
// paths are the on-box destinations the setup script installs to.
func (b AgentBundle) agentEnv() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "SMITH_ID=%s\n", b.ID)
	fmt.Fprintf(&sb, "SMITH_ADDR=%s\n", b.Addr)
	fmt.Fprintf(&sb, "SMITH_SERVER=%s\n", b.Server)
	fmt.Fprintf(&sb, "SMITH_CA=/etc/smith/certs/ca.crt\n")
	fmt.Fprintf(&sb, "SMITH_CERT=/etc/smith/certs/%s.crt\n", b.ID)
	fmt.Fprintf(&sb, "SMITH_KEY=/etc/smith/certs/%s.key\n", b.ID)
	return sb.String()
}

// Write streams the gzip'd tarball to w. Every entry lives under a top-level
// "<id>/" directory so the operator untars into a clean folder.
func (b AgentBundle) Write(w io.Writer) error {
	if b.ID == "" {
		return fmt.Errorf("provision: empty agent id")
	}
	binary, err := os.ReadFile(b.BinaryPath)
	if err != nil {
		return fmt.Errorf("provision: read agent binary: %w", err)
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	dir := b.ID + "/"
	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{dir + "ca.crt", 0o644, b.CACertPEM},
		{dir + b.ID + ".crt", 0o644, b.CertPEM},
		{dir + b.ID + ".key", 0o600, b.KeyPEM},
		{dir + "agent.env", 0o600, []byte(b.agentEnv())},
		{dir + "setup.sh", 0o755, setupAgentScript},
		{dir + "smith-agent.service", 0o644, agentUnit},
		{dir + "smith-agent", 0o755, binary},
	}

	for _, f := range files {
		hdr := &tar.Header{
			Name: f.name,
			Mode: f.mode,
			Size:    int64(len(f.data)),
			ModTime: epoch,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("provision: tar header %s: %w", f.name, err)
		}
		if _, err := tw.Write(f.data); err != nil {
			return fmt.Errorf("provision: tar write %s: %w", f.name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("provision: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("provision: close gzip: %w", err)
	}
	return nil
}

// VerifyLeaf is a sanity check used by add-agent: confirm the freshly-issued
// leaf actually chains to the CA we bundled, so a broken bundle fails loudly at
// creation time rather than on the agent box.
func VerifyLeaf(caCertPEM, certPEM []byte) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caCertPEM) {
		return fmt.Errorf("provision: parse CA cert")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("provision: parse leaf cert")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("provision: parse leaf: %w", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("provision: leaf does not chain to CA: %w", err)
	}
	return nil
}
