package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kkjorsvik/smith/internal/api"
	"github.com/kkjorsvik/smith/internal/provision"
	"github.com/kkjorsvik/smith/internal/health"
	"github.com/kkjorsvik/smith/internal/reconciler"
	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/scheduler"
	smithtls "github.com/kkjorsvik/smith/internal/tls"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "gencerts":
			runGenCerts()
			return
		case "add-agent":
			runAddAgent()
			return
		}
	}
	runServer()
}

func runServer() {
	client, err := runtime.NewClient()
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	store, err := reconciler.NewSQLiteStore("/var/lib/smith/state.db")
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Per-node container subnet allocator, persisted in the same state DB.
	// The cluster CIDR is carved into per-node /24 blocks.
	allocator, err := reconciler.NewSubnetAllocator("/var/lib/smith/state.db", runtime.BridgeSubnet)
	if err != nil {
		log.Fatalf("failed to open subnet allocator: %v", err)
	}
	defer allocator.Close()

	// Service definitions + ClusterIP/NodePort allocations, persisted in the
	// same state DB.
	serviceStore, err := reconciler.NewServiceStore("/var/lib/smith/state.db")
	if err != nil {
		log.Fatalf("failed to open service store: %v", err)
	}
	defer serviceStore.Close()

	// Ingress definitions (host -> service), persisted in the same state DB.
	ingressStore, err := reconciler.NewIngressStore("/var/lib/smith/state.db")
	if err != nil {
		log.Fatalf("failed to open ingress store: %v", err)
	}
	defer ingressStore.Close()

	// The control plane runs no workloads, so it has no CNI; pass nil.
	if err := client.CleanupAll(nil); err != nil {
		log.Fatalf("cleanup failed: %v", err)
	}

	reg := registry.New()
	sched := scheduler.New(reg)
	monitor := health.NewMonitor(client)

	// serverTLS — for accepting agent connections on :9443.
	serverTLS, err := smithtls.ServerConfig(
		"/etc/smith/certs/ca.crt",
		"/etc/smith/certs/server.crt",
		"/etc/smith/certs/server.key",
	)
	if err != nil {
		log.Fatalf("load server TLS: %v", err)
	}

	// clientTLS — for the reconciler making outbound HTTPS calls to agents.
	clientTLS, err := smithtls.ClientConfig(
		"/etc/smith/certs/ca.crt",
		"/etc/smith/certs/server.crt",
		"/etc/smith/certs/server.key",
	)
	if err != nil {
		log.Fatalf("load client TLS: %v", err)
	}

	r := reconciler.New(client, store, monitor, reg, sched, clientTLS, 5*time.Second)
	r.Start()

	// agentClient reaches agents over mTLS for log proxying. It has NO
	// timeout, because follow-mode log streaming is a long-lived connection.
	agentClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: clientTLS,
		},
	}

	server := api.New(store, client, reg, sched, ":9443", serverTLS)
	server.SetStatusFunc(r.AggregateStatus)
	server.SetAgentClient(agentClient)
	server.SetSubnetAllocator(allocator)
	server.SetServiceStore(serviceStore)
	server.SetIngressStore(ingressStore)

	if err := server.LoadToken("/etc/smith/token"); err != nil {
		log.Fatalf("load API token: %v", err)
	}

	server.Start()

	log.Println("smith running — press ctrl+c to stop")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig

	log.Println("shutting down...")
	r.Stop()
}

func runGenCerts() {
	fs := flag.NewFlagSet("gencerts", flag.ExitOnError)
	out := fs.String("out", "/etc/smith/certs", "Directory to write certs into")
	hosts := fs.String("hosts", "", "Comma-separated list of agent hostnames/IPs to generate certs for")
	forceCA := fs.Bool("force-ca", false, "Regenerate the CA even if one exists (DESTRUCTIVE: re-keys the whole cluster)")
	fs.Parse(os.Args[2:])

	if err := os.MkdirAll(*out, 0700); err != nil {
		log.Fatalf("gencerts: mkdir %s: %v", *out, err)
	}

	caCert, caKey := loadOrCreateCA(*out, *forceCA)

	// Issue/refresh the server cert against the CA in force.
	generateCert(*out, "server", caCert, caKey, []string{"smith-server-01.kkjorsvik.com"})

	// Issue/refresh agent certs.
	for _, host := range splitHosts(*hosts) {
		generateCert(*out, sanitizeName(host), caCert, caKey, []string{host})
	}

	log.Println("gencerts: done")
}

// loadOrCreateCA reuses an existing ca.crt/ca.key in dir when present (so
// re-running gencerts or adding an agent never re-keys the cluster), and
// generates a fresh CA only when none exists or forceNew is set.
func loadOrCreateCA(dir string, forceNew bool) (*x509.Certificate, *ecdsa.PrivateKey) {
	caCrtPath := filepath.Join(dir, "ca.crt")
	caKeyPath := filepath.Join(dir, "ca.key")

	if !forceNew && fileExists(caCrtPath) && fileExists(caKeyPath) {
		cert, key, err := loadCA(caCrtPath, caKeyPath)
		if err != nil {
			log.Fatalf("gencerts: load existing CA: %v", err)
		}
		log.Printf("gencerts: reusing existing CA at %s", dir)
		return cert, key
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("gencerts: generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Smith CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		log.Fatalf("gencerts: create CA cert: %v", err)
	}
	writePEM(caCrtPath, "CERTIFICATE", caDER)
	writeKeyPEM(caKeyPath, caKey)
	log.Printf("gencerts: wrote new CA to %s", dir)

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		log.Fatalf("gencerts: parse CA cert: %v", err)
	}
	return caCert, caKey
}

// loadCA parses an existing CA cert + EC private key from disk.
func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", certPath, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("decode %s: not PEM", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", certPath, err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", keyPath, err)
	}
	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil {
		return nil, nil, fmt.Errorf("decode %s: not PEM", keyPath)
	}
	key, err := x509.ParseECPrivateKey(kblock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", keyPath, err)
	}
	return cert, key, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// issueCert mints a leaf (clientAuth+serverAuth) signed by ca and returns the
// cert + key as PEM, without touching disk.
func issueCert(ca *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string) (certPEM, keyPEM []byte) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("gencerts: generate key for %s: %v", hosts[0], err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(2 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		log.Fatalf("gencerts: create cert for %s: %v", hosts[0], err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Fatalf("gencerts: marshal key for %s: %v", hosts[0], err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// generateCert issues a leaf and writes it to out/<name>.{crt,key}. The private
// key is written 0600.
func generateCert(out, name string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string) {
	certPEM, keyPEM := issueCert(ca, caKey, hosts)
	if err := os.WriteFile(filepath.Join(out, name+".crt"), certPEM, 0644); err != nil {
		log.Fatalf("gencerts: write %s.crt: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(out, name+".key"), keyPEM, 0600); err != nil {
		log.Fatalf("gencerts: write %s.key: %v", name, err)
	}
	log.Printf("gencerts: wrote cert for %s", name)
}

func writePEM(path, pemType string, der []byte) {
	data := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der})
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Fatalf("gencerts: write %s: %v", path, err)
	}
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Fatalf("gencerts: marshal key: %v", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Fatalf("gencerts: write %s: %v", path, err)
	}
}

// runAddAgent issues one agent leaf against the EXISTING CA and emits a
// self-contained deploy tarball (certs + env + setup script + unit + binary).
// It never regenerates the CA and never bundles ca.key.
func runAddAgent() {
	fs := flag.NewFlagSet("add-agent", flag.ExitOnError)
	host := fs.String("host", "", "Agent hostname (cert SAN), e.g. smith-agent-03.kkjorsvik.com (required)")
	id := fs.String("id", "", "Node id (default: first label of -host)")
	addr := fs.String("addr", "", "Agent mTLS callback addr (default: <host>:9000)")
	server := fs.String("server", "smith-server-01.kkjorsvik.com:9443", "Control-plane internal mTLS addr")
	out := fs.String("out", "/etc/smith/certs", "Cert directory (must already contain the CA)")
	binary := fs.String("binary", "bin/smith-agent", "Path to the smith-agent binary to bundle")
	bundle := fs.String("bundle", "", "Output tarball path (default: ./<id>.tar.gz)")
	fs.Parse(os.Args[2:])

	if *host == "" {
		log.Fatal("add-agent: -host is required")
	}
	nodeID := *id
	if nodeID == "" {
		nodeID = sanitizeName(*host)
	}
	callbackAddr := *addr
	if callbackAddr == "" {
		callbackAddr = *host + ":9000"
	}
	outPath := *bundle
	if outPath == "" {
		outPath = nodeID + ".tar.gz"
	}

	// Require an existing CA — add-agent must never create one (that would
	// orphan every other node). Bootstrap with `gencerts` first.
	caCrtPath := filepath.Join(*out, "ca.crt")
	caKeyPath := filepath.Join(*out, "ca.key")
	if !fileExists(caCrtPath) || !fileExists(caKeyPath) {
		log.Fatalf("add-agent: no CA in %s — run `smith-server gencerts` on the control plane first", *out)
	}
	caCert, caKey, err := loadCA(caCrtPath, caKeyPath)
	if err != nil {
		log.Fatalf("add-agent: load CA: %v", err)
	}

	caCertPEM, err := os.ReadFile(caCrtPath)
	if err != nil {
		log.Fatalf("add-agent: read ca.crt: %v", err)
	}

	// Issue the leaf, persist it next to the CA, and verify it chains before
	// we bundle anything.
	certPEM, keyPEM := issueCert(caCert, caKey, []string{*host})
	if err := os.WriteFile(filepath.Join(*out, nodeID+".crt"), certPEM, 0644); err != nil {
		log.Fatalf("add-agent: write leaf cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(*out, nodeID+".key"), keyPEM, 0600); err != nil {
		log.Fatalf("add-agent: write leaf key: %v", err)
	}
	if err := provision.VerifyLeaf(caCertPEM, certPEM); err != nil {
		log.Fatalf("add-agent: %v", err)
	}

	f, err := os.Create(outPath)
	if err != nil {
		log.Fatalf("add-agent: create bundle %s: %v", outPath, err)
	}
	defer f.Close()

	b := provision.AgentBundle{
		ID:         nodeID,
		Addr:       callbackAddr,
		Server:     *server,
		CACertPEM:  caCertPEM,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		BinaryPath: *binary,
	}
	if err := b.Write(f); err != nil {
		log.Fatalf("add-agent: %v", err)
	}

	fmt.Printf("add-agent: wrote bundle %s for %s (%s)\n", outPath, nodeID, *host)
	fmt.Printf("  scp %s <newhost>:~/\n", outPath)
	fmt.Printf("  ssh <newhost> 'tar xzf %s && cd %s && sudo ./setup.sh'\n", filepath.Base(outPath), nodeID)
}

func splitHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

func sanitizeName(host string) string {
	// Use the first label of the hostname as the cert filename.
	// e.g. "smith-agent-01.kkjorsvik.com" -> "smith-agent-01"
	for i, c := range host {
		if c == '.' {
			return host[:i]
		}
	}
	return host
}
