package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// State represents the state of operations.
type State string

const (
	// StatePrepare means the operations are being built.
	StatePrepare State = "Prepare"
	// StateRun means the executor has taken control.
	StateRun State = "Run"
	// StateDone means the operations are complete.
	StateDone State = "Done"
	// StateBroken means the operations failed and rollback also failed. The value is "Failed" for historical reasons.
	StateBroken State = "Failed"
)

const (
	// lockPollInterval is the duration between attempts to acquire a file lock.
	lockPollInterval = 100 * time.Millisecond
)

// ErrExecutionInProgress is returned when a modification is attempted on operations that are not in the Prepare state.
var ErrExecutionInProgress = errors.New("operations are in progress and cannot be modified")

// Operations represents a set of operations to be performed atomically.
type Operations struct {
	state   State
	install []string
	remove  []string
	err     error // To record failure reason
}

func (t *Operations) Install(packageFile string) {
	for _, p := range t.install {
		if p == packageFile {
			return // Already in the list
		}
	}
	t.install = append(t.install, packageFile)
}

func (t *Operations) Remove(packageName string) {
	for _, p := range t.remove {
		if p == packageName {
			return // Already in the list
		}
	}
	t.remove = append(t.remove, packageName)
}

// State returns the operations's current state.
func (t *Operations) State() State {
	return t.state
}

// PackagesToInstall returns the list of package files to install.
func (t *Operations) PackagesToInstall() []string {
	return t.install
}

// PackagesToRemove returns the list of package names to remove.
func (t *Operations) PackagesToRemove() []string {
	return t.remove
}

// InProgress returns true if the operations are in the Run state.
func (t *Operations) InProgress() bool {
	return t.state == StateRun
}

// Err returns the last execution error.
// It returns an error if the state is Broken, or if an error is set from a previous run.
func (o *Operations) Err() error {
	if o.err == nil && o.state == StateBroken {
		return fmt.Errorf("operations finished in state %s with no specific error", o.state)
	}
	return o.err
}

// ConsumerStore provides a safe API for the Groom daemon to interact with the
// operations file. Its methods use short-lived locks and fail if the
// operations are in progress.
type ConsumerStore struct {
	*store
}

// NewConsumerStore creates a new store for the daemon.
func NewConsumerStore(baseDir string) (*ConsumerStore, error) {
	s, err := newStore(baseDir)
	if err != nil {
		return nil, err
	}
	return &ConsumerStore{s}, nil
}

// Update acquires a short-lived lock to safely modify the operations
// plan. It will fail if the operations are not in a Prepare state or if the
// executor is currently running.
func (ds *ConsumerStore) Update(modify func(ops *Operations) error) error {
	locked, err := ds.tryLock()
	if err != nil {
		return fmt.Errorf("failed to check operations lock: %w", err)
	}
	if !locked {
		return ErrExecutionInProgress
	}
	defer ds.unlock()
	ops, err := ds.Operations()
	if err != nil {
		if os.IsNotExist(err) {
			ops = &Operations{state: StatePrepare}
		} else {
			return fmt.Errorf("failed to load existing operations: %w", err)
		}
	}
	if ops.State() != StatePrepare {
		return ErrExecutionInProgress
	}
	if err := modify(ops); err != nil {
		return fmt.Errorf("modification callback failed: %w", err)
	}
	return ds.persist(ops)
}

// ExecutorStore provides an API for the Groom executor process to take exclusive
// control of operations and modify its state during execution.
type ExecutorStore struct {
	*store
}

// NewExecutorStore creates a new executor instance.
func NewExecutorStore(baseDir string) (*ExecutorStore, error) {
	s, err := newStore(baseDir)
	if err != nil {
		return nil, err
	}
	return &ExecutorStore{s}, nil
}

// Done sets the operations state to Done.
// It must be called while holding the operations lock.
func (e *ExecutorStore) Done() error {
	_, err := e.updateState(StateDone, nil)
	return err
}

// RolledBack sets the operations state to Prepare and records the error that
// caused the rollback. The provided error must not be nil.
// It must be called while holding the operations lock.
func (e *ExecutorStore) RolledBack(err error) error {
	if err == nil {
		err = errors.New("RolledBack with no error")
	}
	_, err = e.updateState(StatePrepare, err)
	return err
}

// Broken sets the operations state to Broken and records the error.
// The provided error must not be nil.
// It must be called while holding the operations lock.
func (e *ExecutorStore) Broken(err error) (*Operations, error) {
	if err == nil {
		return nil, errors.New("Broken requires a non-nil error")
	}
	return e.updateState(StateBroken, err)
}

// store handles the persistence and lifecycle of operations on disk.
// This type is unexported and provides the core, unsafe primitives.
type store struct {
	stateFile string
	lockFile  string
	fileLock  *flock.Flock
}

// newStore creates a new operations store.
// It ensures the base directory exists.
func newStore(baseDir string) (*store, error) {
	if err := os.MkdirAll(baseDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create executor directory %s: %w", baseDir, err)
	}

	s := &store{
		stateFile: filepath.Join(baseDir, "operations.json"),
		lockFile:  filepath.Join(baseDir, "operations.lock"),
	}
	s.fileLock = flock.New(s.lockFile)

	return s, nil
}

// Operations loads the current operations from disk.
// Returns os.ErrNotExist if the operations file does not exist.
func (s *store) Operations() (*Operations, error) {
	// serializable is an embedded struct for persistence, decoupling storage from the public API.
	type serializableOperations struct {
		State             State    `json:"state"`
		PackagesToInstall []string `json:"packages_to_install,omitempty"`
		PackagesToRemove  []string `json:"packages_to_remove,omitempty"`
		Error             string   `json:"error,omitempty"`
	}

	data, err := os.ReadFile(s.stateFile)
	if err != nil {
		return nil, err // os.ErrNotExist is passed through
	}

	var sTx serializableOperations
	if err := json.Unmarshal(data, &sTx); err != nil {
		return nil, fmt.Errorf("failed to unmarshal operations file %s: %w", s.stateFile, err)
	}

	var txErr error
	if sTx.Error != "" {
		txErr = errors.New(sTx.Error)
	}

	tx := &Operations{
		state:   sTx.State,
		install: sTx.PackagesToInstall,
		remove:  sTx.PackagesToRemove,
		err:     txErr,
	}

	// Ensure slices are not nil if they were omitted from JSON
	if tx.install == nil {
		tx.install = make([]string, 0)
	}
	if tx.remove == nil {
		tx.remove = make([]string, 0)
	}

	return tx, nil
}

// persist atomically saves the operations to disk using a write-to-temp-and-rename strategy.
func (s *store) persist(ops *Operations) error {
	// serializable is an embedded struct for persistence, decoupling storage from the public API.
	type serializableOperations struct {
		State             State    `json:"state"`
		PackagesToInstall []string `json:"packages_to_install,omitempty"`
		PackagesToRemove  []string `json:"packages_to_remove,omitempty"`
		Error             string   `json:"error,omitempty"`
	}
	var sErr = ""
	if ops.err != nil {
		sErr = ops.err.Error()
	}

	sTx := serializableOperations{
		State:             ops.state,
		PackagesToInstall: ops.install,
		PackagesToRemove:  ops.remove,
		Error:             sErr,
	}

	data, err := json.MarshalIndent(sTx, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal operations: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(s.stateFile), "operations-*.json.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file for operations: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write to temp operations file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp operations file: %w", err)
	}

	return os.Rename(tmpFile.Name(), s.stateFile)
}

// lock acquires an exclusive, blocking lock on behalf of the executor.
// It respects the provided context for cancellation by polling.
func (s *store) lock(ctx context.Context) error {
	ticker := time.NewTicker(lockPollInterval)
	defer ticker.Stop()

	for {
		// Check for context cancellation before trying to lock.
		select {
		case <-ctx.Done():
			return fmt.Errorf("failed to acquire operations lock: %w", ctx.Err())
		default:
		}

		// Try to acquire the lock non-blockingly.
		locked, err := s.tryLock()
		if err != nil {
			return fmt.Errorf("failed to try-lock operations: %w", err)
		}
		if locked {
			return nil // Success
		}

		// Wait for the next poll interval or for the context to be cancelled.
		select {
		case <-ctx.Done():
			return fmt.Errorf("failed to acquire operations lock: %w", ctx.Err())
		case <-ticker.C:
			// Continue to next loop iteration.
		}
	}
}

// tryLock attempts to acquire a non-blocking lock on behalf of the daemon.
func (s *store) tryLock() (bool, error) {
	locked, err := s.fileLock.TryLock()
	if err != nil {
		return false, fmt.Errorf("failed to try-lock operations: %w", err)
	}
	return locked, nil
}

// unlock releases the file lock.
func (s *store) unlock() error {
	return s.fileLock.Unlock()
}

// updateState is a convenience method for the executor to atomically update the operations state on disk.
// It must be called while holding the operations lock.
func (s *store) updateState(newState State, errInfo error) (*Operations, error) {
	if !s.fileLock.Locked() {
		return nil, errors.New("updateState must be called while holding the operations lock")
	}

	ops, err := s.Operations()
	if err != nil {
		return nil, fmt.Errorf("failed to load operations for state update: %w", err)
	}

	ops.state, ops.err = newState, errInfo

	return ops, s.persist(ops)
}
