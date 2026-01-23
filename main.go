package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
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
	CurrentVersion = "v0.0.1"
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

func main() {
	log.Printf("👲 Groom %s starting on %s...", CurrentVersion, Hostname)

	// Create a context to control the loop
	ctx, cancel := context.WithCancel(context.Background())

	// Start mDNS Responder (Advertising)
	stopAdvertising := startAdvertising(ctx)
	watchForConcierge(ctx)

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
func startAdvertising(ctx context.Context) (stop func()) {
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

// --- mDNS LISTENING (The Watchdog) ---

func watchForConcierge(ctx context.Context) {
	// Browse specifically for the Concierge service
	log.Println("👀 Watching for Concierge orders...")

	// Use a persistent lookup with the main context.
	// This will handle query retransmission and listen for unsolicited announcements (multicast).
	go func() {
		// LookupType blocks until ctx is canceled.
		if err := dnssd.LookupType(ctx, "_concierge._tcp", addConcierge, func(dnssd.BrowseEntry) {}); err != nil {
			// Only log real errors, not context cancellation
			if ctx.Err() == nil {
				log.Printf("❌ mDNS lookup failed: %v", err)
			}
		}
	}()
}

func addConcierge(entry dnssd.BrowseEntry) {
	targetVer := entry.Text["target_version"]
	downloadUrl := entry.Text["url"]

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

	// Restart Systemd
	log.Println("✅ Binary replaced. Triggering Systemd restart...")
	cmd := exec.Command("systemctl", "restart", "groom")
	if err := cmd.Run(); err != nil {
		log.Printf("❌ Systemd restart failed: %v", err)
		// Usually if this fails, we exit anyway and let systemd restart us as crashed
		os.Exit(0)
	}

	os.Exit(0)
}
