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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brutella/dnssd"
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

// Template for the installer script executed via systemd-run
const installerScriptTemplate = `#!/bin/bash
set -u

POOL_FILE="%s"
TARGET_FILE="%s"
CURRENT_FILE="%s"
BACKUP_FILE="%s"

log() { echo "[Groom-Installer] $1"; }

log "Starting installation of $(basename "$POOL_FILE")"

# Backup existing installed file if it exists
if [ -n "$CURRENT_FILE" ] && [ -f "$CURRENT_FILE" ]; then
  log "Backing up existing version $(basename "$CURRENT_FILE") to $(basename "$BACKUP_FILE")"
  mv "$CURRENT_FILE" "$BACKUP_FILE"
fi

# Attempt installation
log "Running apt-get install..."
# We use apt-get install to handle dependencies resolution if needed
if apt-get install -y "$POOL_FILE"; then
  log "Installation successful."
  
  # Commit: Move pool file to installed location (Source of Truth)
  log "Committing: Moving pool file to installed cache"
  mv "$POOL_FILE" "$TARGET_FILE"
  
  # Cleanup backup
  if [ -n "$BACKUP_FILE" ] && [ -f "$BACKUP_FILE" ]; then
    log "Removing backup file"
    rm "$BACKUP_FILE"
  fi
  
  log "SUCCESS"
  exit 0
else
  log "Installation failed."
  
  # Rollback
  if [ -n "$BACKUP_FILE" ] && [ -f "$BACKUP_FILE" ]; then
    log "Rolling back: Re-installing previous version"
    if apt-get install -y "$BACKUP_FILE"; then
      log "Rollback installation successful."
      log "Restoring backup file to active position"
      mv "$BACKUP_FILE" "$CURRENT_FILE"
    else
      log "FATAL: Rollback failed."
      exit 1
    fi
  else
    log "No backup to rollback to (or first install). System might be in inconsistent state."
  fi
  
  exit 1
fi
`

func main() {
	if addr := os.Getenv("GROOM_ADDR"); addr != "" {
		listenAddr = addr
	}

	// Extract port for mDNS
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

	// Ensure directories exist
	os.MkdirAll(PoolDir, 0755)
	os.MkdirAll(InstalledDir, 0755)

	// Handlers
	http.HandleFunc("/pool/", handlePool)
	http.HandleFunc("/installed/", handleInstalled)
	http.HandleFunc("/health", handleHealth)

	server := &http.Server{Addr: listenAddr}

	// mDNS
	stopAdvertising := startAdvertising(port)

	// HTTP Server
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Signal Handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("👋 Shutdown signal received.")
	stopAdvertising()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	log.Println("🛑 Groom stopped.")
}

// --- INSTALLED HANDLERS ---

func handleInstalled(w http.ResponseWriter, r *http.Request) {
	// Path: /installed/{filename}
	// If path is just /installed/, filename is empty
	arg := strings.TrimPrefix(r.URL.Path, "/installed/")

	switch r.Method {
	case http.MethodGet:
		if arg == "" {
			listInstalled(w, r)
		} else {
			http.Error(w, "Not implemented", http.StatusNotImplemented)
		}
	case http.MethodPost:
		// POST /installed/filename.deb -> Install from pool
		if arg == "" {
			http.Error(w, "Filename required", http.StatusBadRequest)
			return
		}
		installPackage(w, r, arg)
	case http.MethodDelete:
		if arg == "" {
			purgeInstalled(w, r)
		} else {
			removePackage(w, r, arg)
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func listInstalled(w http.ResponseWriter, r *http.Request) {
	files, err := os.ReadDir(InstalledDir)
	if err != nil {
		fail(w, "Failed to read installed dir", err)
		return
	}
	var list []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".deb") {
			list = append(list, f.Name())
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// installPackage handles the transaction of installing a deb from the pool via systemd-run
func installPackage(w http.ResponseWriter, r *http.Request, poolFilename string) {
	// Basic security check: ensure filename doesn't contain path traversal
	if filepath.Base(poolFilename) != poolFilename {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	sourcePath := filepath.Join(PoolDir, poolFilename)
	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		http.Error(w, "File not found in pool", http.StatusNotFound)
		return
	}

	// Identify Package Name to find potential conflicts/upgrades
	pkgName, err := getPackageName(sourcePath)
	if err != nil {
		fail(w, "Invalid deb file", err)
		return
	}

	// Paths configuration
	// 1. Target: The pool filename (preserving version) inside installed dir
	targetDeb := filepath.Join(InstalledDir, poolFilename)

	// 2. Current: Use helper to find the existing file for this package (e.g. version 1.0)
	currentDeb := findInstalledPackage(pkgName)

	// 3. Backup: The location to store the current file during update
	backupDeb := ""
	if currentDeb != "" {
		backupDeb = currentDeb + ".previous"
	}

	// Generate the ephemeral installer script
	scriptContent := fmt.Sprintf(installerScriptTemplate, sourcePath, targetDeb, currentDeb, backupDeb)
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("groom_install_%s.sh", pkgName))

	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
		fail(w, "Failed to create installer script", err)
		return
	}

	// Construct a unique unit name for systemd-run
	unitName := fmt.Sprintf("groom-install-%s", pkgName)

	log.Printf("🚀 Launching detached installation for %s (unit: %s)...", pkgName, unitName)

	// Launch via systemd-run
	cmd := exec.Command("systemd-run",
		"--unit="+unitName,
		"--description=Groom Service Installer Worker for "+pkgName,
		"--service-type=oneshot",
		// Allow the script to live even if groom dies (which happens during self-update)
		"--collect",
		scriptPath,
	)

	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("❌ Failed to launch installer: %s", string(output))
		http.Error(w, fmt.Sprintf("Failed to schedule installation: %s", string(output)), http.StatusInternalServerError)
		return
	}

	// Return 202 Accepted because the operation continues in background
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "Installation scheduled. Monitor journalctl -u %s", unitName)
}

// findInstalledPackage scans the installed directory to find a deb file matching the package name
func findInstalledPackage(pkgName string) string {
	files, err := os.ReadDir(InstalledDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		log.Printf("Failed to read installed directory: %v", err)
		return ""
	}

	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".deb") {
			path := filepath.Join(InstalledDir, f.Name())
			name, err := getPackageName(path)
			if err == nil && name == pkgName {
				return path
			}
		}
	}
	return ""
}

// removePackage uninstalls a package
func removePackage(w http.ResponseWriter, r *http.Request, filename string) {
	// filename is expected to be the .deb filename in installed folder
	// We need to resolve the package name from it first

	installedPath := filepath.Join(InstalledDir, filename)
	if _, err := os.Stat(installedPath); os.IsNotExist(err) {
		http.Error(w, "File not found in installed", http.StatusNotFound)
		return
	}

	pkgName, err := getPackageName(installedPath)
	if err != nil {
		fail(w, "Failed to read package info", err)
		return
	}

	// Prevent suicide: do not allow removing the agent itself
	if pkgName == SelfPackageName {
		http.Error(w, "Cannot remove groom agent itself via API", http.StatusForbidden)
		return
	}

	log.Printf("🗑️ Removing %s...", pkgName)
	cmd := exec.Command("apt-get", "remove", "-y", pkgName)
	if out, err := cmd.CombinedOutput(); err != nil {
		fail(w, fmt.Sprintf("Remove failed: %s", string(out)), err)
		return
	}

	// Remove record from installed
	os.Remove(installedPath)

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Removed %s", pkgName)
}

// purgeInstalled removes all packages except Groom
func purgeInstalled(w http.ResponseWriter, r *http.Request) {
	files, err := os.ReadDir(InstalledDir)
	if err != nil {
		if os.IsNotExist(err) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "Purged 0 packages")
			return
		}
		fail(w, "Read dir failed", err)
		return
	}

	count := 0
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".deb") {
			fullPath := filepath.Join(InstalledDir, f.Name())
			pkgName, err := getPackageName(fullPath)
			if err != nil {
				log.Printf("Skipping unreadable file %s", f.Name())
				continue
			}

			// Protect Groom
			if pkgName == SelfPackageName {
				continue
			}

			log.Printf("🔥 Purging %s...", pkgName)
			// Purge to remove config files too
			exec.Command("apt-get", "purge", "-y", pkgName).Run()
			os.Remove(fullPath)
			count++
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Purged %d packages", count)
}

// --- POOL HANDLERS (Simplified) ---

func handlePool(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/pool/")
	switch r.Method {
	case http.MethodPost:
		if filename == "" {
			http.Error(w, "Filename required", http.StatusBadRequest)
			return
		}
		uploadPool(w, r, filename)
	case http.MethodGet:
		listPool(w)
	case http.MethodDelete:
		if filename == "" {
			os.RemoveAll(PoolDir)
			os.MkdirAll(PoolDir, 0755)
			w.WriteHeader(200)
		} else {
			os.Remove(filepath.Join(PoolDir, filename))
			w.WriteHeader(200)
		}
	}
}

func listPool(w http.ResponseWriter) {
	files, _ := os.ReadDir(PoolDir)
	var list []string
	for _, f := range files {
		if !f.IsDir() {
			list = append(list, f.Name())
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func uploadPool(w http.ResponseWriter, r *http.Request, filename string) {
	path := filepath.Join(PoolDir, filename)
	// Basic security check
	if filepath.Base(filename) != filename {
		http.Error(w, "Invalid filename", 400)
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fail(w, "Create failed", err)
		return
	}
	defer f.Close()
	io.Copy(f, r.Body)
	w.WriteHeader(201)
}

// --- HELPERS ---

func getPackageName(debPath string) (string, error) {
	// dpkg-deb -f file Package
	out, err := exec.Command("dpkg-deb", "-f", debPath, "Package").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func startAdvertising(port int) func() {
	hostname, _ := os.Hostname()
	cfg := dnssd.Config{
		Name:   hostname,
		Type:   "_groom._tcp",
		Domain: "local",
		Port:   port,
		Text:   map[string]string{"version": CurrentVersion},
	}
	service, _ := dnssd.NewService(cfg)
	responder, _ := dnssd.NewResponder()
	handle, _ := responder.Add(service)
	go responder.Respond(context.Background())
	return func() { responder.Remove(handle) }
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"healthy"}`))
}

func fail(w http.ResponseWriter, msg string, err error) {
	log.Printf("❌ %s: %v", msg, err)
	http.Error(w, msg, http.StatusInternalServerError)
}
