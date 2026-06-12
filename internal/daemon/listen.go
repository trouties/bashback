package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/lockfile"
	"github.com/trouties/bashback/internal/paths"
)

// ErrAlreadyRunning means a live daemon already owns the session socket.
var ErrAlreadyRunning = errors.New("daemon already running for session")

// SocketAlive reports whether a unix socket currently has a listener. Exposed
// for the CLI's daemon-status checks.
func SocketAlive(path string) bool { return socketAlive(path) }

// Listen binds the session's unix socket, refusing if a live daemon already
// holds it and clearing a stale socket file otherwise.
func Listen(layout paths.Layout, sessionID string) (net.Listener, error) {
	if err := os.MkdirAll(layout.RunDir(), 0o700); err != nil {
		return nil, err
	}
	path := layout.SocketPath(sessionID)
	// Serialize the alive→remove→bind sequence behind a flock: two daemons racing
	// to claim one session must not both see the socket as free and bind in turn.
	// The lock only guards the critical section; once net.Listen binds, a later
	// caller's socketAlive check reports the live listener, so the lock is released
	// as soon as the sequence completes.
	lock, err := lockfile.Acquire(path+".bindlock", time.Second)
	if err != nil {
		return nil, ErrAlreadyRunning
	}
	defer func() { _ = lock.Release() }()
	if socketAlive(path) {
		return nil, ErrAlreadyRunning
	}
	// Stale socket from a crashed daemon: remove before rebinding.
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return net.Listen("unix", path)
}

// CleanStaleSockets removes dead socket files under run/ (no live listener).
func CleanStaleSockets(layout paths.Layout) {
	ents, err := os.ReadDir(layout.RunDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		p := filepath.Join(layout.RunDir(), e.Name())
		if !socketAlive(p) {
			_ = os.Remove(p)
		}
	}
}
