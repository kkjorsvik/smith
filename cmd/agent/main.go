package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/kkjorsvik/smith/internal/agent"
	"github.com/kkjorsvik/smith/internal/runtime"
	smithtls "github.com/kkjorsvik/smith/internal/tls"
)

func main() {
	var (
		id         = flag.String("id", "", "Unique node ID (e.g. smith-agent-01)")
		addr       = flag.String("addr", "", "This agent's HTTP address (e.g. 192.168.1.55:9000)")
		hostIP     = flag.String("hostip", "", "Underlay IP other nodes route container traffic through (default: host part of -addr)")
		serverAddr = flag.String("server", "", "Control plane internal mTLS address (e.g. smith-server-01.kkjorsvik.com:9443)")
		caCert     = flag.String("ca", "/etc/smith/certs/ca.crt", "Path to Smith CA cert")
		cert       = flag.String("cert", "", "Path to this agent's cert")
		key        = flag.String("key", "", "Path to this agent's key")
	)
	flag.Parse()

	if *id == "" {
		log.Fatal("agent: -id is required")
	}
	if *addr == "" {
		log.Fatal("agent: -addr is required")
	}
	if *serverAddr == "" {
		log.Fatal("agent: -server is required")
	}
	if *cert == "" {
		log.Fatal("agent: -cert is required")
	}
	if *key == "" {
		log.Fatal("agent: -key is required")
	}

	// Resolve the underlay host IP used as the route target by peer nodes.
	// Defaults to the host portion of -addr. Peers install routes via this
	// value with netlink, so it must be an IP — resolve a hostname if needed.
	resolvedHostIP := *hostIP
	if resolvedHostIP == "" {
		host, _, err := net.SplitHostPort(*addr)
		if err != nil {
			log.Fatalf("agent: cannot derive -hostip from -addr %q: %v (pass -hostip)", *addr, err)
		}
		resolvedHostIP = host
	}
	resolvedHostIP, err := resolveHostIP(resolvedHostIP)
	if err != nil {
		log.Fatalf("agent: %v (pass -hostip with an explicit IP)", err)
	}

	// For outbound calls to the control plane.
	clientTLS, err := smithtls.ClientConfig(*caCert, *cert, *key)
	if err != nil {
		log.Fatalf("agent: load client TLS: %v", err)
	}

	// For inbound connections from the control plane.
	serverTLS, err := smithtls.ServerConfig(*caCert, *cert, *key)
	if err != nil {
		log.Fatalf("agent: load server TLS: %v", err)
	}

	client, err := runtime.NewClient()
	if err != nil {
		log.Fatalf("agent: connect to containerd: %v", err)
	}
	defer client.Close()

	// agent.New initializes CNI; ghost-container cleanup (which needs CNI
	// to release stale IP allocations) runs inside a.Start() before the
	// agent registers with the control plane.
	a := agent.New(*id, *addr, resolvedHostIP, *serverAddr, client, clientTLS, serverTLS)
	if err := a.Start(); err != nil {
		log.Fatalf("agent: start: %v", err)
	}

	log.Printf("agent: %s running — press ctrl+c to stop", *id)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig

	log.Println("agent: shutting down...")
	a.Stop()
}

// resolveHostIP returns s unchanged if it is already an IP, otherwise it
// resolves the hostname to its first IPv4 address. Peers route container
// traffic via this value, so it must be an IP.
func resolveHostIP(s string) (string, error) {
	if net.ParseIP(s) != nil {
		return s, nil
	}
	addrs, err := net.LookupHost(s)
	if err != nil {
		return "", fmt.Errorf("resolve host IP %q: %w", s, err)
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return a, nil
		}
	}
	return "", fmt.Errorf("no IPv4 address found for %q", s)
}
