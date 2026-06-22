package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
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
	"github.com/kkjorsvik/smith/internal/health"
	"github.com/kkjorsvik/smith/internal/reconciler"
	"github.com/kkjorsvik/smith/internal/registry"
	"github.com/kkjorsvik/smith/internal/runtime"
	"github.com/kkjorsvik/smith/internal/scheduler"
	smithtls "github.com/kkjorsvik/smith/internal/tls"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "gencerts" {
		runGenCerts()
		return
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
	fs.Parse(os.Args[2:])

	if err := os.MkdirAll(*out, 0700); err != nil {
		log.Fatalf("gencerts: mkdir %s: %v", *out, err)
	}

	// Generate CA key and cert.
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

	writePEM(filepath.Join(*out, "ca.crt"), "CERTIFICATE", caDER)
	writeKeyPEM(filepath.Join(*out, "ca.key"), caKey)
	log.Printf("gencerts: wrote CA to %s", *out)

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		log.Fatalf("gencerts: parse CA cert: %v", err)
	}

	// Generate server cert.
	generateCert(*out, "server", caCert, caKey, []string{"smith-server-01.kkjorsvik.com"})

	// Generate agent certs.
	if *hosts != "" {
		for _, host := range splitHosts(*hosts) {
			name := sanitizeName(host)
			generateCert(*out, name, caCert, caKey, []string{host})
		}
	}

	log.Println("gencerts: done")
}

func generateCert(out, name string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("gencerts: generate key for %s: %v", name, err)
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
		log.Fatalf("gencerts: create cert for %s: %v", name, err)
	}

	writePEM(filepath.Join(out, name+".crt"), "CERTIFICATE", der)
	writeKeyPEM(filepath.Join(out, name+".key"), key)
	log.Printf("gencerts: wrote cert for %s", name)
}

func writePEM(path, pemType string, der []byte) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("gencerts: create %s: %v", path, err)
	}
	defer f.Close()
	pem.Encode(f, &pem.Block{Type: pemType, Bytes: der})
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Fatalf("gencerts: marshal key: %v", err)
	}
	writePEM(path, "EC PRIVATE KEY", der)
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
