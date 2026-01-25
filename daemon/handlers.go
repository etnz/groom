package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// registerHandlers sets up the HTTP routes.
func (s *Server) registerHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/pool/", s.handlePool)
	mux.HandleFunc("/installed/", s.handleInstalled)
	mux.HandleFunc("/health", s.handleHealth)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"healthy"}`))
}

func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/pool/")
	switch r.Method {
	case http.MethodPost:
		if filename == "" {
			http.Error(w, "Filename required", http.StatusBadRequest)
			return
		}
		// Basic security check
		if filepath.Base(filename) != filename {
			http.Error(w, "Invalid filename", http.StatusBadRequest)
			return
		}
		if err := s.uploadPoolOp(filename, r.Body); err != nil {
			s.fail(w, "Create failed", err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		list, err := s.listPoolOp()
		if err != nil {
			s.fail(w, "List pool failed", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	case http.MethodDelete:
		if filename == "" {
			if err := s.clearPoolOp(); err != nil {
				s.fail(w, "Clear pool failed", err)
				return
			}
		} else {
			if err := s.deletePoolFileOp(filename); err != nil {
				s.fail(w, "Delete failed", err)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleInstalled(w http.ResponseWriter, r *http.Request) {
	arg := strings.TrimPrefix(r.URL.Path, "/installed/")

	switch r.Method {
	case http.MethodGet:
		if arg == "" {
			list, err := s.listInstalledOp()
			if err != nil {
				s.fail(w, "Failed to read installed dir", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(list)
		} else {
			http.Error(w, "Not implemented", http.StatusNotImplemented)
		}
	case http.MethodPost:
		// POST /installed/filename.deb -> Install from pool
		if arg == "" {
			http.Error(w, "Filename required", http.StatusBadRequest)
			return
		}
		// Basic security check
		if filepath.Base(arg) != arg {
			http.Error(w, "Invalid filename", http.StatusBadRequest)
			return
		}

		unitName, err := s.scheduleInstallOp(arg)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "File not found in pool", http.StatusNotFound)
			} else {
				log.Printf("❌ Failed to launch installer: %v", err)
				http.Error(w, fmt.Sprintf("Failed to schedule installation: %v", err), http.StatusInternalServerError)
			}
			return
		}

		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "Installation scheduled. Monitor journalctl -u %s", unitName)

	case http.MethodDelete:
		if arg == "" {
			count, err := s.purgeInstalledOp()
			if err != nil {
				s.fail(w, "Purge failed", err)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "Purged %d packages", count)
		} else {
			pkgName, err := s.removePackageOp(arg)
			if err != nil {
				if os.IsNotExist(err) {
					http.Error(w, "File not found in installed", http.StatusNotFound)
				} else if errors.Is(err, ErrForbidden) {
					http.Error(w, "Cannot remove groom agent itself via API", http.StatusForbidden)
				} else {
					s.fail(w, fmt.Sprintf("Remove failed: %v", err), err)
				}
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "Removed %s", pkgName)
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) fail(w http.ResponseWriter, msg string, err error) {
	log.Printf("❌ %s: %v", msg, err)
	http.Error(w, msg, http.StatusInternalServerError)
}
