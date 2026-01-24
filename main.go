package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brutella/dnssd"
)

// listenAddr is the TCP address to bind to (e.g. ":8080" or "192.168.1.50:80").
// It can be overridden by the GROOM_ADDR environment variable.
var listenAddr = ":8080"

// CurrentVersion is injected at build time
var CurrentVersion = "v0.0.1"

const (
	// Directory where .deb files are stored
	PoolDir = "/var/lib/groom/pool"
)

func main() {
	// Support GROOM_ADDR to bind to a specific VIP or interface
	if addr := os.Getenv("GROOM_ADDR"); addr != "" {
		listenAddr = addr
	}

	// Extract port for mDNS advertising
	_, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		if strings.HasPrefix(listenAddr, ":") {
			portStr = listenAddr[1:]
		} else {
			portStr = "8080"
		}
	}
	port, _ := strconv.Atoi(portStr)

	log.Printf("🎩 Groom Agent started on %s", listenAddr)

	// Ensure pool directory exists
	if err := os.MkdirAll(PoolDir, 0755); err != nil {
		log.Fatalf("Failed to create pool directory: %v", err)
	}

	// Setup Handlers
	http.HandleFunc("/pool/", handlePool) // Handles /pool/{filename} and /pool/
	http.HandleFunc("/health", handleHealth)

	server := &http.Server{Addr: listenAddr}

	// 1. Start mDNS Advertising
	stopAdvertising := startAdvertising(port)

	// 2. Start HTTP Server
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// 3. Wait for Shutdown Signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("👋 Shutdown signal received. Stopping services...")

	// 4. Graceful Shutdown
	stopAdvertising()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("HTTP Shutdown error: %v", err)
	}

	log.Println("🛑 Groom stopped gracefully.")
}

func handlePool(w http.ResponseWriter, r *http.Request) {
	// Extract filename from path: /pool/filename.deb -> filename.deb
	// If path is just /pool/, filename will be empty
	filename := strings.TrimPrefix(r.URL.Path, "/pool/")

	switch r.Method {
	case http.MethodPost:
		if filename == "" {
			http.Error(w, "Filename required in path", http.StatusBadRequest)
			return
		}
		handlePoolUpload(w, r, filename)

	case http.MethodDelete:
		if filename == "" {
			handlePoolPurge(w, r)
		} else {
			handlePoolDelete(w, r, filename)
		}

	case http.MethodGet:
		if filename == "" {
			handlePoolList(w, r)
		} else {
			http.Error(w, "Single file download not implemented yet", http.StatusNotImplemented)
		}

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// GET /pool/
func handlePoolList(w http.ResponseWriter, r *http.Request) {
	files, err := os.ReadDir(PoolDir)
	if err != nil {
		fail(w, "Failed to read pool directory", err)
		return
	}

	var filenames []string
	for _, f := range files {
		if !f.IsDir() {
			filenames = append(filenames, f.Name())
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(filenames); err != nil {
		fail(w, "Failed to encode response", err)
	}
}

// POST /pool/{filename}
func handlePoolUpload(w http.ResponseWriter, r *http.Request, filename string) {
	// Basic security check: ensure filename doesn't contain path traversal
	if filepath.Base(filename) != filename {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	// Ensure it looks like a debian package (basic check)
	if !strings.HasSuffix(filename, ".deb") {
		http.Error(w, "Only .deb files allowed", http.StatusBadRequest)
		return
	}

	targetPath := filepath.Join(PoolDir, filename)
	log.Printf("📥 Receiving %s...", filename)

	// Create directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		fail(w, "Failed to create directory", err)
		return
	}

	// Create/Overwrite file
	file, err := os.Create(targetPath)
	if err != nil {
		fail(w, "Failed to create file", err)
		return
	}
	defer file.Close()

	size, err := io.Copy(file, r.Body)
	if err != nil {
		fail(w, "Failed to write file", err)
		return
	}

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "Uploaded %s (%d bytes)", filename, size)
}

// DELETE /pool/{filename}
func handlePoolDelete(w http.ResponseWriter, r *http.Request, filename string) {
	// Basic security check
	if filepath.Base(filename) != filename {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	targetPath := filepath.Join(PoolDir, filename)
	log.Printf("🗑️ Deleting %s...", filename)

	err := os.Remove(targetPath)
	if err != nil && !os.IsNotExist(err) {
		fail(w, "Failed to delete file", err)
		return
	}

	w.WriteHeader(http.StatusOK)
	if os.IsNotExist(err) {
		fmt.Fprintf(w, "File %s was already deleted", filename)
	} else {
		fmt.Fprintf(w, "Deleted %s", filename)
	}
}

// DELETE /pool/
func handlePoolPurge(w http.ResponseWriter, r *http.Request) {
	log.Println("🔥 Purging entire pool...")

	// Re-create the directory to empty it safely
	if err := os.RemoveAll(PoolDir); err != nil {
		fail(w, "Failed to purge pool", err)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Pool purged"))
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
		log.Printf("mDNS Service init failed: %v", err)
		return func() {}
	}

	responder, err := dnssd.NewResponder()
	if err != nil {
		log.Printf("mDNS Responder init failed: %v", err)
		return func() {}
	}

	handle, err := responder.Add(service)
	if err != nil {
		log.Printf("mDNS Registration failed: %v", err)
		return func() {}
	}

	go func() {
		if err := responder.Respond(context.Background()); err != nil {
			log.Println("mDNS Responder stopped:", err)
		}
	}()

	log.Printf("📢 mDNS Advertising active for %s on port %d", hostname, port)

	return func() {
		log.Println("📢 Sending mDNS Goodbye packet...")
		responder.Remove(handle)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"healthy"}`))
}

func fail(w http.ResponseWriter, msg string, err error) {
	log.Printf("❌ %s: %v", msg, err)
	http.Error(w, msg, http.StatusInternalServerError)
}
