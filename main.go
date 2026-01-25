package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/etnz/groom/daemon"
)

// Configuration variables
var (
	listenAddr     = ":8080"
	CurrentVersion = "v0.0.1"
	// SelfPackageName defines the name of the package that contains Groom itself
	// to prevent accidental self-deletion during purge.
	SelfPackageName = "groom-agent"
)

const (
	PoolDir      = "/var/lib/groom/pool"
	InstalledDir = "/var/lib/groom/installed"
)

func main() {
	if addr := os.Getenv("GROOM_ADDR"); addr != "" {
		listenAddr = addr
	}

	cfg := daemon.Config{
		ListenAddr:      listenAddr,
		Version:         CurrentVersion,
		SelfPackageName: SelfPackageName,
		PoolDir:         PoolDir,
		InstalledDir:    InstalledDir,
	}

	server := daemon.New(cfg)
	server.Start()

	// Signal Handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Stop(ctx)
}
