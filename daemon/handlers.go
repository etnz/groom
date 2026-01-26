package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/etnz/groom/executor"
)

// transactionStatus is the serializable representation of the executor's operations.
type transactionStatus struct {
	State             executor.State `json:"state"`
	PackagesToInstall []string       `json:"packages_to_install"`
	PackagesToRemove  []string       `json:"packages_to_remove"`
	Error             string         `json:"error,omitempty"`
}

// registerHandlers sets up the HTTP routes.
func (s *Server) registerHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/pool/", s.handlePool)
	mux.HandleFunc("/installed/", s.handleInstalled)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/transaction", s.handleTransaction)
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

func (s *Server) handleTransaction(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetTransaction(w, r)
	case http.MethodPost:
		s.handleCommitTransaction(w, r)
	case http.MethodDelete:
		s.handleClearTransaction(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGetTransaction(w http.ResponseWriter, r *http.Request) {
	ops, err := s.executorStore.Operations()
	if err != nil && !os.IsNotExist(err) {
		s.fail(w, "failed to get transaction state", err)
		return
	}

	var status transactionStatus
	if os.IsNotExist(err) {
		status = transactionStatus{
			State:             executor.StatePrepare,
			PackagesToInstall: []string{},
			PackagesToRemove:  []string{},
		}
	} else {
		status = transactionStatus{
			State:             ops.State(),
			PackagesToInstall: ops.PackagesToInstall(),
			PackagesToRemove:  ops.PackagesToRemove(),
		}
		if ops.Err() != nil {
			status.Error = ops.Err().Error()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleCommitTransaction(w http.ResponseWriter, r *http.Request) {
	ops, err := s.executorStore.Operations()
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Cannot commit an empty transaction plan", http.StatusBadRequest)
		} else {
			s.fail(w, "failed to read transaction state", err)
		}
		return
	}

	if ops.State() != executor.StatePrepare {
		http.Error(w, fmt.Sprintf("Transaction not in prepare state (current state: %s)", ops.State()), http.StatusConflict)
		return
	}

	if len(ops.PackagesToInstall()) == 0 && len(ops.PackagesToRemove()) == 0 {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Transaction plan is empty, nothing to commit."))
		return
	}

	log.Println("🚀 Committing transaction, launching executor...")
	cmd := exec.Command("systemd-run",
		"--unit=groom-executor",
		"--description=Groom Executor",
		"--service-type=oneshot",
		"--collect",
		"/usr/local/bin/groom", "--execute",
	)

	if output, err := cmd.CombinedOutput(); err != nil {
		s.fail(w, fmt.Sprintf("failed to start executor: %s", string(output)), err)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("Executor triggered to apply changes."))
}

func (s *Server) handleClearTransaction(w http.ResponseWriter, r *http.Request) {
	err := s.executorStore.Update(func(ops *executor.Operations) error {
		ops.Clear()
		return nil
	})

	if err != nil {
		if errors.Is(err, executor.ErrExecutionInProgress) {
			http.Error(w, "cannot clear a transaction that is in progress", http.StatusConflict)
		} else {
			s.fail(w, "failed to clear transaction", err)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
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
		// POST /installed/{filename.deb} -> Stage an install operation
		if arg == "" {
			http.Error(w, "Filename required", http.StatusBadRequest)
			return
		}
		if filepath.Base(arg) != arg {
			http.Error(w, "Invalid filename", http.StatusBadRequest)
			return
		}
		s.stageInstall(w, r, arg)
	case http.MethodDelete:
		if arg == "" {
			// DELETE /installed/ -> Stage a purge of all packages
			s.stagePurgeAll(w, r)
		} else {
			// DELETE /installed/{filename.deb} -> Stage a remove operation
			s.stageRemove(w, r, arg)
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) stageInstall(w http.ResponseWriter, r *http.Request, poolFilename string) {
	sourcePath := filepath.Join(s.cfg.PoolDir, poolFilename)
	if _, err := os.Stat(sourcePath); err != nil {
		http.Error(w, "File not found in pool", http.StatusNotFound)
		return
	}

	err := s.executorStore.Update(func(ops *executor.Operations) error {
		ops.Install(sourcePath)
		return nil
	})

	if err != nil {
		if errors.Is(err, executor.ErrExecutionInProgress) {
			http.Error(w, "Transaction in progress, cannot stage new operations", http.StatusConflict)
		} else {
			s.fail(w, "failed to stage install operation", err)
		}
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) stageRemove(w http.ResponseWriter, r *http.Request, installedFilename string) {
	installedPath := filepath.Join(s.cfg.InstalledDir, installedFilename)
	if _, err := os.Stat(installedPath); err != nil {
		http.Error(w, "File not found in installed", http.StatusNotFound)
		return
	}

	pkgName, err := s.getPackageName(installedPath)
	if err != nil {
		s.fail(w, "failed to read package info", err)
		return
	}

	if pkgName == s.cfg.SelfPackageName {
		http.Error(w, "Cannot stage removal of groom agent itself via API", http.StatusForbidden)
		return
	}

	err = s.executorStore.Update(func(ops *executor.Operations) error {
		ops.Remove(pkgName)
		return nil
	})

	if err != nil {
		if errors.Is(err, executor.ErrExecutionInProgress) {
			http.Error(w, "Transaction in progress, cannot stage new operations", http.StatusConflict)
		} else {
			s.fail(w, "failed to stage remove operation", err)
		}
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) stagePurgeAll(w http.ResponseWriter, r *http.Request) {
	installedFiles, err := s.listInstalledOp()
	if err != nil {
		s.fail(w, "failed to list installed packages", err)
		return
	}

	var packagesToRemove []string
	for _, file := range installedFiles {
		fullPath := filepath.Join(s.cfg.InstalledDir, file)
		pkgName, err := s.getPackageName(fullPath)
		if err != nil {
			log.Printf("Skipping unreadable file %s during purge staging", file)
			continue
		}
		if pkgName == s.cfg.SelfPackageName {
			continue
		}
		packagesToRemove = append(packagesToRemove, pkgName)
	}

	if len(packagesToRemove) == 0 {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "No packages to purge.")
		return
	}

	err = s.executorStore.Update(func(ops *executor.Operations) error {
		for _, pkgName := range packagesToRemove {
			ops.Remove(pkgName)
		}
		return nil
	})

	if err != nil {
		if errors.Is(err, executor.ErrExecutionInProgress) {
			http.Error(w, "Transaction in progress, cannot stage new operations", http.StatusConflict)
		} else {
			s.fail(w, "failed to stage purge operation", err)
		}
		return
	}

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "Staged removal of %d packages", len(packagesToRemove))
}

func (s *Server) fail(w http.ResponseWriter, msg string, err error) {
	log.Printf("❌ %s: %v", msg, err)
	http.Error(w, msg, http.StatusInternalServerError)
}
