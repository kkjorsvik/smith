package main

import (
	"flag"
	"log"
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

	if err := client.CleanupAll(); err != nil {
		log.Fatalf("agent: cleanup: %v", err)
	}

	a := agent.New(*id, *addr, *serverAddr, client, clientTLS, serverTLS)
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
