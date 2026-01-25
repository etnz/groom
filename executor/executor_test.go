package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTest(t *testing.T) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "groom-operations-test")
	require.NoError(t, err)
	return dir, func() { os.RemoveAll(dir) }
}

func TestNewConsumerStore_CreatesDirectory(t *testing.T) {
	dir, cleanup := setupTest(t)
	defer cleanup()

	daemonStoreDir := filepath.Join(dir, "new-dir")
	_, err := NewConsumerStore(daemonStoreDir)
	require.NoError(t, err)
	_, err = os.Stat(daemonStoreDir)
	assert.False(t, os.IsNotExist(err), "NewConsumerStore should create the base directory if it doesn't exist")
}

func TestOperationsDurability(t *testing.T) {
	dir, cleanup := setupTest(t)
	defer cleanup()

	// Process 1: Create and save a operations
	daemonStore1, err := NewConsumerStore(dir)
	require.NoError(t, err)

	err = daemonStore1.Update(func(ops *Operations) error {
		ops.Install("/path/to/deb")
		ops.Remove("old-package")
		return nil
	})
	require.NoError(t, err)

	// Process 2: Create a new store and load the operations
	daemonStore2, err := NewConsumerStore(dir)
	require.NoError(t, err)

	ops2, err := daemonStore2.Operations()
	require.NoError(t, err)

	// Verify data is the same
	assert.Equal(t, StatePrepare, ops2.State())
	assert.Equal(t, []string{"/path/to/deb"}, ops2.PackagesToInstall())
	assert.Equal(t, []string{"old-package"}, ops2.PackagesToRemove())
}

func TestUpdateIsAtomic(t *testing.T) {
	dir, cleanup := setupTest(t)
	defer cleanup()

	daemonStore, err := NewConsumerStore(dir)
	require.NoError(t, err)

	err = daemonStore.Update(func(ops *Operations) error {
		return nil
	})
	require.NoError(t, err)

	_, err = daemonStore.Operations()
	assert.NoError(t, err)

	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, f := range files {
		assert.NotContains(t, f.Name(), ".tmp", "no temporary files should remain after a successful save")
	}
}

func TestLockingRobustness(t *testing.T) {
	dir, cleanup := setupTest(t)
	defer cleanup()

	// Simulate an executor and a daemon competing for the lock.
	executor, err := NewExecutorStore(dir)
	require.NoError(t, err)
	daemonStore, err := NewConsumerStore(dir)
	require.NoError(t, err)

	// Goroutine 1 (Executor) acquires the lock
	ctx := context.Background()
	err = executor.lock(ctx)
	require.NoError(t, err)
	t.Log("Goroutine 1 acquired lock")

	var wg sync.WaitGroup
	wg.Add(1)

	// Goroutine 2 (Daemon) tries to stage a modification and should fail
	go func() {
		defer wg.Done()
		t.Log("Goroutine 2 attempting to stage modifications...")
		err := daemonStore.Update(func(ops *Operations) error { return nil })
		require.ErrorIs(t, err, ErrExecutionInProgress, "Staging should fail when executor holds the lock")
		t.Log("Goroutine 2 failed to stage, as expected")
	}()

	wg.Wait()

	// Goroutine 1 releases the lock
	err = executor.unlock()
	require.NoError(t, err)
	t.Log("Goroutine 1 released lock")

	// Now, Goroutine 2 should be able to stage a modification
	err = daemonStore.Update(func(ops *Operations) error { return nil })
	require.NoError(t, err, "Staging should succeed after lock is released")
	t.Log("Goroutine 2 staged successfully after release")
}

func TestFSM_SuccessPath(t *testing.T) {
	dir, cleanup := setupTest(t)
	defer cleanup()

	daemonStore, err := NewConsumerStore(dir)
	require.NoError(t, err)
	// Use a separate executor instance for the same directory
	executor, err := NewExecutorStore(dir)
	require.NoError(t, err)

	// 1. Daemon stages operations
	err = daemonStore.Update(func(ops *Operations) error {
		return nil
	})
	require.NoError(t, err)

	// 2. Executor locks and transitions to Run
	err = executor.lock(context.Background())
	require.NoError(t, err)
	defer executor.unlock()

	_, err = executor.updateState(StateRun, nil)
	require.NoError(t, err)

	loadedOps, err := executor.Operations()
	require.NoError(t, err)
	assert.Equal(t, StateRun, loadedOps.State())

	// 3. Executor finishes and transitions to Done
	err = executor.Done()
	require.NoError(t, err)

	loadedOps, err = executor.Operations()
	require.NoError(t, err)
	assert.Equal(t, StateDone, loadedOps.State())
}

func TestFSM_FailurePaths(t *testing.T) {
	testCases := []struct {
		name          string
		endState      State
		failureReason error
	}{
		{"RolledBack", StatePrepare, fmt.Errorf("apt failed")},
		{"Broken", StateDone, fmt.Errorf("rollback also failed")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dir, cleanup := setupTest(t)
			defer cleanup()

			daemonStore, err := NewConsumerStore(dir)
			require.NoError(t, err)
			executor, err := NewExecutorStore(dir)
			require.NoError(t, err)

			err = daemonStore.Update(func(ops *Operations) error {
				return nil
			})
			require.NoError(t, err)

			err = executor.lock(context.Background())
			require.NoError(t, err)
			defer executor.unlock()

			_, err = executor.updateState(StateRun, nil)
			require.NoError(t, err)

			if tc.endState == StatePrepare { // RolledBack case
				err = executor.RolledBack(tc.failureReason)
			} else { // Broken case
				_, err = executor.Broken(tc.failureReason)
			}
			require.NoError(t, err)

			loadedOps, err := executor.Operations()
			require.NoError(t, err)
			assert.Equal(t, tc.endState, loadedOps.State())
			opsErr := loadedOps.Err()
			require.Error(t, opsErr)
			assert.Equal(t, tc.failureReason.Error(), opsErr.Error())
		})
	}
}

func TestConsumerStore_Update(t *testing.T) {
	dir, cleanup := setupTest(t)
	defer cleanup()

	daemonStore, err := NewConsumerStore(dir)
	require.NoError(t, err)

	t.Run("creates new operations file if none exists", func(t *testing.T) {
		err := daemonStore.Update(func(ops *Operations) error {
			ops.Install("new-pkg.deb")
			return nil
		})
		require.NoError(t, err)

		loaded, err := daemonStore.Operations()
		require.NoError(t, err)
		assert.Equal(t, StatePrepare, loaded.State())
		assert.Contains(t, loaded.PackagesToInstall(), "new-pkg.deb")
	})

	t.Run("fails if operations are not in prepare state", func(t *testing.T) {
		// Manually create a "Run" operations file
		startedOps := &Operations{}
		startedOps.state = StateRun
		err := daemonStore.persist(startedOps)
		require.NoError(t, err)

		err = daemonStore.Update(func(ops *Operations) error {
			ops.Install("another-pkg.deb")
			return nil
		})
		require.ErrorIs(t, err, ErrExecutionInProgress)
	})

	t.Run("fails if lock is held by executor", func(t *testing.T) {
		executor, err := NewExecutorStore(dir)
		require.NoError(t, err)

		require.NoError(t, executor.lock(context.Background()))
		defer executor.unlock()

		err = daemonStore.Update(func(ops *Operations) error { return nil })
		require.ErrorIs(t, err, ErrExecutionInProgress)
	})
}

func TestAddPackage_IsIdempotent(t *testing.T) {
	ops := &Operations{}

	ops.Install("/path/to/package.deb")
	ops.Install("/path/to/package.deb")
	assert.Len(t, ops.PackagesToInstall(), 1)

	ops.Remove("old-package")
	ops.Remove("old-package")
	assert.Len(t, ops.PackagesToRemove(), 1)
}

func TestExecutorStore_Methods_FailsWithoutLock(t *testing.T) {
	dir, cleanup := setupTest(t)
	defer cleanup()

	daemonStore, err := NewConsumerStore(dir)
	require.NoError(t, err)
	executor, err := NewExecutorStore(dir)
	require.NoError(t, err)

	// Stage an operation so the file exists
	err = daemonStore.Update(func(ops *Operations) error {
		return nil
	})
	require.NoError(t, err)

	// Attempt to update state without holding a lock
	err = executor.Done()
	require.Error(t, err, "Done() should fail without a lock")
	assert.Contains(t, err.Error(), "must be called while holding the operations lock")

	err = executor.RolledBack(fmt.Errorf("test"))
	require.Error(t, err, "RolledBack() should fail without a lock")
	assert.Contains(t, err.Error(), "must be called while holding the operations lock")
}
