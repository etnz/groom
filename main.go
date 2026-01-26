package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/grandcat/zeroconf"
)

// Defined by the build system.
var CurrentVersion = "v0.0.1"

func main() {
	server, err := zeroconf.Register("groom-service", "_groom._tcp", "local.", 8080, nil, nil)
	if err != nil {
		log.Fatalf("Failed to register mDNS service: %v", err)
	}
	defer server.Shutdown()

	log.Println("mDNS responder started. Press Ctrl+C to exit.")
	// Signal Handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down.")
}
