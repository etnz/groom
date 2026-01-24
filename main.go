package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/brutella/dnssd"
)

// CurrentVersion is injected by GoReleaser via ldflags
var CurrentVersion = "dev"

func main() {
	log.Printf("🎩 Groom Service %s starting...", CurrentVersion)

	// Determine address to listen on
	addr := ":8080"
	if envAddr := os.Getenv("GROOM_ADDR"); envAddr != "" {
		addr = envAddr
	}

	// Extract port for mDNS
	port := 8080
	_, portStr, err := net.SplitHostPort(addr)
	if err == nil {
		if p, err := strconv.Atoi(portStr); err == nil {
			port = p
		}
	} else if strings.HasPrefix(addr, ":") {
		if p, err := strconv.Atoi(addr[1:]); err == nil {
			port = p
		}
	}

	// Setup HTTP Server
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "healthy\n")
	})

	server := &http.Server{Addr: addr, Handler: mux}

	// Start mDNS
	stopBonjour := startAdvertising(port)

	// Start HTTP Server
	go func() {
		log.Printf("🌍 HTTP server listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("❌ Server failed to start: %v", err)
		}
	}()

	// Wait for Shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("👋 Shutting down...")
	stopBonjour()
}

func startAdvertising(port int) func() {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "groom-unknown"
	}

	cfg := dnssd.Config{
		Name:   hostname,
		Type:   "_groom._tcp",
		Domain: "local",
		Port:   port,
		Text: map[string]string{
			"version": CurrentVersion,
			"status":  "healthy",
		},
	}

	service, err := dnssd.NewService(cfg)
	if err != nil {
		log.Printf("❌ mDNS Error: %v", err)
		return func() {}
	}

	responder, err := dnssd.NewResponder()
	if err != nil {
		log.Printf("❌ mDNS Error: %v", err)
		return func() {}
	}

	handle, err := responder.Add(service)
	if err != nil {
		log.Printf("❌ mDNS Error: %v", err)
		return func() {}
	}

	go func() {
		if err := responder.Respond(context.Background()); err != nil {
			log.Printf("⚠️ mDNS Responder stopped: %v", err)
		}
	}()

	log.Printf("📢 mDNS advertising enabled for %s on port %d", hostname, port)

	return func() {
		responder.Remove(handle)
	}
}
