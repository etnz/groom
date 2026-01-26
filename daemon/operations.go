package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brutella/dnssd"
)

var ErrForbidden = fmt.Errorf("forbidden")

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
