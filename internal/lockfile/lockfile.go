//go:build unix

// Package lockfile is the flock-based exclusive lock guarding the degraded
// direct-write path: when the daemon is unreachable, clients serialize their own
// git access on the shadow repo so concurrent commands don't race index.lock.
package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrTimeout is returned when the lock can't be acquired within the budget.
var ErrTimeout = errors.New("lockfile: timed out acquiring lock")

// Lock holds an exclusive advisory lock on a file descriptor.
type Lock struct {
	f *os.File
}

// Acquire blocks (polling) until it holds an exclusive lock on path or the
// timeout elapses. The lock file is created if absent.
func Acquire(path string, timeout time.Duration) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrTimeout
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Release unlocks and closes the underlying file.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
