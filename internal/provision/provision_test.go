package provision

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCA returns a self-signed CA and issues one leaf for host, all as PEM.
func testCA(t *testing.T, host string) (caCertPEM, leafCertPEM, leafKeyPEM []byte) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	caCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	leafCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	leafKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caCertPEM, leafCertPEM, leafKeyPEM
}

func TestVerifyLeaf(t *testing.T) {
	ca, leaf, _ := testCA(t, "smith-agent-03.kkjorsvik.com")
	if err := VerifyLeaf(ca, leaf); err != nil {
		t.Fatalf("valid leaf failed to verify: %v", err)
	}

	// A leaf from a different CA must not verify against this CA.
	otherCA, _, _ := testCA(t, "x")
	if err := VerifyLeaf(otherCA, leaf); err == nil {
		t.Fatal("leaf from a different CA verified — should have failed")
	}
}

func TestAgentBundleContents(t *testing.T) {
	ca, leaf, key := testCA(t, "smith-agent-03.kkjorsvik.com")

	// A stand-in binary so Write has something to embed.
	dir := t.TempDir()
	binPath := filepath.Join(dir, "smith-agent")
	if err := os.WriteFile(binPath, []byte("#!/bin/true\n"), 0755); err != nil {
		t.Fatal(err)
	}

	b := AgentBundle{
		ID:         "smith-agent-03",
		Addr:       "smith-agent-03.kkjorsvik.com:9000",
		Server:     "smith-server-01.kkjorsvik.com:9443",
		CACertPEM:  ca,
		CertPEM:    leaf,
		KeyPEM:     key,
		BinaryPath: binPath,
	}
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}

	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)

	got := map[string]int64{}    // name -> mode
	content := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		got[hdr.Name] = hdr.Mode
		data, _ := io.ReadAll(tr)
		content[hdr.Name] = data
	}

	want := map[string]int64{
		"smith-agent-03/ca.crt":              0o644,
		"smith-agent-03/smith-agent-03.crt":  0o644,
		"smith-agent-03/smith-agent-03.key":  0o600,
		"smith-agent-03/agent.env":           0o600,
		"smith-agent-03/setup.sh":            0o755,
		"smith-agent-03/smith-agent.service": 0o644,
		"smith-agent-03/smith-agent":         0o755,
	}
	for name, mode := range want {
		m, ok := got[name]
		if !ok {
			t.Errorf("bundle missing %s", name)
			continue
		}
		if m != mode {
			t.Errorf("%s mode = %o, want %o", name, m, mode)
		}
	}
	if len(got) != len(want) {
		t.Errorf("bundle has %d entries, want %d: %v", len(got), len(want), got)
	}

	// The CA private key must never be bundled.
	for name := range got {
		if filepath.Base(name) == "ca.key" {
			t.Fatal("bundle contains ca.key — must never leave the control plane")
		}
	}

	// env file wires the systemd unit to the right paths.
	env := string(content["smith-agent-03/agent.env"])
	for _, want := range []string{
		"SMITH_ID=smith-agent-03",
		"SMITH_ADDR=smith-agent-03.kkjorsvik.com:9000",
		"SMITH_SERVER=smith-server-01.kkjorsvik.com:9443",
		"SMITH_CERT=/etc/smith/certs/smith-agent-03.crt",
		"SMITH_KEY=/etc/smith/certs/smith-agent-03.key",
	} {
		if !bytes.Contains([]byte(env), []byte(want)) {
			t.Errorf("agent.env missing %q; got:\n%s", want, env)
		}
	}
}
