package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// acquireLock opens lockPath and tries a non-blocking exclusive flock.
// Returns the *os.File (caller must call releaseLock on it), and ok=true
// if the lock was acquired. ok=false means another process holds the lock.
// Errors other than "already held" (missing dir, permission denied, etc.)
// are returned with ok=false.
//
// Scribe uses advisory locks so concurrent cron jobs (sync, dream, capture)
// and manual invocations can serialize without trampling on the git repo
// and state files.
func acquireLock(lockPath string) (f *os.File, ok bool, err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, false, err
	}
	f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

// releaseLock releases the advisory lock and closes the file. Safe to defer.
func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// lockPathFor returns the canonical path for scribe's per-command lock file.
// Keep callers aligned — commit.go inspects these same paths to decide
// whether another scribe process is active.
func lockPathFor(lockDir, name string) string {
	return filepath.Join(lockDir, "scribe-"+name+".lock")
}
