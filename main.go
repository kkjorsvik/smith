package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kkjorsvik/smith/internal/api"
	"github.com/kkjorsvik/smith/internal/health"
	"github.com/kkjorsvik/smith/internal/reconciler"
	"github.com/kkjorsvik/smith/internal/runtime"
)

func main() {
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

	if err := client.CleanupAll(); err != nil {
		log.Fatalf("cleanup failed: %v", err)
	}

	monitor := health.NewMonitor(client)

	r := reconciler.New(client, store, monitor, 5*time.Second)
	r.Start()

	server := api.New(store, client, ":8080")
	server.Start()

	log.Println("smith running — press ctrl+c to stop")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig

	log.Println("shutting down...")
	r.Stop()
}
