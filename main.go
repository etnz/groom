package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/brutella/dnssd"
)

// Configuration Constants
const (
	ServiceName = "Groom-Agent"
)

var (
	// CurrentVersion const value is actually injected by the release process.
	CurrentVersion = "v0.0.0"
	// BinaryName contains the fully qualified binary exposed in Github. e.g. "groom-linux-arm64"
	// the download URL will be https://github.com/etnz/groom/releases/download/${Version}/${BinaryName}
	BinaryName = "groom-linux-amd64"
	// Hostname is set at init using the localhost name.
	Hostname string
)

func init() {
	var err error
	Hostname, err = os.Hostname()
	if err != nil {
		Hostname = "groom-unknown"
	}
}

// temporary as global, the current goal is to have something that works for self update
// THEN and only then we'll refactor this code.
var ctx context.Context
var cancel func()
var responder dnssd.Responder
var handle dnssd.ServiceHandle
var sig chan os.Signal

func main() {
	log.Printf("👲 Groom %s starting on %s...", CurrentVersion, Hostname)

	// Create a context to control the loop
	ctx, cancel = context.WithCancel(context.Background())

	// Start mDNS Responder (Advertising)
	startAdvertising(ctx)
	watchForConcierge(ctx)

	// Block until Shutdown Signal
	sig = make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("👋 Shutdown signal received. Goodbye.")
	shutdown()
}

func shutdown() {
	log.Println("📢 Sending mDNS Goodbye packet...")
	responder.Remove(handle) // Triggers the Goodbye packet
	cancel()                 // Context
	os.Exit(0)
}

// startAdvertising starts an mDNS responder server to advertize
// information about this groom's instance.
func startAdvertising(ctx context.Context) {
	cfg := dnssd.Config{
		Name:   Hostname,
		Type:   "_groom._tcp",
		Domain: "local",
		Port:   80,
		Text: map[string]string{
			"version": CurrentVersion,
			"status":  "idle",
		},
	}

	service, err := dnssd.NewService(cfg)
	if err != nil {
		log.Fatalf("Failed to create mDNS service: %v", err)
	}

	responder, err = dnssd.NewResponder()
	if err != nil {
		log.Fatalf("Failed to create mDNS responder: %v", err)
	}

	handle, err = responder.Add(service)
	if err != nil {
		log.Fatalf("Failed to add service to responder: %v", err)
	}

	go func() {
		log.Println("📢 Groom's mDNS Advertising active.")
		if err := responder.Respond(ctx); err != nil && err != context.Canceled {
			log.Println("mDNS Responder stopped:", err)
		}
	}()
}

// --- mDNS LISTENING (The Watchdog) ---

func watchForConcierge(ctx context.Context) {
	// Browse specifically for the Concierge service
	log.Println("👀 Watching for Concierge orders...")

	// Use a persistent lookup with the main context.
	// This will handle query retransmission and listen for unsolicited announcements (multicast).
	go func() {
		// LookupType blocks until ctx is canceled.
		if err := dnssd.LookupType(ctx, "_concierge._tcp.local.", addConcierge, func(dnssd.BrowseEntry) {}); err != nil {
			// Only log real errors, not context cancellation
			if ctx.Err() == nil {
				log.Printf("❌ mDNS lookup failed: %v", err)
			}
		}
	}()
}

func addConcierge(entry dnssd.BrowseEntry) {
	targetVer := entry.Text["target_version"]

	// If no version is advertised, we can't do anything
	if targetVer == "" {
		return
	}

	// Construct the download URL based on convention
	// Pattern: https://github.com/etnz/groom/releases/download/${Version}/${BinaryName}
	downloadUrl := fmt.Sprintf("https://github.com/etnz/groom/releases/download/%s/%s", targetVer, BinaryName)

	log.Printf("📢 Concierge advertised targetVersion=%q (Current: %q) => Download URL: %q", targetVer, CurrentVersion, downloadUrl)

	// Check if we need to update
	if targetVer != "" && targetVer != CurrentVersion {
		log.Printf("⚠️  ORDER RECEIVED: Update to %s required (Current: %s)", targetVer, CurrentVersion)
		performSelfUpdate(downloadUrl)
		return // Stop watching, we are restarting
	}
}

// performSelfUpdate performs self update by downloading a new binary file from a remote URL
// and using it in lieu of the current one, and exiting the main.
func performSelfUpdate(url string) {
	log.Printf("⬇️  Downloading new version from: %s", url)

	// Download.
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("❌ Update failed (Download): %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("❌ Update failed (HTTP %d)", resp.StatusCode)
		return
	}

	// Save to tmp.
	tmpPath := "/tmp/groom_new"
	out, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("❌ Update failed (File Create): %v", err)
		return
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		log.Printf("❌ Update failed (Write): %v", err)
		return
	}

	// Make executable.
	if err := os.Chmod(tmpPath, 0755); err != nil {
		log.Printf("❌ Update failed (Chmod): %v", err)
		return
	}

	// Atomic Swap.
	selfPath, err := os.Executable()
	if err != nil {
		// Fallback
		selfPath = "/usr/local/bin/groom"
	}

	log.Printf("🔄 Replacing binary at %s...", selfPath)
	// It's important to use os.Rename instead of attempting to
	// download directly on os.Executable, because linux allows
	// moving onto an open file, but not opening it for write.
	if err := os.Rename(tmpPath, selfPath); err != nil {
		log.Printf("❌ Update failed (Rename): %v", err)
		return
	}

	// Shutdown, we assume that someone (systemd?) is responsible for
	// restarting the (new) program.
	log.Println("✅ Binary replaced. Exiting to force restart...")
	shutdown() // turn down properly the groom
}
