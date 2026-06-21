package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kkjorsvik/smith/internal/agent"
	"github.com/kkjorsvik/smith/internal/runtime"
)

func main() {
	var (
		id         = flag.String("id", "", "Unique node ID (e.g. smith-agent-01)")
		addr       = flag.String("addr", "", "This agent's HTTP address (e.g. 192.168.1.55:9000)")
		serverAddr = flag.String("server", "", "Control plane HTTP address (e.g. 192.168.1.54:8080)")
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

	client, err := runtime.NewClient()
	if err != nil {
		log.Fatalf("agent: connect to containerd: %v", err)
	}
	defer client.Close()

	if err := client.CleanupAll(); err != nil {
		log.Fatalf("agent: cleanup: %v", err)
	}

	a := agent.New(*id, *addr, *serverAddr, client)
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
