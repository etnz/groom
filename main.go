package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/brutella/dnssd"
)

// Configuration Constants
const (
    // CurrentVersion const value is actually injected by the release process.
	CurrentVersion = "v0.0.1"
	ServiceName    = "Groom-Agent"
)

var Hostname string

func init() {
	var err error
	Hostname, err = os.Hostname()
	if err != nil {
		Hostname = "groom-unknown"
	}
}

func main() {
	log.Printf("👲 Groom %s starting on %s...", CurrentVersion, Hostname)

	// Create a context to control the loop
	ctx, cancel := context.WithCancel(context.Background())

	// Start mDNS Responder (Advertising)
	stopAdvertising := startAdvertising(ctx)

	// Block until Shutdown Signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("👋 Shutdown signal received. Goodbye.")

    stopAdvertising()
	cancel()
}

// startAdvertising starts an mDNS responder server to advertize
// information about this groom's instance.
func startAdvertising(ctx context.Context) (stop func()){
	cfg := dnssd.Config{
		Name:   Hostname,
		Type:   "_groom._tcp",
		Domain: "local",
		Text: map[string]string{
			"version": CurrentVersion,
			"status":  "idle",
		},
	}

	service, err := dnssd.NewService(cfg)
	if err != nil {
		log.Fatalf("Failed to create mDNS service: %v", err)
	}

	responder, err := dnssd.NewResponder()
	if err != nil {
		log.Fatalf("Failed to create mDNS responder: %v", err)
	}

	handle, err := responder.Add(service)
	if err != nil {
		log.Fatalf("Failed to add service to responder: %v", err)
	}

	go func() {
		log.Println("📢 Groom's mDNS Advertising active.")
		if err := responder.Respond(ctx); err != nil && err != context.Canceled {
			log.Println("mDNS Responder stopped:", err)
		}
	}()

	return func() {
        log.Println("📢 Sending mDNS Goodbye packet...")
		responder.Remove(handle) // Triggers the Goodbye packet
    }
}
