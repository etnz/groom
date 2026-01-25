package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brutella/dnssd"
)

var ErrForbidden = fmt.Errorf("forbidden")

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

func (s *Server) startAdvertisingOp(port int) (func(), error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname: %w", err)
	}
	cfg := dnssd.Config{
		Name:   hostname,
		Type:   "_groom._tcp",
		Domain: "local",
		Port:   port,
		Text:   map[string]string{"version": s.cfg.Version},
	}
	service, err := dnssd.NewService(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create service: %w", err)
	}
	responder, err := dnssd.NewResponder()
	if err != nil {
		return nil, fmt.Errorf("failed to create responder: %w", err)
	}
	handle, err := responder.Add(service)
	if err != nil {
		return nil, fmt.Errorf("failed to add service to responder: %w", err)
	}
	go responder.Respond(context.Background())
	return func() { responder.Remove(handle) }, nil
}

func (s *Server) listPoolOp() ([]string, error) {
	files, err := os.ReadDir(s.cfg.PoolDir)
	if err != nil {
		return nil, err
	}
	var list []string
	for _, f := range files {
		if !f.IsDir() {
			list = append(list, f.Name())
		}
	}
	return list, nil
}

func (s *Server) uploadPoolOp(filename string, content io.Reader) error {
	path := filepath.Join(s.cfg.PoolDir, filename)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, content)
	return err
}

func (s *Server) clearPoolOp() error {
	if err := os.RemoveAll(s.cfg.PoolDir); err != nil {
		return err
	}
	return os.MkdirAll(s.cfg.PoolDir, 0755)
}

func (s *Server) deletePoolFileOp(filename string) error {
	return os.Remove(filepath.Join(s.cfg.PoolDir, filename))
}

func (s *Server) listInstalledOp() ([]string, error) {
	files, err := os.ReadDir(s.cfg.InstalledDir)
	if err != nil {
		return nil, err
	}
	var list []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".deb") {
			list = append(list, f.Name())
		}
	}
	return list, nil
}

func (s *Server) scheduleInstallOp(poolFilename string) (string, error) {
	sourcePath := filepath.Join(s.cfg.PoolDir, poolFilename)
	if _, err := os.Stat(sourcePath); err != nil {
		return "", err
	}

	// Identify Package Name to find potential conflicts/upgrades
	pkgName, err := s.getPackageName(sourcePath)
	if err != nil {
		return "", fmt.Errorf("invalid deb file: %w", err)
	}

	// Paths configuration
	targetDeb := filepath.Join(s.cfg.InstalledDir, poolFilename)
	currentDeb := s.findInstalledPackage(pkgName)
	backupDeb := ""
	if currentDeb != "" {
		backupDeb = currentDeb + ".previous"
	}

	// Generate the ephemeral installer script
	scriptContent := fmt.Sprintf(installerScriptTemplate, sourcePath, targetDeb, currentDeb, backupDeb)
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("groom_install_%s.sh", pkgName))

	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
		return "", fmt.Errorf("failed to create installer script: %w", err)
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
		return "", fmt.Errorf("%s", string(output))
	}

	return unitName, nil
}

func (s *Server) removePackageOp(filename string) (string, error) {
	installedPath := filepath.Join(s.cfg.InstalledDir, filename)
	if _, err := os.Stat(installedPath); err != nil {
		return "", err
	}

	pkgName, err := s.getPackageName(installedPath)
	if err != nil {
		return "", fmt.Errorf("failed to read package info: %w", err)
	}

	// Prevent suicide: do not allow removing the agent itself
	if pkgName == s.cfg.SelfPackageName {
		return "", ErrForbidden
	}

	log.Printf("🗑️ Removing %s...", pkgName)
	cmd := exec.Command("apt-get", "remove", "-y", pkgName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("remove failed: %s: %w", string(out), err)
	}

	// Remove record from installed
	os.Remove(installedPath)
	return pkgName, nil
}

func (s *Server) purgeInstalledOp() (int, error) {
	files, err := os.ReadDir(s.cfg.InstalledDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	count := 0
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".deb") {
			fullPath := filepath.Join(s.cfg.InstalledDir, f.Name())
			pkgName, err := s.getPackageName(fullPath)
			if err != nil {
				log.Printf("Skipping unreadable file %s", f.Name())
				continue
			}

			// Protect Groom
			if pkgName == s.cfg.SelfPackageName {
				continue
			}

			log.Printf("🔥 Purging %s...", pkgName)
			// Purge to remove config files too
			cmd := exec.Command("apt-get", "purge", "-y", pkgName)
			if out, err := cmd.CombinedOutput(); err != nil {
				log.Printf("Failed to purge package %s: %s", pkgName, string(out))
				continue
			}
			os.Remove(fullPath)
			count++
		}
	}
	return count, nil
}

func (s *Server) getPackageName(debPath string) (string, error) {
	// dpkg-deb -f file Package
	out, err := exec.Command("dpkg-deb", "-f", debPath, "Package").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *Server) findInstalledPackage(pkgName string) string {
	files, err := os.ReadDir(s.cfg.InstalledDir)
	if err != nil {
		return ""
	}

	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".deb") {
			path := filepath.Join(s.cfg.InstalledDir, f.Name())
			name, err := s.getPackageName(path)
			if err == nil && name == pkgName {
				return path
			}
		}
	}
	return ""
}
