package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/etnz/groom/daemon"
	"github.com/etnz/groom/executor"
)

// Configuration variables
var (
	listenAddr     = ":8080"
	CurrentVersion = "v0.0.1"
	// SelfPackageName defines the name of the package that contains Groom itself
	// to prevent accidental self-deletion during purge.
	SelfPackageName = "groom"
)
var execute = flag.Bool("execute", false, "Run the executor logic instead of the daemon")

const (
	PoolDir      = "/var/lib/groom/pool"
	InstalledDir = "/var/lib/groom/installed"
	StateDir     = "/var/lib/groom"
)

func main() {
	flag.Parse()

	if *execute {
		if err := executor.Run(StateDir); err != nil {
			log.Fatalf("Executor failed: %v", err)
		}
		return // Exit after executor runs
	}

	if addr := os.Getenv("GROOM_ADDR"); addr != "" {
		listenAddr = addr
	}

	cfg := daemon.Config{
		ListenAddr:      listenAddr,
		Version:         CurrentVersion,
		SelfPackageName: SelfPackageName,
		PoolDir:         PoolDir,
		InstalledDir:    InstalledDir,
		StateDir:        StateDir,
	}

	server, err := daemon.New(cfg)
	if err != nil {
		log.Fatalf("failed to create daemon: %v", err)
	}
	server.Start()

	// Signal Handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Stop(ctx)
}
